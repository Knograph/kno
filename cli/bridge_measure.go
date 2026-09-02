package cli

import (
	"context"
	"fmt"
	"io"
	"iter"
	"net/url"
	"sort"
	"strings"

	"github.com/knograph/kno/adapters/agent/openaicompat"
	"github.com/knograph/kno/adapters/agent/pricing"
	openaituner "github.com/knograph/kno/adapters/tuner/openai"
	"github.com/knograph/kno/adapters/tuner/together"
	"github.com/knograph/kno/bridge"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
	"google.golang.org/protobuf/proto"
)

// This file is the eval seam's CLI choke point: --evals resolved and
// sealed exactly once (docs/plans/2026-09-01-bridge-eval-seam.md §9),
// the pre-flight completeness refusal (edge case 1), and the production
// core.Tuner and bridge.AgentFactory this build was missing entirely —
// #184 shipped bridge.Run's whole submit/poll/deploy/teardown spine
// without ever constructing a real Tuner, because confirmAndStop always
// refused first. This is what wires it live.

// confirmAndRun runs the SAME budget-confirmation machinery every other
// spend path in Kno uses (stats/budget.Guard, cli's confirmFunc) against
// the bridge's total quote, so an armed-but-unconfirmed run declines
// through the identical errs.ErrBudgetExceeded path a Case-level spend
// would. Once confirmed, it resolves --evals, builds the production
// EvalRunner, and calls bridge.Run for real.
func confirmAndRun(
	ctx context.Context, _ io.Reader, out io.Writer, f bridgeFlags,
	db store.Store, plan *value.Plan, groups *bridge.GroupsPlan,
	scheme, model string, quotes []bridge.GroupQuote, servePrice pricing.ServePrice, hostingCapUSDMicros int64,
	evalPrice *knov1.Price, evalCapUSDMicros int64, evalCalls int,
) error {
	// Three addends now, not two: training, the hosting cap, and the eval
	// pass's own worst case — docs/plans/2026-09-02-openai-tuner.md §4.
	// evalCapUSDMicros is zero whenever evalPrice is nil (Together today),
	// so this sum is byte-for-byte what it was before for every scheme
	// pricing.LookupFineTunedPrice carries no row for.
	total := bridge.TotalEstimatedCostUSDMicros(quotes) + hostingCapUSDMicros + evalCapUSDMicros
	var totalTokens int64
	for _, q := range quotes {
		totalTokens += q.TrainTokens
	}

	recorder := &consentRecorder{}
	guard := budget.New(
		budget.Limits{MaxCostUSDMicros: usdToMicros(f.maxCostUSD)},
		confirmFunc(out, f.yes, f.jsonOut, recorder),
		usdToMicros(confirmThresholdUSD),
	)

	res, err := guard.Authorize(ctx, budget.Estimate{
		Calls:         int64(len(quotes) + evalCalls),
		CostUSDMicros: total,
		Tokens:        totalTokens,
	})
	if err != nil {
		// The SAME refusal a declined Case-level spend produces:
		// errs.ErrBudgetExceeded, exit 2, resumable in spirit (nothing was
		// spent). Nothing was written, nothing was submitted.
		return err
	}
	// This reservation covers training, the hosting cap, and the eval
	// pass's worst case. Release rather than Settle: the guard bridge.Run
	// receives below re-authorizes every real spend itself (training,
	// hosting ticks, and — when evalPrice is non-nil — each eval Case
	// through core.ScorePass's own Estimator path), so double-counting
	// this planning-time reservation would overstate the first real
	// charge.
	res.Release()

	// --evals is required only once armed: the un-armed plan needs no
	// Case content, only Asset content (rendering the training files).
	if f.evalsPath == "" {
		return errs.ErrInvalidInput.WithFix(
			"pass --evals, naming the eval source the value.Plan's Case IDs came from " +
				"(the same one --holdout-frac/--split-seed used for kno baseline/kno value)",
		).
			Wrap(fmt.Errorf("bridge: --bridge is armed but --evals names no eval source; " +
				"measuring a deployed model needs Case content, not only Asset content"))
	}

	evalSrc, err := resolveEvals(evalsFlags{
		path: f.evalsPath, holdoutFrac: f.holdoutFrac, splitSeed: f.splitSeed,
		allowInsecureURL: f.allowInsecureURL, allowPrivateAddress: f.allowPrivateAddress,
	})
	if err != nil {
		return err
	}
	// Sealed HERE, once, for the whole bridge run — the plan's §9 choke
	// point. ScoreEvalRunner and every group's Measure call read Cases
	// through this value and nothing else; core/seal.go's *SealedEvals is
	// what makes "forgot to seal" a compile error rather than a holdout
	// leak discovered later.
	sealed := core.Seal(evalSrc)

	devCaseIDs := bridge.DevCaseIDsForGroups(plan, groups)
	wanted := wantedCaseIDs(devCaseIDs, plan.ControlCaseIDs)
	cases, missing, err := resolveWantedCases(ctx, sealed, wanted)
	if err != nil {
		return fmt.Errorf("reading --evals: %w", err)
	}
	if len(missing) > 0 {
		// Edge case 1: "A Case ID in the plan with no Case in --evals.
		// Refuse before any spend, naming the Case." Also covers edge
		// case 4 (zero overlap): when every wanted ID is missing, this
		// refusal fires with the whole set named.
		sort.Strings(missing)
		return errs.ErrInvalidInput.WithFix(
			"pass --evals naming the SAME eval source (and --holdout-frac/--split-seed) " +
				"kno value used, so its dev Case IDs resolve to real Cases",
		).
			Wrap(fmt.Errorf("bridge: %d Case ID(s) the value.Plan names have no Case in --evals, "+
				"including %v; nothing was submitted", len(missing), firstN(missing, 5)))
	}

	goal, err := resolveGoal(f.goalName)
	if err != nil {
		return err
	}

	tuner, err := newBridgeTuner(f, scheme)
	if err != nil {
		return err
	}
	agentFactory, err := bridgeAgentFactory(f, scheme, evalPrice)
	if err != nil {
		return err
	}

	emitter, err := bridge.NewEmitter(ctx, db, f.selectRunID+"-bridge")
	if err != nil {
		return err
	}

	runID := f.selectRunID + "-bridge"
	if err := db.CreateRun(ctx, &knov1.Run{
		Id: runID, Stage: knov1.Stage_STAGE_BRIDGE, Status: knov1.RunStatus_RUN_STATUS_RUNNING,
	}); err != nil {
		return fmt.Errorf("opening the bridge run: %w", err)
	}

	runGuard := budget.New(budget.Limits{MaxCostUSDMicros: usdToMicros(f.maxCostUSD)}, nil, 0)

	result, runErr := bridge.Run(ctx, bridge.RunParams{
		RunID: runID, Store: db, Guard: runGuard, Tuner: tuner, Emitter: emitter, Provider: scheme,
		Quotes:         quotes,
		BaseModel:      &knov1.AgentRef{Ref: f.tuner, Scheme: scheme, Target: model},
		Epochs:         f.epochs,
		GoalDomain:     goal.Domain(),
		Level:          0.95,
		NGroups:        len(groups.LeaveOneOut),
		DevCaseIDs:     devCaseIDs,
		ControlCaseIDs: plan.ControlCaseIDs,
		Eval: &bridge.ScoreEvalRunner{
			Cases: core.Seal(staticEvals{cases: cases}),
			Goal:  goal, Guard: runGuard, NewAgent: agentFactory,
			AcceptFreeCalls: acceptFreeCalls(evalPrice, servePrice),
		},
		ServePrice:       servePrice,
		MaxLiveEndpoints: f.maxLiveEndpoints,
		MaxServeMinutes:  f.maxServeMinutes,
		JobTimeout:       f.bridgeTimeout,
	})
	if runErr != nil {
		return runErr
	}
	return renderBridgeResult(out, f, result)
}

func firstN(ss []string, n int) []string {
	if len(ss) <= n {
		return ss
	}
	return ss[:n]
}

// wantedCaseIDs is the union of every leave-one-out group's dev Case IDs
// plus the reserved control partition — every Case ID any group's Measure
// call could ask for, computed once so the pre-flight refusal and the
// production runner's Cases source agree on exactly the same set.
func wantedCaseIDs(devCaseIDs map[string][]string, controlCaseIDs []string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, ids := range devCaseIDs {
		for _, id := range ids {
			set[id] = struct{}{}
		}
	}
	for _, id := range controlCaseIDs {
		set[id] = struct{}{}
	}
	return set
}

// resolveWantedCases reads sealed's dev Cases ONCE and returns the subset
// named by want, plus every wanted ID that was never seen. Evals.Cases
// yields BORROWED values (see core.Evals's doc); every retained Case is
// cloned before being kept.
func resolveWantedCases(ctx context.Context, sealed *core.SealedEvals, want map[string]struct{}) ([]*core.Case, []string, error) {
	seq, err := sealed.Cases(ctx)
	if err != nil {
		return nil, nil, err
	}
	found := make(map[string]*core.Case, len(want))
	for c, err := range seq {
		if err != nil {
			return nil, nil, err
		}
		if _, ok := want[c.GetId()]; !ok {
			continue
		}
		found[c.GetId()] = proto.Clone(c).(*core.Case)
	}
	var missing []string
	for id := range want {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	cases := make([]*core.Case, 0, len(found))
	for _, c := range found {
		cases = append(cases, c)
	}
	return cases, missing, nil
}

// staticEvals is a small in-memory core.Evals over Cases already resolved
// and filtered by the CLI — what ScoreEvalRunner reads from, rather than
// re-querying the original --evals source (a remote dataset) once per
// group's Measure call.
type staticEvals struct{ cases []*core.Case }

func (s staticEvals) Cases(context.Context) (iter.Seq2[*core.Case, error], error) {
	return func(yield func(*core.Case, error) bool) {
		for _, c := range s.cases {
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}

// tunerFactory constructs the core.Tuner one --tuner scheme needs.
type tunerFactory func(f bridgeFlags) (core.Tuner, error)

// evalAgentFactoryBuilder constructs the bridge.AgentFactory that invokes
// one scheme's deployed model over its eval passes, given the eval-pass
// price resolveEvalPrice already resolved for --tuner's base model
// (nil when this scheme/model has no per-token rate).
type evalAgentFactoryBuilder func(f bridgeFlags, evalPrice *knov1.Price) (bridge.AgentFactory, error)

// bridgeAdapter bundles what one --tuner scheme needs: a Tuner constructor
// and the AgentFactory builder for its deployed model's eval passes.
//
// This is docs/debt.md#161's repayment: newBridgeTuner and
// bridgeAgentFactory used to switch on scheme separately, so adding a
// second Tuner meant adding an arm to TWO switches (and could add one to
// only one of them, since nothing forced the pair to move together). A
// second adapter is now ONE map entry carrying both halves.
type bridgeAdapter struct {
	tuner tunerFactory
	agent evalAgentFactoryBuilder
}

// bridgeAdapters is the scheme-keyed registry — docs/debt.md#161's
// repayment: a second Tuner adapter is a map entry, not a branch. "openai"
// (adapters/tuner/openai) lands here as PR 2 of
// docs/plans/2026-09-02-openai-tuner.md; "together" shipped first per the
// tuner-bridge plan's Step 5 sequencing.
var bridgeAdapters = map[string]bridgeAdapter{
	"together": {tuner: newTogetherTuner, agent: newTogetherAgentFactory},
	"openai":   {tuner: newOpenAITuner, agent: newOpenAIAgentFactory},
}

// supportedTunerSchemes lists what --tuner accepts, sorted, for a refusal
// message that names what IS known rather than only what is not —
// pricing.Models' own convention, applied here.
func supportedTunerSchemes() string {
	schemes := make([]string, 0, len(bridgeAdapters))
	for s := range bridgeAdapters {
		schemes = append(schemes, s)
	}
	sort.Strings(schemes)
	return strings.Join(schemes, ", ")
}

// newBridgeTuner constructs the core.Tuner --tuner names, by dispatching
// through bridgeAdapters rather than a switch.
func newBridgeTuner(f bridgeFlags, scheme string) (core.Tuner, error) {
	adapter, ok := bridgeAdapters[scheme]
	if !ok {
		return nil, errs.ErrCapabilityUnsupported.
			WithFix(fmt.Sprintf("pass --tuner as one of: %s", supportedTunerSchemes())).
			Wrap(fmt.Errorf("no Tuner adapter for scheme %q", scheme))
	}
	return adapter.tuner(f)
}

// bridgeAgentFactory builds the bridge.AgentFactory for --tuner's
// provider, by dispatching through bridgeAdapters rather than a switch.
// evalPrice is threaded through from resolveEvalPrice (resolved once in
// runBridgeCore) rather than re-derived here from a transport Ref's
// scheme — see resolveEvalPrice's doc for why re-deriving would
// double-count a Together run's hosting ticker.
func bridgeAgentFactory(f bridgeFlags, scheme string, evalPrice *knov1.Price) (bridge.AgentFactory, error) {
	adapter, ok := bridgeAdapters[scheme]
	if !ok {
		return nil, errs.ErrCapabilityUnsupported.
			WithFix(fmt.Sprintf("pass --tuner as one of: %s", supportedTunerSchemes())).
			Wrap(fmt.Errorf("no inference wiring for scheme %q", scheme))
	}
	return adapter.agent(f, evalPrice)
}

// newTogetherTuner is bridgeAdapters["together"].tuner.
func newTogetherTuner(f bridgeFlags) (core.Tuner, error) {
	bindings, err := keyBindings(f.keyEnv)
	if err != nil {
		return nil, err
	}
	return together.New(together.Options{
		KeyEnv:               bindings,
		AllowInsecureBaseURL: f.allowInsecureURL,
		AllowPrivateAddress:  f.allowPrivateAddress,
	})
}

// newTogetherAgentFactory is bridgeAdapters["together"].agent: an Agent
// that invokes the deployed model over its OpenAI-compatible chat
// completions API, per the tuner-bridge plan's Step 5 ("the eval passes go
// through adapters/agent/openaicompat... with no new inference code").
//
// (verify): a Together dedicated endpoint's inference route is asserted
// here as together.DefaultBaseURL + "/v1", the OpenAI-compatible path
// Together's own docs describe for its hosted models generally. This PR
// does not confirm that a DEDICATED endpoint answers on the identical
// path with its served-model name in the "model" field — the same class
// of provider fact the tuner-bridge plan tags (verify) throughout, and
// unconfirmed by this PR against a live endpoint. See this PR's report.
func newTogetherAgentFactory(f bridgeFlags, evalPrice *knov1.Price) (bridge.AgentFactory, error) {
	userBindings, err := keyBindings(f.keyEnv)
	if err != nil {
		return nil, err
	}
	baseURL := together.DefaultBaseURL + "/v1"
	host := bridgeHostOf(baseURL)
	// together.Tuner resolves TOGETHER_API_KEY against its OWN default
	// host automatically; openaicompat's default host is api.openai.com,
	// so pointing it at Together needs an explicit binding. Together's
	// own default is applied here so a user does not have to spell out
	// --key-env twice for one provider; an explicit --key-env for this
	// host still wins (userBindings is applied after).
	bindings := map[string]string{host: together.DefaultKeyEnv}
	for h, v := range userBindings {
		bindings[h] = v
	}

	return func(_ context.Context, model *knov1.AgentRef) (core.Agent, error) {
		ref := &knov1.AgentRef{
			Scheme: "openai", Target: model.GetTarget(), BaseUrl: baseURL,
		}
		return openaicompat.New(openaicompat.Options{
			Ref: ref, KeyEnv: bindings,
			AllowInsecureBaseURL: f.allowInsecureURL,
			AllowPrivateAddress:  f.allowPrivateAddress,
			// evalPrice is nil for Together today (pricing.LookupFineTunedPrice
			// carries no "together" rows), which is what makes
			// ScoreEvalRunner's AcceptFreeCalls true and this Agent's own
			// Estimator path never consulted. A per-run-generated endpoint
			// id is never in the static pricing table regardless — that
			// was true before this change and stays true — but the price
			// itself is now resolved once at the CLI layer (by base model,
			// not by this endpoint's generated name) rather than always
			// nil, so a future per-token scheme registered here gets
			// priced correctly instead of silently asserting free.
			Price: evalPrice,
		})
	}, nil
}

// bridgeHostOf extracts the host from an already-validated absolute URL —
// unlike agentwiring.go's checkAbsoluteHTTP, no refusal path is needed
// here: baseURL is a package constant plus a literal suffix, not user
// input.
func bridgeHostOf(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// newOpenAITuner is bridgeAdapters["openai"].tuner.
func newOpenAITuner(f bridgeFlags) (core.Tuner, error) {
	bindings, err := keyBindings(f.keyEnv)
	if err != nil {
		return nil, err
	}
	return openaituner.New(openaituner.Options{
		KeyEnv:               bindings,
		AllowInsecureBaseURL: f.allowInsecureURL,
		AllowPrivateAddress:  f.allowPrivateAddress,
	})
}

// newOpenAIAgentFactory is bridgeAdapters["openai"].agent: an Agent that
// invokes the deployed model over OpenAI's own chat completions API.
//
// Unlike newTogetherAgentFactory, no special base URL or host binding is
// needed: OpenAI auto-serves a fine-tuned model at its OWN default host
// (openaicompat.DefaultBaseURL) under the fine-tuned model's own name —
// there is no separate "dedicated endpoint" host the way a Together
// deployment has. OPENAI_API_KEY resolves against that default host exactly
// as it does for a non-bridge `kno baseline --agent openai:...` run; an
// explicit --key-env still wins for a self-hosted or proxied base URL.
func newOpenAIAgentFactory(f bridgeFlags, evalPrice *knov1.Price) (bridge.AgentFactory, error) {
	bindings, err := keyBindings(f.keyEnv)
	if err != nil {
		return nil, err
	}
	return func(_ context.Context, model *knov1.AgentRef) (core.Agent, error) {
		ref := &knov1.AgentRef{Scheme: openaituner.Scheme, Target: model.GetTarget()}
		return openaicompat.New(openaicompat.Options{
			Ref: ref, KeyEnv: bindings,
			AllowInsecureBaseURL: f.allowInsecureURL,
			AllowPrivateAddress:  f.allowPrivateAddress,
			// evalPrice is resolveEvalPrice's result for --tuner's base
			// model (cli/bridge.go), resolved once and threaded down —
			// see that function's own doc. fineTunedTable ships empty for
			// "openai" in this PR (deliberately: see adapters/tuner/openai's
			// package doc and docs/debt.md#162's disposition), so evalPrice
			// is nil here until a reviewed diff adds rows through
			// internal/cmd/pricingcheck — which is exactly what makes the
			// confirmAndRun refusal below load-bearing rather than a gap.
			Price: evalPrice,
		})
	}, nil
}

package cli

import (
	"context"
	"fmt"
	"io"
	"iter"
	"net/url"
	"sort"

	"github.com/knograph/kno/adapters/agent/openaicompat"
	"github.com/knograph/kno/adapters/agent/pricing"
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
) error {
	total := bridge.TotalEstimatedCostUSDMicros(quotes) + hostingCapUSDMicros
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
		Calls:         int64(len(quotes)),
		CostUSDMicros: total,
		Tokens:        totalTokens,
	})
	if err != nil {
		// The SAME refusal a declined Case-level spend produces:
		// errs.ErrBudgetExceeded, exit 2, resumable in spirit (nothing was
		// spent). Nothing was written, nothing was submitted.
		return err
	}
	// This reservation covers training and the hosting cap; the eval
	// passes themselves are asserted free (see bridgeAgentFactory) and
	// settle nothing against it. Release rather than Settle: the guard
	// bridge.Run receives below re-authorizes every real spend itself, so
	// double-counting this planning-time reservation would overstate the
	// first real charge.
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
	agentFactory, err := bridgeAgentFactory(f, scheme)
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

// newBridgeTuner constructs the core.Tuner --tuner names. Only "together"
// ships in this build (adapters/tuner/together) — matching the
// tuner-bridge plan's Step 5 sequencing (Together first, Fireworks and
// OpenAI later).
func newBridgeTuner(f bridgeFlags, scheme string) (core.Tuner, error) {
	if scheme != "together" {
		return nil, errs.ErrCapabilityUnsupported.
			WithFix("pass --tuner together:<model>; no other Tuner ships in this build").
			Wrap(fmt.Errorf("no Tuner adapter for scheme %q", scheme))
	}
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

// bridgeAgentFactory builds the bridge.AgentFactory for --tuner's
// provider: an Agent that invokes the deployed model over its
// OpenAI-compatible chat completions API, per the tuner-bridge plan's Step
// 5 ("the eval passes go through adapters/agent/openaicompat... with no
// new inference code").
//
// (verify): a Together dedicated endpoint's inference route is asserted
// here as together.DefaultBaseURL + "/v1", the OpenAI-compatible path
// Together's own docs describe for its hosted models generally. This PR
// does not confirm that a DEDICATED endpoint answers on the identical
// path with its served-model name in the "model" field — the same class
// of provider fact the tuner-bridge plan tags (verify) throughout, and
// unconfirmed by this PR against a live endpoint. See this PR's report.
func bridgeAgentFactory(f bridgeFlags, scheme string) (bridge.AgentFactory, error) {
	if scheme != "together" {
		return nil, errs.ErrCapabilityUnsupported.
			WithFix("pass --tuner together:<model>; no other Tuner ships in this build").
			Wrap(fmt.Errorf("no inference wiring for scheme %q", scheme))
	}
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
			// Price left nil deliberately: ScorePass's AcceptFreeCalls
			// asserts these calls are already paid for by the hosting
			// ticker (bridge/hosting.go). A per-run-generated endpoint id
			// is never in the static pricing table regardless, so an
			// unpriced lookup already means "no rate" — consistent with
			// the assertion rather than fighting it.
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

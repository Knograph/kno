package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
	"google.golang.org/protobuf/proto"
)

// Validation is the holdout slice of a Report: what Validate measured and
// what it is allowed to claim.
type Validation = knov1.Validation

// ValidationVerdict is what a Validate run concluded, keyed on the interval.
type ValidationVerdict = knov1.ValidationVerdict

// defaultValidateTrials is how many times each holdout measurement is
// repeated when the caller names no number. One: the holdout is measured in
// two arms already, and a second trial doubles the bill again for a variance
// reduction the paired interval mostly recovers on its own.
const defaultValidateTrials = 1

// validateArms is the number of arms every holdout Case is measured in.
//
// A named constant because it is the multiplier in the consent quote, and the
// quote understating by exactly this factor is the failure core/ring0.go
// records having already happened once at a different multiple. The quote, the
// schedule assertion and the loop all read this one value.
const validateArms = 2

// ValidateOptions configure the Validate stage.
//
// Shaped like ValueOptions where the two stages agree, because they share a
// Store, a Guard, an executor and an invoker — a field meaning one thing here
// and another there is how a resume check ends up comparing the wrong pair of
// values.
type ValidateOptions struct {
	// RunID identifies this Validate run.
	RunID string

	// SelectRunID is the Select run whose Portfolio is being validated.
	SelectRunID string

	// Agent is the CONTROL arm: the agent with nothing injected. It must also
	// implement ContextSetInjector, which validate refuses without.
	Agent Agent

	// AgentRef identifies the agent, for the Run record and the event stream.
	AgentRef *AgentRef

	// Goal scores every measurement, and its Domain decides the interval
	// method for the one delta this run reports.
	Goal Goal

	// GoalName is the Goal's registered name.
	GoalName string

	// Guard authorizes every measurement before it is made.
	Guard *budget.Guard

	// Evals is the eval source, UNSEALED and deliberately so.
	//
	// A plain Evals, not a *SealedEvals and not a holdout reader: the holdout
	// reader is unexported, so cli, tui and api cannot construct one, and
	// Validate opens the holdout itself through openHoldout. A caller handing
	// over a *SealedEvals is refused with errs.ErrHoldoutSealed rather than
	// iterating one and finding no Cases — see openHoldout.
	Evals Evals

	// Pool supplies the Assets the Portfolio names, so their content can be
	// injected and checked against PortfolioEntry.content_hash.
	Pool Pool

	// Store is where measurements, the holdout-use record and the Validation
	// become durable.
	Store store.Store

	// Concurrency bounds workers within one arm.
	Concurrency int

	// Trials is how many times each (Case, arm) is measured. Zero means once.
	Trials int32

	// Resume continues a run whose measurements are already partly recorded.
	Resume bool

	// ContextOnly validates the DESTINATION_CONTEXT entries of a mixed
	// Portfolio, and labels the result a SUBSET everywhere it appears.
	//
	// Refused by default: a number about the context entries, reported as "the
	// Portfolio's holdout gain", is a number about a different set than the one
	// the user is about to export.
	ContextOnly bool

	// AllowRepeatHoldout permits a SECOND, different Portfolio to be measured
	// against a holdout that has already been used.
	//
	// Counted and disclosed, never corrected for: validating N portfolios
	// against one holdout re-introduces the multiplicity the holdout existed to
	// remove, at rate N. The flag exists because the alternative to a counted
	// peek is an uncounted one — the user deletes kno.db or re-splits with
	// --split-seed, and the tool has traded an honest number for a comfortable
	// rule.
	AllowRepeatHoldout bool

	// MaxContextTokens refuses a Portfolio whose summed carrying cost exceeds
	// it, before any spend. Zero is unset, in which case the Portfolio's own
	// recorded Budget.max_context_tokens applies.
	MaxContextTokens int64

	// MinHoldout is the holdout size below which the number is marked
	// underpowered.
	//
	// Supplied by the caller rather than read from adapters/evals/split, which
	// core cannot import (CLAUDE.md prime directive 3). cli passes
	// split.MinHoldout, and a test pins that, so changing the constant changes
	// this verdict. Zero disables the marking.
	MinHoldout int32

	// EvalFingerprint identifies the HOLDOUT — the eval source's content hash
	// combined with the split configuration that divided it.
	//
	// It is the key of the one-shot record. Two runs sharing it met the same
	// Cases; a run with a different one is looking at a different holdout and
	// is not a repeat.
	EvalFingerprint string

	// InputFingerprint pins the inputs, so a resume cannot silently continue a
	// different run.
	InputFingerprint string

	// EstCostPerCallUSDMicros is the fallback estimate for agents that do not
	// implement Estimator, exactly as Baseline and Value use it.
	EstCostPerCallUSDMicros int64

	// MaxAttempts, RetryBudget and RetryBackoff bound retries per measurement,
	// exactly as they do for Baseline and Value.
	MaxAttempts  int
	RetryBudget  int64
	RetryBackoff int64
}

// ValidateResult is what a Validate run produced.
type ValidateResult struct {
	// RunID identifies the run.
	RunID string

	// Validation is the finished record. Nil when the run produced none —
	// interrupted, budget-stopped, or refused before it started.
	Validation *Validation

	// Status is how the run ended.
	Status knov1.RunStatus

	// GoalDirection is which way is better, so the report can un-negate
	// MINIMIZE deltas for display.
	GoalDirection knov1.Direction

	// NothingToValidate is set when the Portfolio selected no Asset this stage
	// can measure. A first-class outcome, not a failure: no agent call is
	// made, no holdout is consumed, and the exit code is zero.
	NothingToValidate bool

	// HoldoutCases is how many Cases the holdout yielded.
	HoldoutCases int

	// HoldoutUseIndex is which portfolio this was for this holdout: 1 for the
	// first.
	HoldoutUseIndex int32

	// AssetCount is how many Assets were injected as a set.
	AssetCount int

	// Spent is what the run actually cost, settled.
	//
	// The guard's number, not the schema's, for BaselineResult.Spent's reason,
	// and populated on EVERY path returning a non-nil result including the
	// error paths: a run that spent real money and then failed for a reason
	// with nothing to do with money still owes the caller the figure. On a
	// resumed run it is run-lifetime spend, because the run is the unit the
	// user authorized.
	Spent budget.Spend
}

// ValidateQuote is the schedule a Validate run would execute, priced before
// anything is authorized.
type ValidateQuote struct {
	// HoldoutCases is how many Cases the holdout yielded.
	HoldoutCases int

	// Arms is always validateArms. Carried explicitly so the consent line can
	// SHOW the doubling rather than fold it into a total — a quote of
	// n x trials would understate the run by exactly this factor.
	Arms int

	// Trials is the repeat count.
	Trials int32

	// Calls is HoldoutCases x Arms x Trials: the number the user consents to.
	Calls int64

	// AssetCount is how many Assets are injected as a set.
	AssetCount int

	// Underpowered reports a holdout below MinHoldout.
	Underpowered bool

	// ExcludedAssetIDs are the entries --context-only left out.
	ExcludedAssetIDs []string

	// NothingToValidate is set when there is no Asset to measure, in which
	// case Calls is zero and the run makes no agent call at all.
	NothingToValidate bool
}

// Derivation renders the quote's arithmetic for the consent dialog.
//
// The derivation and not just the total, because prime directive 4 is about
// disclosure rather than magnitude: two arms is exactly double the naive
// expectation, and a user reading "40 calls" without "20 holdout Cases x 2
// arms x 1 trial" has been told the number but not the shape of the run.
func (q ValidateQuote) Derivation() string {
	return fmt.Sprintf("%d calls (%d holdout Cases x %d arms x %d trial(s))",
		q.Calls, q.HoldoutCases, q.Arms, q.Trials)
}

// errNoSetInjection is returned when the agent cannot carry a Portfolio.
var errNoSetInjection = errors.New("this agent cannot carry a whole Portfolio in its context")

// validate refuses everything that can be refused before any spend.
//
// Every refusal here is free. The alternative to each is a full-price run
// whose output reads as a result — a gain, an interval, a verdict a deploy
// gate acts on — computed over something other than what the user asked for.
func (o ValidateOptions) validate() error {
	switch {
	case o.RunID == "":
		return errs.ErrInvalidInput.Wrap(errors.New("validate: a run ID is required"))
	case o.SelectRunID == "":
		return errs.ErrInvalidInput.
			WithFix("pass --select-run-id, or run `kno select` first").
			Wrap(errors.New("validate: a Portfolio to validate is required"))
	case o.Agent == nil:
		return errs.ErrInvalidInput.Wrap(errors.New("validate: an agent is required"))
	case o.Goal == nil:
		return errs.ErrInvalidInput.Wrap(errors.New("validate: a goal is required"))
	case o.Store == nil:
		return errs.ErrInvalidInput.Wrap(errors.New("validate: a store is required"))
	case o.Guard == nil:
		return errs.ErrInvalidInput.Wrap(errors.New("validate: a budget guard is required"))
	case o.Evals == nil:
		return errs.ErrInvalidInput.
			WithFix("point --evals at the same eval source the pipeline was measured against").
			Wrap(errors.New("validate: an evals source is required"))
	case o.Pool == nil:
		return errs.ErrInvalidInput.
			WithFix("point --pool at the same Assets the Portfolio was selected from").
			Wrap(errors.New("validate: a pool is required"))
	case o.EvalFingerprint == "":
		return errs.ErrInvalidInput.
			Wrap(errors.New("validate: an eval fingerprint is required; without it the " +
				"holdout has no identity and the one-shot record cannot be kept"))
	}

	// The capability check, before any spend, and it is not a formality.
	//
	// An adapter that cannot carry the set still ANSWERS every request — it
	// answers the Case without the Portfolio. Both arms then measure the same
	// thing, the paired difference is noise around zero, and the run produces
	// a verdict at full price that is indistinguishable from a real finding
	// that the Portfolio does not help.
	if _, ok := o.Agent.(ContextSetInjector); !ok {
		return errs.ErrCapabilityUnsupported.
			WithFix("run `kno doctor` for the capability matrix, then validate against an " +
				"adapter that can carry a whole Portfolio (openai: and anthropic: both can)").
			Wrap(fmt.Errorf("validate: %w", errNoSetInjection))
	}
	if c, ok := o.Agent.(Capable); ok && !c.Capabilities().GetContextSetInject() {
		return errs.ErrCapabilityUnsupported.
			WithFix("run `kno doctor` for the capability matrix, then validate against an " +
				"adapter that declares context_set_inject").
			Wrap(fmt.Errorf("validate: the agent implements ContextSetInjector but declares "+
				"context_set_inject false, so %w — an adapter that answered anyway would "+
				"measure every holdout Case without the Portfolio in BOTH arms and report "+
				"the resulting noise as a verdict", errNoSetInjection))
	}
	return nil
}

// validatePlan is everything Validate resolves before it spends anything.
//
// Built identically by Quote and by Validate, so the figure the user consents
// to is a bound on the run by construction rather than by trust.
type validatePlan struct {
	portfolio *Portfolio

	// entries are the DESTINATION_CONTEXT entries in rank order, and assets
	// are their Pool contents in the same order. Order is part of the
	// measurement: providers cache on a prefix, and a stable portfolio prefix
	// across every holdout Case is what keeps the set's tokens from being paid
	// for once per Case.
	entries []*PortfolioEntry
	assets  []*Asset

	// excluded names the entries --context-only left out, so the subset label
	// is auditable rather than asserted.
	excluded []string

	// cases is the materialized holdout. Materialized once so both arms and
	// every trial measure the SAME Cases.
	cases []*Case

	valueRunID       string
	baselineRunID    string
	incompleteReason string

	trials   int32
	useIndex int32

	nothingToValidate bool
}

// underpowered reports a holdout too small to support a full-strength number.
func (p *validatePlan) underpowered(threshold int32) bool {
	return threshold > 0 && len(p.cases) > 0 && int32(len(p.cases)) < threshold //nolint:gosec // bounded by the eval set
}

// calls is the schedule this plan will execute.
func (p *validatePlan) calls() int64 {
	if p.nothingToValidate {
		return 0
	}
	return int64(len(p.cases)) * int64(validateArms) * int64(p.trials)
}

// Quote computes what a Validate run would cost, without spending and without
// consuming the holdout.
//
// It opens the holdout to COUNT it, which is not a peek: no Case is scored, no
// score is read, and no holdout_uses row is written. The one-shot record is
// about measurement, and measurement begins in Validate.
func (o ValidateOptions) Quote(ctx context.Context) (ValidateQuote, error) {
	if err := o.validate(); err != nil {
		return ValidateQuote{}, err
	}
	plan, err := o.plan(ctx)
	if err != nil {
		return ValidateQuote{}, err
	}
	return ValidateQuote{
		HoldoutCases:      len(plan.cases),
		Arms:              validateArms,
		Trials:            plan.trials,
		Calls:             plan.calls(),
		AssetCount:        len(plan.assets),
		Underpowered:      plan.underpowered(o.MinHoldout),
		ExcludedAssetIDs:  plan.excluded,
		NothingToValidate: plan.nothingToValidate,
	}, nil
}

// plan resolves the Portfolio, the Assets, the holdout and the one-shot record
// — every refusal that is free, in the order that keeps each one free.
func (o ValidateOptions) plan(ctx context.Context) (*validatePlan, error) {
	p := &validatePlan{trials: o.trials()}

	if err := o.chain(ctx, p); err != nil {
		return nil, err
	}
	if err := o.entriesFor(p); err != nil {
		return nil, err
	}
	if p.nothingToValidate {
		// Nothing is measured, so nothing else needs resolving: no Pool read,
		// no holdout opened, no use recorded. An empty Portfolio is a legal,
		// complete answer — Select says so — and validate must not turn it
		// into a consumed holdout.
		return p, nil
	}
	if err := o.assetsFor(ctx, p); err != nil {
		return nil, err
	}
	if err := o.holdoutFor(ctx, p); err != nil {
		return nil, err
	}
	return p, o.oneShot(ctx, p)
}

// chain walks Select -> Value -> Baseline and refuses a broken link.
//
// Mirrors cli/report.go's chain check, and for the same reason: a Validation
// naming a Select run that is not a Select run, or whose Portfolio was built
// from a Value run that no longer exists, is a headline number attached to a
// provenance that cannot be followed.
func (o ValidateOptions) chain(ctx context.Context, p *validatePlan) error {
	run, err := o.Store.GetRun(ctx, o.SelectRunID)
	if err != nil {
		return errs.ErrInvalidInput.
			WithFix("pass the run ID of a `kno select` run").
			Wrap(fmt.Errorf("loading select run %s: %w", o.SelectRunID, err))
	}
	if got := run.GetStage(); got != knov1.Stage_STAGE_SELECT {
		return errs.ErrInvalidInput.
			WithFix("pass the run ID of a `kno select` run").
			Wrap(fmt.Errorf("run %s is a %s run, not a select", o.SelectRunID, got))
	}
	portfolio, err := o.Store.Portfolio(ctx, o.SelectRunID)
	if err != nil {
		return errs.ErrInvalidInput.
			WithFix("run `kno select` to completion, then validate its run ID").
			Wrap(fmt.Errorf("loading the Portfolio for %s: %w", o.SelectRunID, err))
	}
	p.portfolio = portfolio
	p.valueRunID = portfolio.GetSourceRunId()
	p.incompleteReason = portfolio.GetSourceIncompleteReason()

	if p.valueRunID == "" {
		return errs.ErrInvalidInput.
			WithFix("re-run `kno select` against a Value run").
			Wrap(fmt.Errorf("the Portfolio from %s names no source Value run, so the "+
				"measurement behind it cannot be followed", o.SelectRunID))
	}
	valueRun, err := o.Store.GetRun(ctx, p.valueRunID)
	if err != nil {
		return errs.ErrInvalidInput.
			WithFix("validate against a Portfolio whose Value run is still in this database").
			Wrap(fmt.Errorf("loading value run %s: %w", p.valueRunID, err))
	}
	if got := valueRun.GetStage(); got != knov1.Stage_STAGE_VALUE {
		return errs.ErrInvalidInput.Wrap(fmt.Errorf(
			"the Portfolio from %s names %s as its source, which is a %s run, not a value",
			o.SelectRunID, p.valueRunID, got))
	}
	p.baselineRunID = valueRun.GetBaselineRunId()
	if p.baselineRunID == "" {
		return errs.ErrInvalidInput.Wrap(fmt.Errorf(
			"value run %s names no baseline run, so the chain behind this Portfolio "+
				"is broken", p.valueRunID))
	}
	return nil
}

// entriesFor partitions the Portfolio by Destination.
//
// v0.2 validates a Portfolio whose selected entries are ALL
// DESTINATION_CONTEXT. knowledge_base needs the writable-KB adapters that do
// not exist yet and tuning_set needs the Tuner path, so measuring the context
// subset and calling the result "the Portfolio's holdout gain" would be a
// number about a different set than the one the user is about to export.
// Refusing by default, with an opt-in that CHANGES THE LABEL, is the version
// of this that cannot be misread.
func (o ValidateOptions) entriesFor(p *validatePlan) error {
	var other []string
	for _, e := range p.portfolio.GetSelected() {
		if e.GetDestination() == knov1.Destination_DESTINATION_CONTEXT {
			p.entries = append(p.entries, e)
			continue
		}
		other = append(other, e.GetAssetId())
	}
	if len(other) > 0 && !o.ContextOnly {
		return errs.ErrInvalidInput.
			WithFix("pass --context-only to validate the context entries alone; the number " +
				"is then labelled a subset everywhere it appears").
			Wrap(fmt.Errorf("the Portfolio from %s selects %d entr(ies) for a destination "+
				"validate cannot measure yet (%s); a number covering only the context "+
				"entries, reported as the Portfolio's holdout gain, would be a number "+
				"about a different set than the one you are about to export",
				o.SelectRunID, len(other), strings.Join(other, ", ")))
	}
	p.excluded = other

	// Rank order, and it is part of the measurement rather than a display
	// choice. Providers cache on a PREFIX: a portfolio prefix that is
	// byte-identical across every holdout Case is the difference between
	// paying for the set's tokens once and paying for them n_holdout times.
	sort.SliceStable(p.entries, func(i, j int) bool {
		return p.entries[i].GetRank() < p.entries[j].GetRank()
	})
	if len(p.entries) == 0 {
		p.nothingToValidate = true
	}
	return nil
}

// assetsFor reads the Pool and refuses a set that is not the set that was
// selected.
//
// Both refusals are free and both are before any spend. An Asset missing from
// the Pool would silently shrink the measured set; an Asset whose content
// changed would produce a holdout number for a set that is not the set the
// report names, undetectably.
func (o ValidateOptions) assetsFor(ctx context.Context, p *validatePlan) error {
	seq, err := o.Pool.Assets(ctx)
	if err != nil {
		return errs.ErrInvalidInput.WithFix("check --pool").Wrap(err)
	}
	want := make(map[string]bool, len(p.entries))
	for _, e := range p.entries {
		want[e.GetAssetId()] = true
	}
	byID := make(map[string]*Asset, len(want))
	for a, err := range seq {
		if err != nil {
			return fmt.Errorf("reading the pool: %w", err)
		}
		if !want[a.GetId()] {
			continue
		}
		// Cloned: the iterator borrows its value for one iteration, and these
		// outlive the loop and travel into the injected prompt.
		byID[a.GetId()] = proto.Clone(a).(*Asset)
	}

	var missing, changed []string
	for _, e := range p.entries {
		a, ok := byID[e.GetAssetId()]
		if !ok {
			missing = append(missing, e.GetAssetId())
			continue
		}
		if h := e.GetContentHash(); len(h) > 0 {
			sum := sha256.Sum256(a.GetContent())
			if !bytes.Equal(h, sum[:]) {
				changed = append(changed, e.GetAssetId())
				continue
			}
		}
		p.assets = append(p.assets, a)
	}
	if len(missing) > 0 {
		return errs.ErrInvalidInput.
			WithFix("point --pool at the Assets `kno select` ran against").
			Wrap(fmt.Errorf("the Portfolio names %d Asset(s) this pool does not hold (%s); "+
				"measuring the rest would report a gain for a smaller set than the one "+
				"the Portfolio names", len(missing), strings.Join(missing, ", ")))
	}
	if len(changed) > 0 {
		return errs.ErrInvalidInput.
			WithFix("re-run `kno value` and `kno select` against the edited pool, or " +
				"validate against the pool as it was at selection time").
			Wrap(fmt.Errorf("the content of %d Asset(s) changed since `kno select` recorded "+
				"it (%s); the holdout number would describe a set that is not the set "+
				"the report names", len(changed), strings.Join(changed, ", ")))
	}
	return o.checkContextBudget(p)
}

// checkContextBudget refuses a Portfolio that no longer fits its own cap.
//
// The count is the pessimistic bytes-per-token estimate docs/debt.md#68 names —
// it reserves roughly three times what English prose uses — so this refusal is
// conservative, and the message says so rather than presenting the estimate as
// a measurement.
func (o ValidateOptions) checkContextBudget(p *validatePlan) error {
	ceiling := o.MaxContextTokens
	if ceiling == 0 {
		ceiling = p.portfolio.GetBudget().GetMaxContextTokens()
	}
	if ceiling <= 0 {
		return nil
	}
	var total int64
	for _, a := range p.assets {
		total += a.GetCost().GetContextTokens()
	}
	if total <= ceiling {
		return nil
	}
	return errs.ErrInvalidInput.
		WithFix("raise --max-context-tokens, or re-select under a smaller budget").
		Wrap(fmt.Errorf("the selected set carries an estimated %d context tokens against a "+
			"ceiling of %d; the count is the engine's pessimistic bytes-based estimate, "+
			"so this refusal is conservative rather than measured", total, ceiling))
}

// holdoutFor opens the holdout and materializes it.
//
// This is the only call to openHoldout in the whole module, and
// TestOnlyValidateOpensTheHoldout asserts exactly that. Materialized once so
// both arms and every trial measure the SAME Cases; cloned per the iterator's
// borrow rule.
func (o ValidateOptions) holdoutFor(ctx context.Context, p *validatePlan) error {
	holdout, err := openHoldout(o.Evals)
	if err != nil {
		return err
	}
	seq, err := holdout.Cases(ctx)
	if err != nil {
		return fmt.Errorf("reading the holdout: %w", err)
	}
	for c, err := range seq {
		if err != nil {
			return fmt.Errorf("reading the holdout: %w", err)
		}
		p.cases = append(p.cases, proto.Clone(c).(*Case))
	}
	if len(p.cases) == 0 {
		// split.Counts.Validate refuses this at Baseline, but the eval source
		// can change between runs, so it is re-checked here rather than
		// assumed. A validate over zero Cases would produce a Validation with
		// no number and a consumed holdout, which is the worst of both.
		return errs.ErrInvalidInput.
			WithFix("check --evals and --split-seed name the eval set the pipeline was " +
				"measured against; run `kno evals inspect` to see the split").
			Wrap(fmt.Errorf("this eval source yields no holdout Cases, so there is nothing " +
				"to validate against"))
	}
	return nil
}

// oneShot enforces that a Portfolio meets a holdout exactly once.
//
// The three rules, in the order they are checked:
//
//   - Same Portfolio, prior run COMPLETED: refused, and there is no flag. The
//     number already exists; re-measuring it is not a new experiment, it is
//     the same experiment run twice and reported once.
//   - Same Portfolio, prior run INTERRUPTED or BUDGET_STOPPED: a FRESH run is
//     refused with a fix naming --resume. A resume is not a second peek —
//     CompletedMeasurements skips finished Cases, so the resumed process reads
//     only Cases the first never reached.
//   - A DIFFERENT Portfolio against the same holdout: allowed under
//     --allow-repeat-holdout, counted, and disclosed.
func (o ValidateOptions) oneShot(ctx context.Context, p *validatePlan) error {
	uses, err := o.Store.HoldoutUses(ctx, o.EvalFingerprint)
	if err != nil {
		return fmt.Errorf("reading this holdout's use record: %w", err)
	}

	var prior *store.HoldoutUse
	for i := range uses {
		if uses[i].SelectRunID == o.SelectRunID {
			prior = &uses[i]
			break
		}
	}

	if prior != nil {
		// The resume path re-enters here with its own row already present.
		// That row IS this run, so it is not a repeat of anything.
		if o.Resume && prior.ValidateRunID == o.RunID {
			p.useIndex = useIndexOf(uses, o.SelectRunID)
			return nil
		}
		status := knov1.RunStatus_RUN_STATUS_UNSPECIFIED
		if run, err := o.Store.GetRun(ctx, prior.ValidateRunID); err == nil {
			status = run.GetStatus()
		}
		if status == knov1.RunStatus_RUN_STATUS_COMPLETED {
			return errs.ErrInvalidInput.
				WithFix(fmt.Sprintf("read it with `kno report --validate-run-id %s`",
					prior.ValidateRunID)).
				Wrap(fmt.Errorf("this Portfolio has already met this holdout, in validate "+
					"run %s. The holdout is consumable once per Portfolio: measuring it "+
					"again is not a new experiment, it is the same experiment run twice "+
					"and reported once", prior.ValidateRunID))
		}
		return errs.ErrInvalidInput.
			WithFix(fmt.Sprintf("continue it with `kno validate --resume --run-id %s`",
				prior.ValidateRunID)).
			Wrap(fmt.Errorf("validate run %s already opened this holdout for this Portfolio "+
				"and did not finish (%s). A run that stopped part-way has already seen "+
				"part of the holdout, so a fresh run would be a second look; resuming "+
				"reads only the Cases the first process never reached",
				prior.ValidateRunID, status))
	}

	if o.Resume {
		return errs.ErrInvalidInput.
			WithFix("drop --resume to start this Portfolio's validation").
			Wrap(fmt.Errorf("there is no interrupted validate run for this Portfolio " +
				"against this holdout to resume"))
	}
	if len(uses) > 0 && !o.AllowRepeatHoldout {
		return errs.ErrInvalidInput.
			WithFix("pass --allow-repeat-holdout to measure a second Portfolio against " +
				"this holdout; the count is recorded and printed with the number").
			Wrap(fmt.Errorf("this holdout has already measured %d portfolio(s) (%s). "+
				"Validating another re-introduces exactly the multiplicity the holdout "+
				"existed to remove, and the interval below is not corrected for it",
				len(uses), strings.Join(selectRunIDs(uses), ", ")))
	}
	p.useIndex = int32(len(uses)) + 1 //nolint:gosec // bounded by the number of portfolios a user can select
	return nil
}

// useIndexOf reports a Portfolio's ordinal among a holdout's recorded uses.
func useIndexOf(uses []store.HoldoutUse, selectRunID string) int32 {
	for i := range uses {
		if uses[i].SelectRunID == selectRunID {
			return int32(i) + 1
		}
	}
	return int32(len(uses)) + 1 //nolint:gosec // bounded by the number of portfolios a user can select
}

// selectRunIDs lists the Portfolios a holdout has measured.
func selectRunIDs(uses []store.HoldoutUse) []string {
	out := make([]string, 0, len(uses))
	for i := range uses {
		out = append(out, uses[i].SelectRunID)
	}
	return out
}

// trials resolves the repeat count.
func (o ValidateOptions) trials() int32 {
	if o.Trials > 0 {
		return o.Trials
	}
	return defaultValidateTrials
}

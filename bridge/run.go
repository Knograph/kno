package bridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/stats/interval"
	"github.com/knograph/kno/store"
)

// EvalRunner runs one ablation group's proxy model over its dev and control
// Cases and returns per-Case score deltas, already sign-corrected for the
// Goal's direction — the SAME contract core/value_measure.go's internal
// `pairs` helper produces for Value, one vector per Case (bridge measures
// each group's model exactly once per Case, so there is no trial dimension
// to average within a Case the way Value's ragged-trial pairing has to).
//
// This is an INJECTION SEAM, not a convenience wrapper: computing a real
// answer means invoking a deployed model through the same budget-guarded,
// retrying, panic-safe path core.invoker already implements for Value and
// Validate (core/invoke.go) — six separately-discovered money and
// correctness defects live in that type's retry loop, per its own doc, and
// bridge does not re-implement them a third time. That type is UNEXPORTED
// from core today. Run refuses to start without an EvalRunner (see
// RunParams.validate) rather than deploying a paid endpoint and then
// silently skipping the measurement it exists to buy — see this PR's
// report for the scope decision and what wiring a real implementation
// needs (an Evals source the current `kno bridge` flags do not accept, and
// an Agent built from the deployed Endpoint via adapters/agent/openaicompat).
type EvalRunner interface {
	// Measure returns goalDeltas (paired against the all-in model, over the
	// group's dev Cases — empty for the "all-in" group itself, which IS the
	// baseline) and controlDeltas (paired against the same baseline over
	// value.Plan.ControlCaseIDs). Each inner slice is exactly one value: the
	// dimension core/value_measure.go's perCaseMeans expects, kept ragged-
	// shape-compatible with stats/interval's PairedTrials rather than
	// introducing a second interval entry point for a single-trial case.
	Measure(ctx context.Context, group string, model *knov1.AgentRef, devCaseIDs, controlCaseIDs []string) (goalDeltas, controlDeltas [][]float64, err error)
}

// RunParams bundles one full bridge run: every group's submission, polling,
// reconciliation, and (when Eval is configured) deployment and measurement.
type RunParams struct {
	RunID    string
	Store    store.Store
	Guard    *budget.Guard
	Tuner    core.Tuner
	Emitter  *Emitter
	Provider string

	// Quotes is QuoteGroups' output: one entry per group, already priced and
	// rendered — Run does not re-render or re-price.
	Quotes []GroupQuote

	BaseModel *knov1.AgentRef
	Epochs    int32

	// PollInterval bounds how often Status is polled. Defaults to 10s.
	PollInterval time.Duration

	// JobTimeout is --bridge-timeout: how long Run waits for one job to
	// reach a terminal status before giving up on WAITING (never
	// cancelling — see acceptance criterion 24). Defaults to 60 minutes.
	JobTimeout time.Duration

	// GoalDomain and Level parameterize the interval methods Run calls once
	// an EvalRunner reports deltas.
	GoalDomain knov1.ScoreDomain
	Level      float64

	// DevCaseIDs maps each leave-one-out group's name to its cluster's dev
	// Case IDs — value.Plan.Clusters[i].CaseIDs, keyed by tag. The AllIn
	// group needs no entry: it is the baseline every other group's Eval
	// call is paired against.
	DevCaseIDs map[string][]string

	// ControlCaseIDs is the reserved control partition every group's
	// interference read is measured over.
	ControlCaseIDs []string

	// Eval computes each group's measurement. REQUIRED: see EvalRunner's
	// doc for why Run refuses to start without one rather than deploying a
	// paid endpoint and skipping what it was deployed to buy.
	Eval EvalRunner

	// ServePrice, ServeReplicas, MaxLiveEndpoints and MaxServeMinutes
	// configure the hosting lifecycle Deploy uses once a group's job
	// succeeds.
	ServePrice       pricing.ServePrice
	ServeReplicas    int
	MaxLiveEndpoints int
	MaxServeMinutes  int32

	// TickInterval bounds how often SettleServeTick runs while an
	// endpoint is live. Defaults to 1 minute.
	TickInterval time.Duration
}

func (p *RunParams) validate() error {
	switch {
	case p.RunID == "":
		return errors.New("bridge: Run requires a run ID")
	case p.Store == nil || p.Guard == nil || p.Tuner == nil || p.Emitter == nil:
		return errors.New("bridge: Run requires a Store, Guard, Tuner, and Emitter")
	case len(p.Quotes) == 0:
		return errors.New("bridge: Run requires at least one GroupQuote")
	case p.Eval == nil:
		// See EvalRunner's doc. This is a configuration refusal, checked
		// BEFORE any Submit call: deploying a paid endpoint with no way to
		// measure it would spend real hosting money for zero information,
		// which is worse than refusing to start.
		return errors.New("bridge: Run requires an EvalRunner; see EvalRunner's doc for why this build " +
			"ships the interface without a production implementation")
	}
	if p.PollInterval <= 0 {
		p.PollInterval = 10 * time.Second
	}
	if p.JobTimeout <= 0 {
		p.JobTimeout = 60 * time.Minute
	}
	if p.TickInterval <= 0 {
		p.TickInterval = time.Minute
	}
	if p.ServeReplicas <= 0 {
		p.ServeReplicas = 1
	}
	if p.Level <= 0 {
		p.Level = 0.95
	}
	return nil
}

// ErrJobTimedOut is returned when a job does not reach a terminal status
// within RunParams.JobTimeout — acceptance criterion 24: stops WAITING,
// never cancels, the row stays non-terminal, and resume polls the same
// JobRef rather than re-submitting.
var ErrJobTimedOut = errors.New("bridge: a tuning job did not reach a terminal status before --bridge-timeout")

// RunResult is what Run produced.
type RunResult struct {
	Measured []*knov1.BridgeGroupMeasured
	Skipped  []string // groups whose job did not succeed: reported unknown, never substituted
}

// Run executes the tuner-bridge plan's Steps 2 and 3 end to end for every
// group in p.Quotes, in order: the resume-time endpoint sweep FIRST (Step
// 2(g): "a resumed bridge run's first act, before any deploy and before any
// submit"), then per group — submit-or-adopt-or-resume, poll to terminal,
// reconcile, deploy, settle-forward while Eval measures, teardown
// (unconditional), and emit BridgeGroupMeasured.
//
// A group whose job does not reach JOB_STATUS_SUCCEEDED is reported
// unknown and Run continues to the next group — Step 3's "all-in job
// succeeds, one LOO job fails" edge case: partial is reported as partial,
// never substituted.
func Run(ctx context.Context, p RunParams) (*RunResult, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}

	swept, err := SweepEndpoints(ctx, p.Store, p.Tuner, p.Guard, p.Emitter, p.RunID, p.MaxServeMinutes)
	if err != nil {
		return nil, fmt.Errorf("bridge: resume-time endpoint sweep: %w", err)
	}
	_ = swept // callers that want the sweep report read it via kno doctor / the store directly

	limiter := NewLiveEndpointLimiter(p.MaxLiveEndpoints)
	result := &RunResult{}

	// AllIn runs first (Groups() already orders it first) and its measured
	// scores become every leave-one-out group's baseline.
	var baselineModel *knov1.AgentRef

	for _, q := range p.Quotes {
		ref, err := submitOrResumeGroup(ctx, p, q)
		if err != nil {
			return result, err
		}
		if ref == nil {
			// Abandoned: no recoverable job for this group. Reported
			// unknown, never substituted.
			result.Skipped = append(result.Skipped, q.Group)
			continue
		}

		state, err := pollToTerminal(ctx, p, q.Group, ref)
		if err != nil {
			return result, err
		}

		rec, err := findRecord(ctx, p.Store, p.RunID, q.Group)
		if err != nil {
			return result, err
		}
		if _, err := ReconcileTerminal(ctx, p.Store, p.Guard, p.RunID, rec, state); err != nil {
			return result, err
		}
		if err := p.Emitter.JobStateChanged(ctx, ref.GetId(), q.Group, state); err != nil {
			return result, err
		}

		if state.GetStatus() != knov1.JobStatus_JOB_STATUS_SUCCEEDED {
			result.Skipped = append(result.Skipped, q.Group)
			continue
		}

		measured, model, err := deployMeasureTeardown(ctx, p, q, ref, limiter, baselineModel)
		if err != nil {
			return result, err
		}
		if q.Group == AllIn {
			baselineModel = model
		}
		result.Measured = append(result.Measured, measured)
		if err := p.Emitter.GroupMeasured(ctx, measured); err != nil {
			return result, err
		}
	}
	return result, nil
}

// submitOrResumeGroup calls SubmitGroup and translates its outcome into a
// JobRef to poll, or nil for a group this run cannot recover (abandoned).
func submitOrResumeGroup(ctx context.Context, p RunParams, q GroupQuote) (*core.JobRef, error) {
	job := &core.TuningJob{
		BaseModel:              p.BaseModel,
		AssetIds:               q.AssetIDs,
		TrainingData:           q.TrainingData,
		Epochs:                 p.Epochs,
		Suffix:                 suffixFor(p.RunID, q.Group),
		AblationGroup:          q.Group,
		EstimatedCostUsdMicros: q.EstimatedCostUSDMicros,
	}
	res, err := SubmitGroup(ctx, SubmitGroupParams{
		RunID: p.RunID, AblationGroup: q.Group,
		Store: p.Store, Guard: p.Guard, Tuner: p.Tuner,
		Job: job, TrainTokens: q.TrainTokens,
		TrainingFileSHA256: q.TrainingFileSHA256, Provider: p.Provider,
	})
	if err != nil {
		if errors.Is(err, ErrAlreadyAbandoned) {
			return nil, nil
		}
		return nil, err
	}
	switch res.Outcome {
	case SubmitOutcomeAbandoned:
		if err := p.Emitter.OrphanSpend(ctx, q.Group, 0, 0); err != nil {
			return nil, err
		}
		return nil, nil
	case SubmitOutcomeSubmitted:
		if err := p.Emitter.JobSubmitted(ctx, q.Group, p.Provider, res.Ref.GetId(), p.BaseModel,
			q.EstimatedCostUSDMicros, q.TrainTokens); err != nil {
			return nil, err
		}
	}
	return res.Ref, nil
}

// suffixFor builds the model-name suffix every submitted job carries for
// traceability and adopt-by-suffix recovery — TuningJob.suffix's own godoc:
// "kno-<run_id>-<group>".
func suffixFor(runID, group string) string {
	return fmt.Sprintf("kno-%s-%s", runID, group)
}

// pollToTerminal polls a job's Status until it reaches a terminal
// JobStatus or JobTimeout elapses. On timeout it returns ErrJobTimedOut
// WITHOUT cancelling the job — acceptance criterion 24.
func pollToTerminal(ctx context.Context, p RunParams, group string, ref *core.JobRef) (*core.JobState, error) {
	deadline := time.Now().Add(p.JobTimeout)
	for {
		state, err := p.Tuner.Status(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("polling the %s group's job %s: %w", group, ref.GetId(), err)
		}
		if err := p.Emitter.JobStateChanged(ctx, ref.GetId(), group, state); err != nil {
			return nil, err
		}
		if isTerminal(state.GetStatus()) {
			return state, nil
		}
		if time.Now().After(deadline) {
			return nil, errs.ErrInterrupted.
				WithFix(fmt.Sprintf("the %s group's job %s is still running at the provider; "+
					"re-run with --resume to keep polling it, or check the provider's console", group, ref.GetId())).
				Wrap(fmt.Errorf("%w: group %s job %s", ErrJobTimedOut, group, ref.GetId()))
		}
		select {
		case <-time.After(p.PollInterval):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func isTerminal(s knov1.JobStatus) bool {
	switch s {
	case knov1.JobStatus_JOB_STATUS_SUCCEEDED, knov1.JobStatus_JOB_STATUS_FAILED, knov1.JobStatus_JOB_STATUS_CANCELLED:
		return true
	default:
		return false
	}
}

// deployMeasureTeardown deploys a succeeded job's model, runs its eval
// passes while settling hosting minutes forward, and tears the endpoint
// down UNCONDITIONALLY — success or failure — before returning. Teardown's
// own failure is never swallowed: it fails this call, per
// core.Tuner.Teardown's contract.
func deployMeasureTeardown(
	ctx context.Context, p RunParams, q GroupQuote, ref *core.JobRef,
	limiter *LiveEndpointLimiter, baselineModel *knov1.AgentRef,
) (*knov1.BridgeGroupMeasured, *knov1.AgentRef, error) {
	if err := limiter.Acquire(ctx); err != nil {
		return nil, nil, fmt.Errorf("acquiring a live-endpoint slot for %s: %w", q.Group, err)
	}
	defer limiter.Release()

	ep, err := DeployGroup(ctx, DeployParams{
		RunID: p.RunID, AblationGroup: q.Group, Store: p.Store, Tuner: p.Tuner,
		Emitter: p.Emitter, Ref: ref,
	})
	if err != nil {
		return nil, nil, err
	}

	// Teardown is unconditional and defer-shaped, per the plan's Step
	// 2(f): it runs on success, on eval-pass failure, on the serve-minute
	// cap, and on cancellation. Its own failure is never swallowed — it
	// replaces whatever error this function was about to return with its
	// own, naming the endpoint, because a leaked endpoint outranks a
	// measurement error in urgency.
	var measureErr error
	defer func() {
		tdErr := TeardownGroup(ctx, TeardownParams{
			RunID: p.RunID, AblationGroup: q.Group, Store: p.Store, Tuner: p.Tuner,
			Emitter: p.Emitter, Endpoint: ep,
		})
		if tdErr != nil {
			measureErr = tdErr
		}
	}()

	model := &knov1.AgentRef{Ref: ep.Served, Scheme: ep.Provider, Target: ep.Served}
	if q.Group == AllIn {
		// The all-in group IS the baseline every leave-one-out group is
		// compared against; it has no delta of its own to report against a
		// prior model.
		measureErr = nil
		return &knov1.BridgeGroupMeasured{
			AblationGroup: q.Group,
			NotMeasured:   knov1.RejectionReason_REJECTION_REASON_UNSPECIFIED,
			Verdict:       knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_UNSPECIFIED,
		}, model, measureErr
	}
	if baselineModel == nil {
		measureErr = fmt.Errorf("bridge: group %s reached deploy with no all-in baseline model — "+
			"the all-in job must succeed before any leave-one-out group's delta is meaningful", q.Group)
		return nil, model, measureErr
	}

	stopTick := startServeTicker(ctx, p, q.Group, ep)
	defer stopTick()

	devCaseIDs := p.DevCaseIDs[q.Group]
	goalDeltas, controlDeltas, err := p.Eval.Measure(ctx, q.Group, model, devCaseIDs, p.ControlCaseIDs)
	if err != nil {
		measureErr = fmt.Errorf("measuring the %s group: %w", q.Group, err)
		return nil, model, measureErr
	}

	ev := groupMeasuredEvent(q.Group, p.GoalDomain, p.Level, goalDeltas, controlDeltas)
	return ev, model, measureErr
}

// startServeTicker starts a background goroutine settling hosting minutes
// forward at p.TickInterval and returns a function that stops it. The
// caller must call the stop function before returning so the goroutine
// never outlives the endpoint it is billing for.
func startServeTicker(ctx context.Context, p RunParams, group string, ep *core.Endpoint) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(p.TickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_, _ = SettleServeTick(ctx, SettleServeParams{
					RunID: p.RunID, AblationGroup: group, Store: p.Store, Guard: p.Guard,
					Price: p.ServePrice, Replicas: p.ServeReplicas, ReadyAt: ep.ReadyAt,
				})
			case <-done:
				// A final tick on the way out, so the last partial minute
				// before Teardown is not lost.
				_, _ = SettleServeTick(ctx, SettleServeParams{
					RunID: p.RunID, AblationGroup: group, Store: p.Store, Guard: p.Guard,
					Price: p.ServePrice, Replicas: p.ServeReplicas, ReadyAt: ep.ReadyAt,
				})
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return func() { close(done) }
}

// groupMeasuredEvent computes Δ_group and Δ_control with their intervals
// and derives the verdict — prime directive 5's own rule applied here: a
// delta is never carried without its interval.
func groupMeasuredEvent(group string, domain knov1.ScoreDomain, level float64, goalDeltas, controlDeltas [][]float64) *knov1.BridgeGroupMeasured {
	ev := &knov1.BridgeGroupMeasured{AblationGroup: group}

	var goalIv *knov1.Interval
	if len(goalDeltas) > 0 {
		goalIv = interval.PairedTrials(goalDeltas, domain, level)
	}
	var controlIv *knov1.Interval
	controlUnderpowered := true
	if len(controlDeltas) > 0 {
		if means, trials, ok := perCaseMeansLocal(controlDeltas); ok {
			controlIv = interval.HarmBound(means, domain, trials, level)
			controlUnderpowered = controlIv == nil
		}
	}

	if goalIv == nil {
		ev.NotMeasured = knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED
		ev.Verdict = knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_UNSPECIFIED
		return ev
	}

	ev.DeltaGroup = meanOfMeansLocal(goalDeltas)
	ev.DeltaGroupInterval = goalIv
	if controlIv != nil {
		ev.DeltaControl = meanOfMeansLocal(controlDeltas)
		ev.DeltaControlInterval = controlIv
	}
	ev.ControlUnderpowered = controlUnderpowered

	// INTERFERENCE IS DELIBERATELY NOT DECIDED HERE. core/select.go's own
	// harm gate (netInterval, core/select.go:495) does not read
	// ControlInterval alone — it combines DeltaInterval and ControlInterval
	// into a variance-weighted NET interval (netInterval, unexported) and
	// gates on net.GetHigh() <= 0. HarmBound's own doc (stats/interval/
	// interval.go:131) explains why a naive "Low <= 0" or "Low > 0" read of
	// the ONE-SIDED bound alone is exactly the underpowered-looks-like-
	// passed failure mode this package exists to avoid: shipping a guessed
	// sign rule here for a P0-sensitive "did this measurably regress
	// something" claim is a statistical-validity risk CLAUDE.md's prime
	// directive 5 exists to prevent, not a detail to approximate. See this
	// PR's report — BRIDGE_GROUP_VERDICT_INTERFERENCE is never emitted by
	// this build; a group that would need it reports CONFIRMED or
	// UNCONFIRMED on delta_group alone, and delta_control/its interval are
	// still recorded on the event for a human or a future gate to read.
	if goalIv.GetLow() > 0 {
		ev.Verdict = knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_CONFIRMED
	} else {
		ev.Verdict = knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_UNCONFIRMED
		ev.NotMeasured = knov1.RejectionReason_REJECTION_REASON_BRIDGE_UNCONFIRMED
	}
	return ev
}

// perCaseMeansLocal and meanOfMeansLocal mirror core/value_measure.go's
// perCaseMeans/meanOfMeans exactly (both unexported there, so bridge cannot
// import them) — collapsing each Case's per-trial deltas to one mean per
// Case, then averaging the Cases. Kept dependency-free and this small
// deliberately, rather than exporting core's version, because bridge's
// EvalRunner contract is single-trial per Case (see EvalRunner's doc): a
// divergence between the two copies here would show up immediately as a
// wrong-shaped interval, not as a silent drift in what "trial" means.
func perCaseMeansLocal(perCase [][]float64) (means []float64, trials int, ok bool) {
	if len(perCase) == 0 {
		return nil, 0, false
	}
	trials = len(perCase[0])
	if trials == 0 {
		return nil, 0, false
	}
	means = make([]float64, len(perCase))
	for i, tr := range perCase {
		if len(tr) != trials {
			return nil, 0, false
		}
		var sum float64
		for _, v := range tr {
			sum += v
		}
		means[i] = sum / float64(trials)
	}
	return means, trials, true
}

func meanOfMeansLocal(perCase [][]float64) float64 {
	means, _, ok := perCaseMeansLocal(perCase)
	if !ok || len(means) == 0 {
		return 0
	}
	var total float64
	for _, m := range means {
		total += m
	}
	return total / float64(len(means))
}

package bridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// EvalRunner invokes one ablation group's deployed proxy model over a set
// of Cases and returns each Case's RAW score, direction-normalised, keyed
// by Case ID — never a delta, and never positional.
//
// core.ScorePass (core/score.go) is the production implementation's
// injection seam: computing a real answer means invoking a deployed model
// through the same budget-guarded, retrying, panic-safe path
// core.invoker already implements for Value and Validate — six
// separately-discovered money and correctness defects live in that type's
// retry loop, per its own doc, and bridge does not re-implement them a
// third time. See NewEvalRunner.
//
// KEYED BY CASE ID, NOT POSITIONAL, and that shape is load-bearing rather
// than a style choice. core/value/route.go's cluster() assigns a Case to
// EVERY matching cluster, so a Case tagged with two failure tags sits in
// two groups' dev sets with different membership and different ordering
// from the all-in union pass. Pairing "slot i of the union pass" against
// "slot j of group G's pass" is wrong, and wrong in the worst way — a
// plausible interval of the wrong width. Keyed lookups make that alignment
// problem not exist: a Case in two clusters is looked up twice, correctly,
// which is what a multi-tag Case should do. bridge.Run computes
// Δ = groupScores[id] − allInScores[id] over exactly the group's own dev
// Case IDs (see computeVerdict, bridge/measure.go) — pairing is Run's
// job, the component that legitimately holds both sides, never Measure's.
//
// Run refuses to start without an EvalRunner (see RunParams.validate)
// rather than deploying a paid endpoint and then silently skipping the
// measurement it exists to buy.
type EvalRunner interface {
	// Measure invokes model once per Case in caseIDs and returns each
	// Case's score. caseIDs carries no dev/control distinction — the
	// caller (bridge.Run) decides what the set means and which Cases it
	// pairs against which baseline. A Case Measure cannot score (a
	// transport error, a refused capability) is simply absent from the
	// returned map; that alone is not an error.
	Measure(ctx context.Context, group string, model *knov1.AgentRef, caseIDs []string) (map[string]float64, error)
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
	// an EvalRunner reports scores.
	GoalDomain knov1.ScoreDomain
	Level      float64

	// NGroups is the Bonferroni multiplicity N — the bridge eval-seam
	// plan's §8: the PLANNED leave-one-out group count, fixed at quote
	// time (len(Quotes)-1, excluding AllIn), never a count of groups that
	// happened to reach a verdict. A dynamic N would make one group's
	// correction, and therefore its verdict, depend on how many OTHER
	// groups' jobs had failed by the time it was measured — the same
	// group could then get a different verdict on a resume depending on
	// unrelated failures. Values below 2 apply no correction (Bonferroni
	// over a single comparison is a no-op; portfolio.Correct itself
	// refuses nScreened < 2).
	NGroups int

	// DevCaseIDs maps each leave-one-out group's name to its cluster's dev
	// Case IDs — value.Plan.Clusters[i].CaseIDs, keyed by tag. The AllIn
	// group needs no entry: its own Case set is the union of every other
	// group's (see unionCaseIDs, bridge/measure.go).
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

	// Now reads the current time, stamped on VerdictEmittedAt. Nil uses
	// time.Now. Injected so a resume test can drive the clock.
	Now func() time.Time
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

func (p RunParams) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
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
// reconcile, then measure per groupCompletion's decision (deploy only what
// is still needed, or recompute a lost verdict from stored scores, or skip
// entirely if this group already has a durable verdict), teardown
// (unconditional whenever a deploy happened), and emit BridgeGroupMeasured
// exactly once per group.
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

	// AllIn runs first (Groups() already orders it first), so every
	// leave-one-out group below can read its scores back from the store —
	// never from an in-memory value, which would not survive a resume.
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

		// Reload: ReconcileTerminal may have updated the row (State,
		// Status, ActualCostUSDMicros) since findRecord above.
		rec, err = findRecord(ctx, p.Store, p.RunID, q.Group)
		if err != nil {
			return result, err
		}

		ev, err := measureGroup(ctx, p, q, ref, rec, limiter)
		if err != nil {
			return result, err
		}
		if ev != nil {
			result.Measured = append(result.Measured, ev)
		}
	}
	return result, nil
}

// measureGroup is one group's measurement, dispatched by
// groupCompletion's read of the durable state — the bridge eval-seam
// plan's §6 resume idempotency: never re-deploy a group that is already
// fully scored and reported, never lose a paid-for verdict whose event
// never made it to the stream, and only ever pay for the Cases still
// missing when partial progress exists.
func measureGroup(
	ctx context.Context, p RunParams, q GroupQuote, ref *core.JobRef,
	rec *store.TuningJobRecord, limiter *LiveEndpointLimiter,
) (*knov1.BridgeGroupMeasured, error) {
	needed := groupNeededCaseIDs(p, q.Group)
	have, err := loadGroupScores(ctx, p.Store, p.RunID, q.Group)
	if err != nil {
		return nil, err
	}

	switch {
	case q.Group == AllIn:
		if err := ensureMeasured(ctx, p, q, ref, limiter, needed, have); err != nil {
			return nil, err
		}
		// The all-in group IS the baseline every leave-one-out group is
		// compared against; it has no delta or verdict of its own.
		return nil, nil

	case rec.VerdictEmittedAt != "":
		// Already reported in a prior process (or earlier in this one, for
		// a duplicate Quotes entry — Groups() never produces one, but
		// nothing enforces that on RunParams directly). Never re-measure,
		// never re-emit; recompute the event from durable state purely so
		// THIS process's RunResult is complete.
		return recomputeVerdict(ctx, p, q.Group)

	default:
		if err := ensureMeasured(ctx, p, q, ref, limiter, needed, have); err != nil {
			return nil, err
		}
		return emitVerdict(ctx, p, q.Group, rec)
	}
}

// ensureMeasured deploys and measures only the Cases group still needs,
// when it needs any — a resumed group whose Cases are already all durably
// scored costs nothing here, per §6: "A resumed run re-deploys only if
// Cases remain unscored, and re-scores only those".
func ensureMeasured(
	ctx context.Context, p RunParams, q GroupQuote, ref *core.JobRef,
	limiter *LiveEndpointLimiter, needed []string, have map[string]float64,
) error {
	missing := missingIDs(needed, have)
	if len(missing) == 0 {
		return nil
	}
	return deployAndMeasure(ctx, p, q, ref, limiter, missing)
}

// deployAndMeasure deploys a succeeded job's model, invokes it over
// missing while settling hosting minutes forward, persists every score it
// gets back, and tears the endpoint down UNCONDITIONALLY — success or
// failure — before returning. Teardown's own failure is never swallowed:
// it fails this call, per core.Tuner.Teardown's contract.
func deployAndMeasure(
	ctx context.Context, p RunParams, q GroupQuote, ref *core.JobRef,
	limiter *LiveEndpointLimiter, missing []string,
) (retErr error) {
	if err := limiter.Acquire(ctx); err != nil {
		return fmt.Errorf("acquiring a live-endpoint slot for %s: %w", q.Group, err)
	}
	defer limiter.Release()

	ep, err := DeployGroup(ctx, DeployParams{
		RunID: p.RunID, AblationGroup: q.Group, Store: p.Store, Tuner: p.Tuner,
		Emitter: p.Emitter, Ref: ref,
	})
	if err != nil {
		return err
	}

	// Teardown is unconditional and defer-shaped, per the plan's Step
	// 2(f): it runs on success, on eval-pass failure, on the serve-minute
	// cap, and on cancellation. Its own failure is never swallowed — it
	// replaces whatever error this function was about to return with its
	// own, naming the endpoint, because a leaked endpoint outranks a
	// measurement error in urgency.
	defer func() {
		tdErr := TeardownGroup(ctx, TeardownParams{
			RunID: p.RunID, AblationGroup: q.Group, Store: p.Store, Tuner: p.Tuner,
			Emitter: p.Emitter, Endpoint: ep,
		})
		if tdErr != nil {
			retErr = tdErr
		}
	}()

	model := &knov1.AgentRef{Ref: ep.Served, Scheme: ep.Provider, Target: ep.Served}

	// measureCtx, not ctx: a hosting tick that the budget guard REFUSES has
	// to reach the measurement and stop it. The endpoint bills by the
	// minute whether or not anything is measuring it, so a cap reached
	// mid-measure means every further minute is spend the user did not
	// authorize. The ticker cancels this context on refusal, Measure
	// returns, and the deferred teardown above runs immediately instead of
	// after a measurement nobody can pay for.
	//
	// The ticker itself keeps the parent ctx: it must go on settling the
	// minutes actually consumed between the refusal and teardown. Those
	// are real charges, and recording them as orphan spend is the honest
	// treatment — cancelling the ticker too would simply lose them.
	measureCtx, cancelMeasure := context.WithCancel(ctx)
	defer cancelMeasure()

	stopTick := startServeTicker(ctx, cancelMeasure, p, q.Group, ep)
	defer func() {
		if err := stopTick(); err != nil && retErr == nil {
			retErr = err
		}
	}()

	scores, mErr := p.Eval.Measure(measureCtx, q.Group, model, missing)
	// Persist whatever was scored BEFORE returning any measurement error —
	// a partial pass that scored 40 of 60 Cases before a refusal or a
	// transport failure must not lose those 40. This is bridge.Run's own
	// durability safety net; a production EvalRunner built on
	// core.ScorePass persists each Case as it happens too (ScoreParams.OnScored),
	// so this write is typically idempotent by the time it runs — see
	// recordScores's doc.
	devSet := make(map[string]struct{}, len(p.DevCaseIDs[q.Group]))
	if q.Group != AllIn {
		for _, id := range p.DevCaseIDs[q.Group] {
			devSet[id] = struct{}{}
		}
	}
	armOf := func(id string) store.Arm {
		if q.Group == AllIn {
			// The all-in scores are the baseline itself, not either side
			// of a paired comparison — every row is ArmTreatment
			// uniformly. See armFor's doc.
			return store.ArmTreatment
		}
		return armFor(id, devSet)
	}
	if err := recordScores(ctx, p.Store, p.RunID, q.Group, scores, armOf); err != nil {
		if retErr == nil {
			retErr = err
		}
		return retErr
	}
	if mErr != nil {
		retErr = fmt.Errorf("measuring the %s group: %w", q.Group, mErr)
		return retErr
	}
	return nil
}

// emitVerdict computes a leave-one-out group's verdict from durably
// recorded scores, appends BridgeGroupMeasured, and marks the group
// reported — in that order, because losing a measurement is worse than
// duplicating one: a crash between the two leaves the group "fully scored,
// not yet marked", which resume's default branch in measureGroup recomputes
// and emits again rather than silently dropping. See
// docs/plans/2026-09-01-bridge-eval-seam.md §6.
func emitVerdict(ctx context.Context, p RunParams, group string, rec *store.TuningJobRecord) (*knov1.BridgeGroupMeasured, error) {
	ev, err := recomputeVerdict(ctx, p, group)
	if err != nil {
		return nil, err
	}
	if err := p.Emitter.GroupMeasured(ctx, ev); err != nil {
		return nil, err
	}
	rec.VerdictEmittedAt = p.now().Format(time.RFC3339)
	if err := p.Store.UpdateTuningJob(ctx, p.RunID, rec); err != nil {
		return nil, fmt.Errorf("marking the %s group's verdict emitted: %w", group, err)
	}
	return ev, nil
}

// recomputeVerdict rebuilds a leave-one-out group's BridgeGroupMeasured
// purely from durable state — the store's recorded per-Case scores for
// this group and for the all-in baseline. Used both by emitVerdict (a
// fresh measurement) and by measureGroup's already-reported branch (a
// resumed process reconstructing what a prior one already emitted, for
// this process's own RunResult, without re-emitting).
func recomputeVerdict(ctx context.Context, p RunParams, group string) (*knov1.BridgeGroupMeasured, error) {
	allIn, err := loadGroupScores(ctx, p.Store, p.RunID, AllIn)
	if err != nil {
		return nil, err
	}
	if len(allIn) == 0 {
		return nil, fmt.Errorf("bridge: group %s has no all-in baseline scores recorded — "+
			"the all-in job must be measured before any leave-one-out group's delta is meaningful", group)
	}
	groupScores, err := loadGroupScores(ctx, p.Store, p.RunID, group)
	if err != nil {
		return nil, err
	}
	return computeVerdict(group, p.GoalDomain, p.Level, p.NGroups,
		allIn, groupScores, p.DevCaseIDs[group], p.ControlCaseIDs), nil
}

// groupNeededCaseIDs is the Case ID set one group's job must be measured
// over: the union of every group's dev Cases plus the control partition
// for AllIn (the union pass, §2), or this group's own dev Cases plus the
// control partition for a leave-one-out group (groupCaseIDs,
// bridge/measure.go).
func groupNeededCaseIDs(p RunParams, group string) []string {
	if group == AllIn {
		return unionCaseIDs(p)
	}
	return groupCaseIDs(p, group)
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

// startServeTicker starts a background goroutine settling hosting minutes
// forward at p.TickInterval and returns a function that stops it, waits for
// the final tick, and reports the first settle error observed.
//
// The error is RETURNED rather than discarded, and onRefusal is called when
// one occurs, because the failure that matters here is the budget guard
// refusing: the endpoint bills by the minute regardless of whether anything
// is measuring it, so a cap reached mid-hosting means every subsequent
// minute is unauthorized spend. Swallowing the error left the endpoint
// running to the end of a measurement nobody could pay for — prime
// directive 4, and invisible because `_, _ =` is an explicit discard that
// errcheck accepts.
//
// The caller must call the stop function before returning so the goroutine
// never outlives the endpoint it is billing for.
func startServeTicker(
	ctx context.Context,
	onRefusal context.CancelFunc,
	p RunParams,
	group string,
	ep *core.Endpoint,
) func() error {
	done := make(chan struct{})
	finished := make(chan struct{})

	var mu sync.Mutex
	var firstErr error

	settle := func() error {
		_, err := SettleServeTick(ctx, SettleServeParams{
			RunID: p.RunID, AblationGroup: group, Store: p.Store, Guard: p.Guard,
			Price: p.ServePrice, Replicas: p.ServeReplicas, ReadyAt: ep.ReadyAt,
		})
		if err == nil {
			return nil
		}
		mu.Lock()
		if firstErr == nil {
			firstErr = fmt.Errorf("settling hosting minutes for the %s group: %w", group, err)
		}
		mu.Unlock()
		return err
	}

	go func() {
		defer close(finished)
		ticker := time.NewTicker(p.TickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := settle(); err != nil {
					// Stop the measurement now. Teardown is the caller's
					// deferred responsibility and runs as soon as Measure
					// returns.
					onRefusal()
				}
			case <-done:
				// A final tick on the way out, so the last partial minute
				// before Teardown is not lost. No onRefusal here: the run is
				// already unwinding, and the charge is recorded either way.
				_ = settle()
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	return func() error {
		close(done)
		<-finished
		mu.Lock()
		defer mu.Unlock()
		return firstErr
	}
}

package core

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
	"google.golang.org/protobuf/proto"
)

// measureOutcome is one measurement's terminal result, shaped like
// Baseline's caseOutcome for the same reason: one shape for "scored" and
// "errored", so the sink cannot file a measurement on both sides of the
// delta's denominator.
type measureOutcome struct {
	// Response is what the agent returned. Nil when the call failed before
	// producing one.
	Response *Response

	// Score is the Goal's judgement. Nil when the measurement errored.
	Score *Score

	// Err is the terminal failure's code, empty when the measurement scored.
	// A budget refusal persists as a done-marker with this set rather than as
	// a missing row — the alternative made resume re-attempt and re-pay.
	Err string

	// Attempts, BilledUSDMicros and SettledCalls survive the panic path so a
	// recovered panic cannot take the money with it.
	Attempts        int
	BilledUSDMicros int64
	SettledCalls    int64
}

// Model reports which model answered, for the mid-run gate — the one field
// both stages' outcome types share with it.
func (o *measureOutcome) Model() string {
	if o == nil || o.Response == nil {
		return ""
	}
	return o.Response.GetResolvedModel()
}

// valueCounts accumulates the run-level counters. The sink runs on one
// goroutine per executor invocation, and a Value run invokes the executor
// once per (Asset, arm, trial) — so the counts are atomic.
type valueCounts struct {
	attempted atomic.Int32
	scored    atomic.Int32
	errored   atomic.Int32

	// models collects the resolved models the run actually measured with, so
	// CaseExecution carries what a mid-run model gate and a reader need. The
	// sink is the only writer and it is single-goroutine per executor pass,
	// but passes are sequential only within an arm — the mutex keeps the
	// arm-to-arm handoff honest.
	modelMu sync.Mutex
	seen    map[string]bool
}

// recordModel remembers one resolved model.
func (c *valueCounts) recordModel(m string) {
	if m == "" {
		return
	}
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	if c.seen == nil {
		c.seen = make(map[string]bool)
	}
	c.seen[m] = true
}

// models returns the sorted set of resolved models.
func (c *valueCounts) models() []string {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	out := make([]string, 0, len(c.seen))
	for m := range c.seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// valueEmitter serializes event writes with the same discipline Baseline's
// aggregator enforces: sequence order and insertion order are the same, and
// a hot-path write failure is remembered rather than returned — an
// observability failure must not destroy a paid measurement.
type valueEmitter struct {
	mu     sync.Mutex
	seq    int64
	closed bool

	// emitFailure holds the first hot-path event-write failure, surfaced at
	// close where it ends the run without destroying what the run bought.
	emitFailure atomic.Pointer[error]
}

// append writes one event under the lock, stamped with the run ID and the
// next sequence. A hot-path caller (the invoker hooks) may discard the error;
// the loop's own emits treat it as fatal.
func (o ValueOptions) append(ctx context.Context, em *valueEmitter, build func() *knov1.Event, what string) error {
	em.mu.Lock()
	defer em.mu.Unlock()

	if em.closed {
		return fmt.Errorf("appending %s event: the run already emitted RunFinished, "+
			"which the schema promises is the last event", what)
	}
	ev := build()
	ev.RunId = o.RunID
	ev.EmittedAt = time.Now().Format(time.RFC3339)
	ev.Sequence = em.next()
	if err := o.Store.AppendEvent(ctx, ev); err != nil {
		return fmt.Errorf("appending %s event: %w", what, err)
	}
	if _, done := ev.GetPayload().(*knov1.Event_RunFinished); done {
		em.closed = true
	}
	return nil
}

// next hands out the next sequence under the append lock.
func (em *valueEmitter) next() int64 {
	em.seq++
	return em.seq
}

// recordEmitFailure keeps the first hot-path event-write failure.
func (em *valueEmitter) recordEmitFailure(err error) {
	if err != nil {
		em.emitFailure.CompareAndSwap(nil, &err)
	}
}

// Value executes the stage: route each Asset, measure it with the invoker per
// arm — the treatment arm carries the Asset, the control arm does not — and
// write one Valuation per Asset computed from what is DURABLY recorded.
//
// The invoker is shared with Baseline (one budget-and-retry core, not two),
// and both of its hooks are wired: the money events carry the measurement
// key, so a retry or an overshoot is attributable to the (Asset, arm, trial)
// that caused it.
func (o ValueOptions) Value(ctx context.Context, pool Pool) (*ValueResult, error) {
	if err := o.validate(pool); err != nil {
		return nil, err
	}

	baseline, err := o.baselineCases(ctx)
	if err != nil {
		return nil, err
	}

	// The holdout canary is the SEAL, not a scan: caseRefs reads through
	// SealedEvals, which never yields a non-dev Case by construction. The
	// canary test drives a real run with a holdout Case planted in the source
	// and asserts it reaches neither the routing nor the store.
	cases, err := o.casesByID(ctx)
	if err != nil {
		return nil, err
	}
	refs, _, err := caseRefs(casesSeq(cases), baseline)
	if err != nil {
		return nil, err
	}

	// Deterministic Asset order, the Q12 resolution: the delta path is
	// structurally order-invariant, and a stable order is what makes the
	// completion boundary under a binding cap reproducible instead of a
	// silent selection.
	assets, err := o.sortedAssets(ctx, pool)
	if err != nil {
		return nil, err
	}

	plan, err := value.Route(refs, assetRefs(assets), o.Routing)
	if err != nil {
		return nil, err
	}

	// The consent's enforcement: the schedule this loop will execute must be
	// no larger than the figure the user was quoted. A quote that under-states
	// is a consent prompt for a smaller run than the one that happens.
	scheduled := 0
	for i := range plan.Routed {
		scheduled += len(measurementsFor(plan.Routed[i], plan, plan.Routed[i].AssetID))
	}
	if err := assertQuoteBounds(scheduled, plan); err != nil {
		return nil, err
	}

	run, completed, err := o.openRun(ctx, plan)
	if err != nil {
		return nil, err
	}
	result := &ValueResult{
		RunID:         o.RunID,
		Valuations:    make([]*Valuation, 0, len(plan.Routed)),
		Plan:          plan,
		GoalDirection: o.Goal.Direction(),
	}

	em := &valueEmitter{}
	if !o.Resume {
		if err := o.emitRunStarted(ctx, em, plan, scheduled); err != nil {
			return nil, err
		}
	} else if err := o.emitRunResumed(ctx, em, plan, completed); err != nil {
		return nil, err
	}

	var stopReason atomic.Int32
	counts := &valueCounts{}
	// The mid-run model gate, armed from the RESUMED run's record: a provider
	// alias re-pointing mid-run changes what every later delta measures, and
	// a Value run's wall-clock is a multiple of Baseline's. The first process
	// has nothing recorded to arm from — the inert gate costs nothing — and
	// its close records the models for the resume to check.
	gate := newModelGate(run)
	for _, routing := range plan.Routed {
		if ctx.Err() != nil {
			stopReason.Store(int32(knov1.RunStatus_RUN_STATUS_INTERRUPTED))
			break
		}
		if err := o.measureAsset(ctx, em, gate, routing, plan, baseline, cases, completed, result, counts, &stopReason); err != nil {
			return nil, err
		}
		if stopReason.Load() != 0 {
			break
		}
	}

	status := knov1.RunStatus(stopReason.Load())
	if status == knov1.RunStatus_RUN_STATUS_UNSPECIFIED {
		status = knov1.RunStatus_RUN_STATUS_COMPLETED
	}
	result.Status = status
	// The result comes back even when the close fails: the run it describes
	// is resumable, and discarding it with the error is how a paid run gets
	// thrown away because its last event did not write. The close itself runs
	// detached, so the cancellation that stopped the run cannot also cancel
	// the record of how it stopped.
	closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), progressWriteGrace)
	defer cancelClose()
	if err := o.finishRun(closeCtx, em, run, status, plan, counts); err != nil {
		return result, err
	}
	if f := em.emitFailure.Load(); f != nil {
		return result, fmt.Errorf("an event write failed mid-run: %w", *f)
	}
	return result, nil
}

// Quote computes the routing plan a Value run would execute under, without
// spending: the figure the user consents to before the first authorize. The
// run re-computes the plan itself — deterministic under the same seed — and
// asserts the schedule against it, so the quoted number is a bound on the
// run by construction rather than by trust.
func (o ValueOptions) Quote(ctx context.Context, pool Pool) (*value.Plan, error) {
	if err := o.validate(pool); err != nil {
		return nil, err
	}
	baseline, err := o.baselineCases(ctx)
	if err != nil {
		return nil, err
	}
	cases, err := o.casesByID(ctx)
	if err != nil {
		return nil, err
	}
	refs, _, err := caseRefs(casesSeq(cases), baseline)
	if err != nil {
		return nil, err
	}
	assets, err := o.sortedAssets(ctx, pool)
	if err != nil {
		return nil, err
	}
	plan, err := value.Route(refs, assetRefs(assets), o.Routing)
	if err != nil {
		return nil, err
	}
	scheduled := 0
	for i := range plan.Routed {
		scheduled += len(measurementsFor(plan.Routed[i], plan, plan.Routed[i].AssetID))
	}
	if err := assertQuoteBounds(scheduled, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// casesSeq adapts a materialized slice to the iterator shape caseRefs reads.
func casesSeq(cases []*Case) func(yield func(*Case, error) bool) {
	return func(yield func(*Case, error) bool) {
		for _, c := range cases {
			if !yield(c, nil) {
				return
			}
		}
	}
}

// sortedAssets reads the pool once and orders it deterministically.
func (o ValueOptions) sortedAssets(ctx context.Context, pool Pool) ([]*Asset, error) {
	all, err := pool.Assets(ctx)
	if err != nil {
		return nil, err
	}
	var assets []*Asset
	for a, err := range all {
		if err != nil {
			return nil, fmt.Errorf("reading the pool: %w", err)
		}
		assets = append(assets, a)
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].GetId() < assets[j].GetId() })
	return assets, nil
}

// assetRefs projects Assets to the router's view: an ID and tags, nothing the
// Asset carries besides.
func assetRefs(assets []*Asset) []value.AssetRef {
	refs := make([]value.AssetRef, len(assets))
	for i, a := range assets {
		refs[i] = value.AssetRef{ID: a.GetId(), Tags: a.GetTags()}
	}
	return refs
}

// casesByID materializes the dev split once, so every arm and every Asset
// measures the SAME Cases. Cloned per the iterator's borrow rule: the yielded
// value is valid for one iteration only.
func (o ValueOptions) casesByID(ctx context.Context) ([]*Case, error) {
	var out []*Case
	seq, err := o.Evals.Cases(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the dev split: %w", err)
	}
	for c, err := range seq {
		if err != nil {
			return nil, fmt.Errorf("reading the dev split: %w", err)
		}
		out = append(out, proto.Clone(c).(*Case))
	}
	return out, nil
}

// openRun loads the resume checkpoint or creates the run row.
//
// Resume consumes the RECORDED Plan rather than re-running routing: a drifted
// ControlReserve, seed, or Asset set would produce measurements the recorded
// rows cannot pair against, and the fingerprint refuses the mismatch.
func (o ValueOptions) openRun(ctx context.Context, plan *value.Plan) (*knov1.Run, map[store.MeasurementKey]struct{}, error) {
	if !o.Resume {
		run := &knov1.Run{
			Id:               o.RunID,
			Stage:            knov1.Stage_STAGE_VALUE,
			CreatedAt:        time.Now().Format(time.RFC3339),
			Agent:            o.AgentRef,
			GoalName:         o.GoalName,
			GoalDirection:    o.Goal.Direction(),
			Status:           knov1.RunStatus_RUN_STATUS_RUNNING,
			InputFingerprint: o.InputFingerprint,
			GoalScoreDomain:  o.Goal.Domain(),
			SamplingSeed:     proto.Int64(plan.Seed),
			BaselineRunId:    o.BaselineRunID,
			DevCaseCount:     int32(plan.EligibleCases), //nolint:gosec // bounded by the eval set
		}
		if err := o.Store.CreateRun(ctx, run); err != nil {
			return nil, nil, fmt.Errorf("creating the run: %w", err)
		}
		return run, nil, nil
	}

	run, err := o.Store.GetRun(ctx, o.RunID)
	if err != nil {
		return nil, nil, fmt.Errorf("loading the run to resume: %w", err)
	}
	if run.GetInputFingerprint() != o.InputFingerprint {
		return nil, nil, errs.ErrInvalidInput.
			WithFix("resume the same run with the same evals, goal, and agent").
			Wrap(fmt.Errorf("the checkpoint's inputs do not match this configuration"))
	}
	// The baseline IS the reference. Resuming against a different one would
	// silently re-pair every recorded score against a different mean, mixing
	// two baselines into one delta.
	if run.GetBaselineRunId() != "" && run.GetBaselineRunId() != o.BaselineRunID {
		return nil, nil, errs.ErrInvalidInput.
			WithFix("resume against the same baseline run this run recorded").
			Wrap(fmt.Errorf("the checkpoint was measured against baseline %s, not %s",
				run.GetBaselineRunId(), o.BaselineRunID))
	}
	if len(run.GetValuePlan()) > 0 {
		var recorded value.Plan
		if err := gob.NewDecoder(bytes.NewReader(run.GetValuePlan())).Decode(&recorded); err != nil {
			return nil, nil, fmt.Errorf("the recorded plan cannot be decoded: %w", err)
		}
		if !equalPlans(&recorded, plan) {
			return nil, nil, errs.ErrInvalidInput.
				WithFix("resume with the same routing configuration: --seed, --sample-rate, " +
					"--control-sample-rate, --control-reserve, --route, and the same pool").
				Wrap(fmt.Errorf("the checkpoint's routing plan does not match this configuration; " +
					"continuing would pair new measurements against rows recorded under a different plan"))
		}
	}
	completed, err := o.Store.CompletedMeasurements(ctx, o.RunID)
	if err != nil {
		return nil, nil, fmt.Errorf("reading completed measurements: %w", err)
	}
	// The guard is in-memory. Without this a resumed run believes it has
	// spent nothing and can consume its cap a second time — a $10 consent
	// authorizing $18 of work. The store is the only thing that outlives the
	// process, and SettledSpend is what the first process actually settled.
	restored, err := o.Store.SettledSpend(ctx, o.RunID)
	if err != nil {
		return nil, nil, fmt.Errorf("loading prior spend: %w", err)
	}
	o.Guard.Restore(restored)
	return run, completed, nil
}

// equalPlans compares the schedule-bearing fields. reflect.DeepEqual would
// also work today, but this comparison is the resume consent check and it
// should name its terms rather than inherit whatever a struct edit adds.
func equalPlans(a, b *value.Plan) bool {
	if a.Mode != b.Mode || a.Trials != b.Trials || a.Seed != b.Seed ||
		a.EligibleCases != b.EligibleCases ||
		!slicesEqual(a.ControlCaseIDs, b.ControlCaseIDs) ||
		len(a.Routed) != len(b.Routed) {
		return false
	}
	for i := range a.Routed {
		x, y := a.Routed[i], b.Routed[i]
		if x.AssetID != y.AssetID || x.FreshControlArm != y.FreshControlArm ||
			x.NotMeasuredReason != y.NotMeasuredReason ||
			!slicesEqual(x.CaseIDs, y.CaseIDs) {
			return false
		}
	}
	return true
}

// measureAsset drives one Asset's arms through the executor and writes its
// Valuation when every surviving measurement is durable.
func (o ValueOptions) measureAsset(
	ctx context.Context,
	em *valueEmitter,
	gate *modelGate,
	routing value.AssetRouting,
	plan *value.Plan,
	baseline map[string]store.CaseScore,
	cases []*Case,
	completed map[store.MeasurementKey]struct{},
	result *ValueResult,
	counts *valueCounts,
	stopReason *atomic.Int32,
) error {
	// A zero-routed Asset costs nothing and its Valuation is the passthrough
	// reason — a real answer, not an omission.
	if len(routing.CaseIDs) == 0 {
		if err := o.emitAssetRouted(ctx, em, routing, plan); err != nil {
			return err
		}
		v, err := o.valuationFor(ctx, &Asset{Id: routing.AssetID}, routing, plan, baseline)
		if err != nil {
			return err
		}
		if err := o.Store.WriteValuation(ctx, o.RunID, v); err != nil {
			return fmt.Errorf("writing the Valuation for %s: %w", routing.AssetID, err)
		}
		result.Valuations = append(result.Valuations, v)
		emitCtx, cancelEmit := detached(ctx)
		em.recordEmitFailure(o.emitAssetValued(emitCtx, em, v))
		cancelEmit()
		return nil
	}

	if err := o.emitAssetRouted(ctx, em, routing, plan); err != nil {
		return err
	}

	// The capability check runs on the WRAPPER — the agent that actually
	// runs — not the receiver. The receiver passes validate()'s check; this
	// asserts the wrapped agent declares what it does, before any spend on
	// this Asset.
	treatment, err := o.Agent.(ContextInjector).WithContext(&Asset{Id: routing.AssetID})
	if err != nil {
		return fmt.Errorf("building the treatment arm for %s: %w", routing.AssetID, err)
	}
	if c, ok := treatment.(Capable); ok && !c.Capabilities().GetContextInject() {
		return errs.ErrCapabilityUnsupported.
			WithFix("measure with an adapter whose wrapped agent declares context injection").
			Wrap(fmt.Errorf("value: the wrapped agent for %s declares context_inject "+
				"false — an adapter that answered anyway would measure the Case "+
				"without the Asset in both arms", routing.AssetID))
	}

	arms := []struct {
		arm   store.Arm
		agent Agent
		ids   []string
	}{
		{arm: store.ArmTreatment, agent: treatment, ids: routing.CaseIDs},
	}
	if routing.FreshControlArm {
		arms = append(arms, struct {
			arm   store.Arm
			agent Agent
			ids   []string
		}{arm: store.ArmControl, agent: o.Agent, ids: routing.CaseIDs})
	}
	// The harm test rides the treatment arm over the reserved partition; its
	// control is the recorded baseline, which is valid because the
	// reservation happened before routing and saw no outcome.
	arms = append(arms, struct {
		arm   store.Arm
		agent Agent
		ids   []string
	}{arm: store.ArmTreatment, agent: treatment, ids: plan.ControlCaseIDs})

	truncated := false
	for _, arm := range arms {
		if len(arm.ids) == 0 {
			continue
		}
		if err := o.measureArm(ctx, em, gate, arm.arm, arm.agent, arm.ids, routing.AssetID, plan, cases, completed, counts, stopReason); err != nil {
			return err
		}
		if stopReason.Load() != 0 {
			truncated = true
			break
		}
	}

	asset := &Asset{Id: routing.AssetID}
	if truncated {
		// Q12's truncation marker: a binding cap cut this Asset mid-measurement,
		// and presenting the partial set as a Valuation would rank "whatever got
		// measured first" as the answer. The durable rows stay — a resume
		// continues from them — and the Valuation says why it is absent.
		//
		// Written under a DETACHED context: the cancellation that caused the
		// stop must not also cancel the record of the stop, or a Ctrl-C run
		// leaves no explanation of where it ended.
		closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), progressWriteGrace)
		defer cancelClose()
		v := &Valuation{
			AssetId:     asset.GetId(),
			NotMeasured: knov1.RejectionReason_REJECTION_REASON_BUDGET_EXHAUSTED,
		}
		if err := o.Store.WriteValuation(closeCtx, o.RunID, v); err != nil {
			return fmt.Errorf("writing the truncated Valuation for %s: %w", routing.AssetID, err)
		}
		result.Valuations = append(result.Valuations, v)
		emitCtx, cancelEmit := detached(ctx)
		em.recordEmitFailure(o.emitAssetValued(emitCtx, em, v))
		cancelEmit()
		return nil
	}

	v, err := o.valuationFor(ctx, asset, routing, plan, baseline)
	if err != nil {
		return err
	}
	if err := o.Store.WriteValuation(ctx, o.RunID, v); err != nil {
		return fmt.Errorf("writing the Valuation for %s: %w", routing.AssetID, err)
	}
	result.Valuations = append(result.Valuations, v)
	emitCtx, cancelEmit := detached(ctx)
	em.recordEmitFailure(o.emitAssetValued(emitCtx, em, v))
	cancelEmit()
	return nil
}

// detached gives a post-spend emitter the same grace the invoker hooks get:
// a cancelled run must not also cancel the record of what it paid for. The
// grace timer is the only bound — there is no early cancel, because the whole
// point is that the parent's cancellation must not reach this write.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	// The caller defers the cancel AFTER the emit, so the parent's cancellation
	// cannot reach the write and the grace timer does not outlive the call.
	return context.WithTimeout(context.WithoutCancel(ctx), progressWriteGrace)
}

// measureArm runs one (arm, trial) at a time over the arm's Case list, one
// invoker per arm. The invoker is built per arm because the treatment arm's
// agent carries the Asset and the control arm's does not — that is the entire
// measurement.
func (o ValueOptions) measureArm(
	ctx context.Context,
	em *valueEmitter,
	gate *modelGate,
	arm store.Arm,
	agent Agent,
	caseIDs []string,
	assetID string,
	plan *value.Plan,
	cases []*Case,
	completed map[store.MeasurementKey]struct{},
	counts *valueCounts,
	stopReason *atomic.Int32,
) error {
	byID := make(map[string]*Case, len(cases))
	for _, c := range cases {
		byID[c.GetId()] = c
	}
	var armCases []*Case
	for _, id := range caseIDs {
		if c, ok := byID[id]; ok {
			armCases = append(armCases, c)
		}
	}

	for trial := int32(1); trial <= plan.Trials; trial++ {
		key := store.MeasurementKey{
			AssetID: assetID,
			Arm:     arm,
			Trial:   trial,
		}
		// Skip consulted in the producer: a completed measurement — including
		// a done-marker row — costs nothing on resume.
		iv := o.invoker(key, arm, agent, em)
		work := o.workFunc(iv, agent)
		sink := o.sinkFunc(key, counts)

		_, runErr := executor.Run(ctx, casesSlice(armCases), work, sink, executor.Options{
			// The only path from a SUCCESSFUL measurement to shutdown: a
			// re-pointed model alias is visible in an answer already paid for,
			// and continuing would measure the rest of the Asset against a
			// different model than the one the run recorded.
			AfterRecord: gate.afterRecord,
			Concurrency: o.Concurrency,
			ID:          func(item any) string { c, _ := item.(*Case); return c.GetId() },
			Skip: func(id string) bool {
				k := key
				k.CaseID = id
				_, done := completed[k]
				return done
			},
			// A budget stop ends the run rather than failing every remaining
			// measurement one at a time — the same escalation Baseline makes,
			// for the same money reason.
			IsFatal: func(err error) bool {
				if errors.Is(err, errs.ErrBudgetExceeded) {
					stopReason.Store(int32(knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED))
					return true
				}
				return false
			},
		})
		if runErr != nil {
			// A recorded stop is not an error: the budget stop escalated
			// through IsFatal, the drain is what the stop means, and the
			// truncated Valuation is written by the caller.
			if stopReason.Load() != 0 {
				return nil
			}
			if ctx.Err() != nil {
				stopReason.Store(int32(knov1.RunStatus_RUN_STATUS_INTERRUPTED))
				return nil
			}
			return fmt.Errorf("measuring the %s arm: %w", arm, runErr)
		}
		if stopReason.Load() != 0 {
			return nil
		}
	}
	return nil
}

// casesSlice adapts the materialized Case list to the executor's iterator
// shape.
func casesSlice(cases []*Case) func(yield func(*Case, error) bool) {
	return func(yield func(*Case, error) bool) {
		for _, c := range cases {
			if !yield(c, nil) {
				return
			}
		}
	}
}

// invoker builds the shared budget-and-retry core with Value's hooks. Both
// hooks are wired — debt #77's trigger — and they emit the money events with
// the measurement key, which is what makes spend attributable to an Asset.
func (o ValueOptions) invoker(key store.MeasurementKey, _ store.Arm, agent Agent, em *valueEmitter) invoker {
	return invoker{
		Agent:        agent,
		AgentRef:     o.AgentRef,
		Guard:        o.Guard,
		Key:          key,
		MaxAttempts:  o.maxAttempts(),
		RetryBudget:  o.retryBudget(),
		RetryBackoff: o.retryBackoff(),
		OnOvershoot: func(ctx context.Context, k store.MeasurementKey, estimated, settled, overshoot int64) {
			em.recordEmitFailure(o.append(ctx, em, func() *knov1.Event {
				return &knov1.Event{
					Payload: &knov1.Event_SettlementOvershoot{
						SettlementOvershoot: &knov1.SettlementOvershoot{
							CaseId:                       k.CaseID,
							AssetId:                      valueStringPtr(k.AssetID),
							Arm:                          valueArm(k.Arm),
							Trial:                        valueTrialPtr(k.Trial),
							ReservedUsdMicros:            estimated,
							SettledUsdMicros:             settled,
							CumulativeOvershootUsdMicros: o.Guard.Overshoot(),
							DeltaUsdMicros:               overshoot,
						},
					},
				}
			}, "settlement-overshoot"))
		},
		OnRetry: func(ctx context.Context, k store.MeasurementKey, attempt int,
			reason knov1.RetryReason, wait, remaining time.Duration,
		) {
			em.recordEmitFailure(o.append(ctx, em, func() *knov1.Event {
				return &knov1.Event{
					Payload: &knov1.Event_RetryAttempted{
						RetryAttempted: &knov1.RetryAttempted{
							CaseId:                 k.CaseID,
							AssetId:                valueStringPtr(k.AssetID),
							Arm:                    valueArm(k.Arm),
							Trial:                  valueTrialPtr(k.Trial),
							AttemptOrdinal:         int32(attempt), //nolint:gosec // bounded by maxAttempts
							Reason:                 reason,
							BackoffMs:              wait.Milliseconds(),
							RetryBudgetRemainingMs: remaining.Milliseconds(),
						},
					},
				}
			}, "retry-attempted"))
		},
	}
}

// workFunc invokes one Case through the arm's invoker and scores the response.
//
// The invoker is built per (arm, trial) so the hooks carry the full
// measurement key; the work function receives it closed over.
func (o ValueOptions) workFunc(iv invoker, agent Agent) executor.WorkFunc[*Case, measureOutcome] {
	return func(ctx context.Context, c *Case) (out *measureOutcome, err error) {
		var billed, settledCalls int64
		var attempts int
		defer func() {
			if p := recover(); p != nil {
				out = &measureOutcome{
					Attempts: attempts, BilledUSDMicros: billed, SettledCalls: settledCalls,
					Err: fmt.Sprintf("panic invoking case %s: %T", c.GetId(), p),
				}
				err = errors.New(out.Err)
			}
		}()

		est, err := o.estimate(ctx, agent, c)
		if err != nil {
			return nil, err
		}

		var resp *Response
		var invokeErr error
		resp, attempts, billed, settledCalls, invokeErr = iv.withRetry(ctx, c, est)
		if invokeErr != nil {
			return &measureOutcome{
				Err: codeOf(invokeErr), Attempts: attempts,
				BilledUSDMicros: billed, SettledCalls: settledCalls,
			}, invokeErr
		}

		score, scoreErr := o.Goal.Score(ctx, c, resp)
		if scoreErr != nil {
			return &measureOutcome{
				Response: resp, Err: codeOf(scoreErr), Attempts: attempts,
				BilledUSDMicros: billed, SettledCalls: settledCalls,
			}, scoreErr
		}
		return &measureOutcome{
			Response: resp, Score: score, Attempts: attempts,
			BilledUSDMicros: billed, SettledCalls: settledCalls,
		}, nil
	}
}

// sinkFunc durably records one measurement, every time — a budget-refused
// measurement persists as a done-marker with its error code rather than as a
// missing row, because a missing row is what made resume re-attempt and
// re-pay. The marker REPLACES the orphan-spend write for measurement-level
// refusals: SettledSpend sums outcomes + measurements + orphan columns, and
// writing both would count the same refusal twice.
func (o ValueOptions) sinkFunc(key store.MeasurementKey, counts *valueCounts) executor.SinkFunc[*Case, measureOutcome] {
	return func(ctx context.Context, r executor.Result[*Case, measureOutcome]) error {
		k := key
		k.CaseID = r.Item.GetId()
		out := r.Value
		if out == nil {
			// The executor discards the value on a work error; the panic
			// guard still produces one, so this is the path where even that
			// was absent. The row is a done-marker: durable, counted, never
			// re-attempted.
			out = &measureOutcome{Err: codeOf(r.Err)}
		}
		recorded := &store.Measurement{
			Key:      k,
			Response: out.Response,
			Score:    out.Score,
			Err:      out.Err,
			Spend:    budget.Spend{Calls: out.SettledCalls, CostUSDMicros: out.BilledUSDMicros},
		}
		if err := o.Store.RecordMeasurement(ctx, o.RunID, recorded); err != nil {
			return fmt.Errorf("recording measurement %s/%s (trial %d): %w", k.AssetID, k.CaseID, k.Trial, err)
		}
		// Counted AFTER the durable write, the same rule Baseline's emit
		// follows: the Run's counts can never outrun the measurements table.
		counts.attempted.Add(1)
		if recorded.Err == "" && recorded.Score != nil {
			counts.scored.Add(1)
		} else {
			counts.errored.Add(1)
		}
		if out.Response != nil {
			counts.recordModel(out.Response.GetResolvedModel())
		}
		return nil
	}
}

// estimate prices one Case through the arm's agent — the treatment arm's
// wrapper prices the injected context, the control arm prices the bare Case.
func (o ValueOptions) estimate(ctx context.Context, agent Agent, c *Case) (budget.Estimate, error) {
	e, ok := agent.(Estimator)
	if !ok {
		return budget.Estimate{Calls: 1, CostUSDMicros: o.EstCostPerCallUSDMicros}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, estimateTimeout)
	defer cancel()
	est, err := e.Estimate(ctx, c)
	capped := o.Guard.Limits().MaxCostUSDMicros > 0
	switch {
	case err != nil && capped:
		return budget.Estimate{}, errs.ErrInvalidInput.WithFix(
			"drop --max-cost-usd to run without a dollar cap, or use an agent that " +
				"can price this model",
		).Wrap(fmt.Errorf("cannot price case %s, and a cost cap cannot be enforced "+
			"against an unknown cost: %w", c.GetId(), err))
	case err != nil:
		return budget.Estimate{Calls: 1, CostUSDMicros: o.EstCostPerCallUSDMicros}, nil
	case est.CostUSDMicros <= 0 && capped:
		return budget.Estimate{}, errs.ErrInvalidInput.WithFix(
			"drop --max-cost-usd to run without a dollar cap, or use an agent that " +
				"can price this model",
		).Wrap(fmt.Errorf("cannot price case %s: the estimate is zero, which a cost "+
			"cap cannot be enforced against", c.GetId()))
	}
	return est, nil
}

// maxAttempts, retryBudget and retryBackoff resolve the retry bounds with the
// same defaults Baseline ships.
func (o ValueOptions) maxAttempts() int {
	if o.MaxAttempts > 0 {
		return o.MaxAttempts
	}
	return DefaultMaxAttempts
}

func (o ValueOptions) retryBudget() time.Duration {
	if o.RetryBudget > 0 {
		return time.Duration(o.RetryBudget)
	}
	return DefaultRetryBudget
}

func (o ValueOptions) retryBackoff() time.Duration {
	if o.RetryBackoff > 0 {
		return time.Duration(o.RetryBackoff)
	}
	return DefaultRetryBackoff
}

// emitRunStarted opens the stream with the run's identity and the shape of
// the work ahead.
func (o ValueOptions) emitRunStarted(ctx context.Context, em *valueEmitter, _ *value.Plan, scheduled int) error {
	return o.append(ctx, em, func() *knov1.Event {
		return &knov1.Event{
			Payload: &knov1.Event_RunStarted{RunStarted: &knov1.RunStarted{
				Stage:           knov1.Stage_STAGE_VALUE,
				Agent:           o.AgentRef,
				GoalName:        o.GoalName,
				GoalDirection:   o.Goal.Direction(),
				TotalCases:      int32(scheduled), //nolint:gosec // bounded by the quote, not by an int32
				GoalScoreDomain: o.Goal.Domain(),
			}},
		}
	}, "run-started")
}

// emitRunResumed opens a continuation: overall progress starts at
// already_completed, and the session denominator is what remains — the two
// coordinate systems the RunResumed godoc exists to keep separate.
func (o ValueOptions) emitRunResumed(ctx context.Context, em *valueEmitter, plan *value.Plan, completed map[store.MeasurementKey]struct{}) error {
	total := int64(plan.Measurements())
	already := int64(len(completed))
	remaining := total - already
	if remaining < 0 {
		remaining = 0
	}
	return o.append(ctx, em, func() *knov1.Event {
		return &knov1.Event{
			Payload: &knov1.Event_RunResumed{RunResumed: &knov1.RunResumed{
				AlreadyCompleted: int32(already),   //nolint:gosec // bounded by the quote
				Remaining:        int32(remaining), //nolint:gosec // bounded by the quote
			}},
		}
	}, "run-resumed")
}

// emitAssetRouted reports one Asset's routing decision before any of its
// spend.
func (o ValueOptions) emitAssetRouted(ctx context.Context, em *valueEmitter, routing value.AssetRouting, plan *value.Plan) error {
	return o.append(ctx, em, func() *knov1.Event {
		return &knov1.Event{
			Payload: &knov1.Event_AssetRouted{AssetRouted: &knov1.AssetRouted{
				AssetId:         routing.AssetID,
				NRouted:         int32(len(routing.CaseIDs)),     //nolint:gosec // bounded by the eval set
				NSampled:        int32(len(routing.CaseIDs)),     //nolint:gosec // the draw IS the routed set here
				NControl:        int32(len(plan.ControlCaseIDs)), //nolint:gosec // bounded by the eval set
				Trials:          plan.Trials,
				FreshControlArm: routing.FreshControlArm,
				NotMeasured:     routing.NotMeasuredReason,
			}},
		}
	}, "asset-routed")
}

// emitAssetValued reports a finished Valuation's headline numbers.
func (o ValueOptions) emitAssetValued(ctx context.Context, em *valueEmitter, v *Valuation) error {
	return o.append(ctx, em, func() *knov1.Event {
		ev := &knov1.AssetValued{
			AssetId:     v.GetAssetId(),
			NotMeasured: v.GetNotMeasured(),
			NPairs:      v.GetNPairs(),
			NDropped:    v.GetNDropped(),
		}
		if iv := v.GetDeltaInterval(); iv != nil {
			ev.DeltaGoal = v.GetDeltaGoal()
			ev.DeltaInterval = iv
		}
		// Zero when the stage does not compute cost yet; carried verbatim so
		// the field's meaning does not fork into an "absent vs zero" branch
		// nothing reads.
		ev.MeasurementCostUsdMicros = v.GetMeasurementCostUsdMicros()
		return &knov1.Event{Payload: &knov1.Event_AssetValued{AssetValued: ev}}
	}, "asset-valued")
}

// finishRun closes the run: records trials and the serialized Plan, marks the
// status and counts, and writes RunFinished last.
func (o ValueOptions) finishRun(
	ctx context.Context,
	em *valueEmitter,
	run *knov1.Run,
	status knov1.RunStatus,
	plan *value.Plan,
	counts *valueCounts,
) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(plan); err != nil {
		return fmt.Errorf("encoding the plan: %w", err)
	}
	trials := plan.Trials
	run.Trials = &trials
	run.ValuePlan = buf.Bytes()
	run.Status = status
	run.FinishedAt = proto.String(time.Now().Format(time.RFC3339))
	// CaseExecution is aggregated from what is DURABLE, not from this
	// process's in-memory counters: a resumed run's close must report the
	// WHOLE run — the first process's paid rows included — never the tail.
	attempted, scored, errored, err := o.Store.MeasurementCounts(ctx, o.RunID)
	if err != nil {
		return fmt.Errorf("counting the run's measurements: %w", err)
	}
	run.CaseExecution = &knov1.CaseExecution{
		AttemptedCaseCount: attempted,
		ScoredCaseCount:    scored,
		ErroredCaseCount:   errored,
		ResolvedModels:     counts.models(),
	}
	run.AttemptedCaseCount = attempted
	run.ScoredCaseCount = scored
	run.ErroredCaseCount = errored
	if err := o.Store.FinishRun(ctx, run); err != nil {
		return fmt.Errorf("closing the run: %w", err)
	}
	return o.append(ctx, em, func() *knov1.Event {
		return &knov1.Event{
			Payload: &knov1.Event_RunFinished{RunFinished: &knov1.RunFinished{
				Status:    status,
				Attempted: attempted,
				Scored:    scored,
				Errored:   errored,
			}},
		}
	}, "run-finished")
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func valueStringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func valueArm(arm store.Arm) *knov1.Arm {
	var a knov1.Arm
	switch arm {
	case store.ArmTreatment:
		a = knov1.Arm_ARM_TREATMENT
	case store.ArmControl:
		a = knov1.Arm_ARM_CONTROL
	default:
		return nil
	}
	return &a
}

func valueTrialPtr(trial int32) *int32 {
	if trial < 1 {
		return nil
	}
	return &trial
}

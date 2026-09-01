package core

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/executor"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
	"google.golang.org/protobuf/proto"
)

// Validate executes the stage: open the holdout, record that it was opened,
// measure every holdout Case in two arms — one carrying the whole Portfolio,
// one carrying nothing — and write the paired difference with its interval.
//
// It is the only stage permitted to read SPLIT_HOLDOUT Cases, and the only
// caller of openHoldout in the module. Every stage before it takes a
// *SealedEvals; this one takes a plain Evals and opens the holdout itself,
// because the reader is unexported and cli cannot construct one.
//
// Two arms, both measured in THIS run, and the cost consequence is stated up
// front because it is the design's main price: 2 x n_holdout x trials agent
// calls. The alternative — pairing a holdout treatment score against the
// recorded dev Baseline mean — is half the price and is not an estimator of
// anything: core.Baseline takes a *SealedEvals, so no baseline run has ever
// scored a holdout Case, and the difference would carry the portfolio effect
// plus a random dev/holdout population difference plus provider drift, three
// terms in one number.
func (o ValidateOptions) Validate(ctx context.Context) (*ValidateResult, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	plan, err := o.plan(ctx)
	if err != nil {
		return nil, err
	}

	result := &ValidateResult{
		RunID:           o.RunID,
		GoalDirection:   o.Goal.Direction(),
		HoldoutCases:    len(plan.cases),
		HoldoutUseIndex: plan.useIndex,
		AssetCount:      len(plan.assets),
	}

	// A Portfolio that selected nothing this stage can measure is a complete
	// answer, not a failure. No agent call, no Run, no consumed holdout: the
	// holdout stays untouched for the Portfolio that eventually earns one.
	if plan.nothingToValidate {
		result.NothingToValidate = true
		result.Status = knov1.RunStatus_RUN_STATUS_COMPLETED
		return result, nil
	}

	// The consent's enforcement: the schedule the loop executes must be no
	// larger than the figure the user was quoted. Cheap, and it exists because
	// a quote that under-states is a consent prompt for a smaller run than the
	// one that happens — the failure core/ring0.go records at a different
	// multiple.
	scheduled := int64(len(plan.cases)) * int64(validateArms) * int64(plan.trials)
	if scheduled > plan.calls() {
		return nil, fmt.Errorf("validate: the schedule holds %d measurements against a "+
			"quote of %d; the figure the user consented to is not a bound on the run",
			scheduled, plan.calls())
	}

	// The treatment arm is built BEFORE the Run row, so an adapter that
	// refuses the set — an empty Asset, a prompt ceiling — costs no consumed
	// holdout. The receiver stays un-injected and is the control arm.
	treatment, err := o.treatmentArm(plan)
	if err != nil {
		return nil, err
	}

	run, completed, err := o.openRun(ctx, plan)
	if err != nil {
		return nil, err
	}

	em := &stageEmitter{}
	if !o.Resume {
		if err := o.emitRunStarted(ctx, em, scheduled); err != nil {
			return o.failedValidate(result, err)
		}
		if err := o.emitHoldoutOpened(ctx, em, plan); err != nil {
			return o.failedValidate(result, err)
		}
	} else if err := o.emitRunResumed(ctx, em, scheduled, completed); err != nil {
		// After openRun, and openRun is where a resume calls Guard.Restore.
		// Returning nil here would throw away the previous session's settled
		// spend along with the result.
		return o.failedValidate(result, err)
	}

	var stopReason atomic.Int32
	counts := &valueCounts{}
	gate := newModelGate(run)

	arms := []struct {
		arm   store.Arm
		agent Agent
	}{
		{arm: store.ArmControl, agent: o.Agent},
		{arm: store.ArmTreatment, agent: treatment},
	}
	for _, arm := range arms {
		if ctx.Err() != nil {
			stopReason.Store(int32(knov1.RunStatus_RUN_STATUS_INTERRUPTED))
			break
		}
		if err := o.measureHoldoutArm(ctx, em, gate, arm.arm, arm.agent, plan, completed, counts, &stopReason); err != nil {
			// The result travels with the error: measurements that settled
			// before this point were paid for, and a caller handed only an
			// error cannot report them.
			return o.failedValidate(result, err)
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
	// Read before the close, so a close that fails still reports the run's
	// real cost.
	result.Spent = o.Guard.Spent()

	closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), progressWriteGrace)
	defer cancelClose()

	if status == knov1.RunStatus_RUN_STATUS_COMPLETED {
		v, err := o.validationFor(ctx, plan)
		if err != nil {
			return o.failedValidate(result, err)
		}
		if err := o.Store.WriteValidation(ctx, o.RunID, v); err != nil {
			return o.failedValidate(result, fmt.Errorf("writing the Validation: %w", err))
		}
		result.Validation = v
		emitCtx, cancelEmit := detached(ctx)
		em.recordEmitFailure(o.emitPortfolioValidated(emitCtx, em, v))
		cancelEmit()
	}
	// A run that stopped early writes NO Validation, deliberately. A partial
	// peek is not a validation: the report keeps its not-yet-validated caveat
	// and adds a line saying an attempt was made and produced no number.

	if err := o.finishRun(closeCtx, em, run, status, counts); err != nil {
		return result, err
	}
	if f := em.emitFailure.Load(); f != nil {
		return result, fmt.Errorf("an event write failed mid-run: %w", *f)
	}
	return result, nil
}

// failedValidate returns a result the caller can still report from, alongside
// the error that ended the run.
//
// Discarding the result on an error path discards the spend figure with it,
// and the money is already gone.
func (o ValidateOptions) failedValidate(result *ValidateResult, err error) (*ValidateResult, error) {
	result.Status = knov1.RunStatus_RUN_STATUS_FAILED
	result.Spent = o.Guard.Spent()
	return result, err
}

// treatmentArm builds the injected agent and re-checks the capability on the
// WRAPPER.
//
// The receiver passed validate()'s check; this asserts the agent that actually
// runs declares what it does, before any spend. An adapter whose wrapper
// silently dropped the set would measure both arms without the Portfolio and
// report the resulting noise as a verdict.
func (o ValidateOptions) treatmentArm(plan *validatePlan) (Agent, error) {
	treatment, err := o.Agent.(ContextSetInjector).WithContextSet(plan.assets)
	if err != nil {
		return nil, fmt.Errorf("building the treatment arm for the Portfolio from %s: %w",
			o.SelectRunID, err)
	}
	if c, ok := treatment.(Capable); ok && !c.Capabilities().GetContextSetInject() {
		return nil, errs.ErrCapabilityUnsupported.
			WithFix("validate against an adapter whose wrapped agent declares context set injection").
			Wrap(fmt.Errorf("validate: the wrapped agent declares context_set_inject false — " +
				"an adapter that answered anyway would measure every holdout Case " +
				"without the Portfolio in both arms"))
	}
	return treatment, nil
}

// openRun creates the Run and consumes the holdout, or loads the checkpoint.
//
// ORDER IS THE WHOLE POINT. The holdout_uses row is written immediately after
// the Run row and BEFORE the first agent call — never on first measurement and
// never at completion. A validate that crashed after measuring one Case has
// already seen part of the holdout, and a record written later would let that
// run look like it never looked.
//
// The Run row is written first because holdout_uses references it; the window
// between the two writes contains no agent call, so a crash inside it leaves a
// holdout that was genuinely never measured.
func (o ValidateOptions) openRun(ctx context.Context, plan *validatePlan) (*knov1.Run, map[store.MeasurementKey]struct{}, error) {
	if !o.Resume {
		trials := plan.trials
		run := &knov1.Run{
			Id:               o.RunID,
			Stage:            knov1.Stage_STAGE_VALIDATE,
			CreatedAt:        time.Now().Format(time.RFC3339),
			Agent:            o.AgentRef,
			GoalName:         o.GoalName,
			GoalDirection:    o.Goal.Direction(),
			Status:           knov1.RunStatus_RUN_STATUS_RUNNING,
			InputFingerprint: o.InputFingerprint,
			GoalScoreDomain:  o.Goal.Domain(),
			Trials:           &trials,
			//nolint:gosec // bounded by the eval set
			HoldoutCaseCount: int32(len(plan.cases)),
		}
		if err := o.Store.CreateRun(ctx, run); err != nil {
			return nil, nil, fmt.Errorf("creating the run: %w", err)
		}
		if err := o.Store.RecordHoldoutUse(ctx, &store.HoldoutUse{
			EvalFingerprint: o.EvalFingerprint,
			SelectRunID:     o.SelectRunID,
			ValidateRunID:   o.RunID,
			CreatedAt:       time.Now().Format(time.RFC3339),
		}); err != nil {
			return nil, nil, fmt.Errorf("recording that this Portfolio met the holdout: %w", err)
		}
		return run, nil, nil
	}

	run, err := o.Store.GetRun(ctx, o.RunID)
	if err != nil {
		return nil, nil, fmt.Errorf("loading the run to resume: %w", err)
	}
	if run.GetInputFingerprint() != o.InputFingerprint {
		return nil, nil, errs.ErrInvalidInput.
			WithFix("resume the same run with the same evals, pool, portfolio, goal and agent").
			Wrap(errors.New("the checkpoint's inputs do not match this configuration; " +
				"re-splitting would reclassify Cases, and a holdout containing " +
				"formerly-dev Cases is not a holdout"))
	}
	completed, err := o.Store.CompletedMeasurements(ctx, o.RunID)
	if err != nil {
		return nil, nil, fmt.Errorf("reading completed measurements: %w", err)
	}
	// The guard is in-memory. Without this a resumed run believes it has spent
	// nothing and can consume its cap a second time. Restore runs BEFORE any
	// Authorize, which is the ordering stats/budget requires.
	restored, err := o.Store.SettledSpend(ctx, o.RunID)
	if err != nil {
		return nil, nil, fmt.Errorf("loading prior spend: %w", err)
	}
	o.Guard.Restore(restored)
	return run, completed, nil
}

// measureHoldoutArm runs one arm over the holdout, one trial at a time.
//
// One invoker per (arm, trial), exactly as Value builds one per arm: the
// treatment arm's agent carries the Portfolio and the control arm's does not,
// and that difference is the entire measurement.
//
// The measurement key's AssetID is the SELECT RUN ID. It is unique per
// Portfolio, meaningful when read back, and it makes the measurements table's
// (run_id, asset_id, case_id, arm, trial) primary key work unchanged — no
// schema change, and the recorded row is the done-marker as everywhere else.
func (o ValidateOptions) measureHoldoutArm(
	ctx context.Context,
	em *stageEmitter,
	gate *modelGate,
	arm store.Arm,
	agent Agent,
	plan *validatePlan,
	completed map[store.MeasurementKey]struct{},
	counts *valueCounts,
	stopReason *atomic.Int32,
) error {
	for trial := int32(1); trial <= plan.trials; trial++ {
		key := store.MeasurementKey{AssetID: o.SelectRunID, Arm: arm, Trial: trial}
		iv := o.invoker(key, agent, em)
		work := o.workFunc(iv, agent)
		sink := o.sinkFunc(key, counts)

		_, runErr := executor.Run(ctx, casesSlice(plan.cases), work, sink, executor.Options{
			AfterRecord: gate.afterRecord,
			Concurrency: o.Concurrency,
			ID:          func(item any) string { c, _ := item.(*Case); return c.GetId() },
			Skip: func(id string) bool {
				k := key
				k.CaseID = id
				_, done := completed[k]
				return done
			},
			IsFatal: func(err error) bool {
				if errors.Is(err, errs.ErrBudgetExceeded) {
					stopReason.Store(int32(knov1.RunStatus_RUN_STATUS_BUDGET_STOPPED))
					return true
				}
				return false
			},
		})
		if runErr != nil {
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

// invoker builds the shared budget-and-retry core with Validate's hooks.
//
// The money events carry the measurement key, so a retry or an overshoot is
// attributable to the (arm, trial, Case) that caused it — the AssetID slot
// carries the Select run, which is what identifies the Portfolio here.
func (o ValidateOptions) invoker(key store.MeasurementKey, agent Agent, em *stageEmitter) invoker {
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

// workFunc invokes one holdout Case through the arm's invoker and scores it.
func (o ValidateOptions) workFunc(iv invoker, agent Agent) executor.WorkFunc[*Case, measureOutcome] {
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
// re-pay.
func (o ValidateOptions) sinkFunc(key store.MeasurementKey, counts *valueCounts) executor.SinkFunc[*Case, measureOutcome] {
	return func(ctx context.Context, r executor.Result[*Case, measureOutcome]) error {
		k := key
		k.CaseID = r.Item.GetId()
		out := r.Value
		if out == nil {
			out = &measureOutcome{Err: codeOf(r.Err)}
		}
		// THE ONE PLACE VALIDATE DEPARTS FROM VALUE'S SINK, and the holdout is
		// why.
		//
		// Value records EVERY terminal outcome as a done-marker, including a
		// cancelled or budget-refused one, because a missing row is what made
		// resume re-attempt and re-pay. That reasoning is about money, and it
		// holds exactly while money was spent. When a measurement was refused
		// or cancelled and the guard settled NOTHING, the marker buys no
		// protection and costs a holdout Case: the row is skipped on resume,
		// the Case can never be paired, and the headline number is quietly
		// computed over a holdout minus whatever happened to be in flight at
		// Ctrl-C. The user cannot recover it either, because the one-shot rule
		// refuses a fresh run.
		//
		// So: nothing settled AND the failure is a stop rather than a fault —
		// no row, and the resume measures the Case for the first time. A real
		// agent error still persists as a done-marker, because retrying a
		// deterministic failure forever is not recovery.
		if out.Err != "" && out.SettledCalls == 0 && out.BilledUSDMicros == 0 &&
			(out.Err == codeOf(context.Canceled) || out.Err == errs.ErrBudgetExceeded.Code) {
			return nil
		}
		recorded := &store.Measurement{
			Key:      k,
			Response: out.Response,
			Score:    out.Score,
			Err:      out.Err,
			// Tokens come from the Response, the same way Value's loop and
			// Baseline's settledSpend compute them. Dropping them here is the
			// defect docs/debt.md#137 named in the Value loop, reproduced in
			// this one: SettledSpend sums measurements.tokens, openRun seeds
			// Guard.Restore from that sum, so a resumed Validate run restored
			// zero tokens for every measurement it had already paid for and
			// under-enforced --max-tokens for the rest of the run.
			// GetPromptTokens on a nil Response returns zero, which is right
			// for an attempt that never reached a provider.
			Spend: budget.Spend{
				Calls:         out.SettledCalls,
				CostUSDMicros: out.BilledUSDMicros,
				Tokens:        out.Response.GetPromptTokens() + out.Response.GetCompletionTokens(),
			},
		}
		if err := o.Store.RecordMeasurement(ctx, o.RunID, recorded); err != nil {
			return fmt.Errorf("recording the %s measurement of %s (trial %d): %w",
				k.Arm, k.CaseID, k.Trial, err)
		}
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

// estimate prices one holdout Case through the arm's agent — the treatment
// arm's wrapper prices the injected Portfolio, the control arm prices the bare
// Case.
func (o ValidateOptions) estimate(ctx context.Context, agent Agent, c *Case) (budget.Estimate, error) {
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

// append writes one event, stamped with the run ID and the next sequence.
func (o ValidateOptions) append(ctx context.Context, em *stageEmitter, build func() *knov1.Event, what string) error {
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

// emitRunStarted opens the stream with the run's identity and the shape of the
// work ahead.
func (o ValidateOptions) emitRunStarted(ctx context.Context, em *stageEmitter, scheduled int64) error {
	return o.append(ctx, em, func() *knov1.Event {
		return &knov1.Event{
			Payload: &knov1.Event_RunStarted{RunStarted: &knov1.RunStarted{
				Stage:           knov1.Stage_STAGE_VALIDATE,
				Agent:           o.AgentRef,
				GoalName:        o.GoalName,
				GoalDirection:   o.Goal.Direction(),
				TotalCases:      int32(scheduled), //nolint:gosec // bounded by the quote
				GoalScoreDomain: o.Goal.Domain(),
			}},
		}
	}, "run-started")
}

// emitHoldoutOpened announces the peek, once, after the durable record and
// before the first agent call. Counts and ordinals only — Case content never
// reaches the event stream.
func (o ValidateOptions) emitHoldoutOpened(ctx context.Context, em *stageEmitter, plan *validatePlan) error {
	return o.append(ctx, em, func() *knov1.Event {
		return &knov1.Event{
			Payload: &knov1.Event_HoldoutOpened{HoldoutOpened: &knov1.HoldoutOpened{
				HoldoutCaseCount: int32(len(plan.cases)), //nolint:gosec // bounded by the eval set
				Underpowered:     plan.underpowered(o.MinHoldout),
				HoldoutUseIndex:  plan.useIndex,
				SelectRunId:      o.SelectRunID,
				AssetCount:       int32(len(plan.assets)), //nolint:gosec // bounded by the Portfolio
			}},
		}
	}, "holdout-opened")
}

// emitRunResumed opens a continuation.
func (o ValidateOptions) emitRunResumed(ctx context.Context, em *stageEmitter, scheduled int64, completed map[store.MeasurementKey]struct{}) error {
	already := int64(len(completed))
	remaining := scheduled - already
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

// emitPortfolioValidated reports the finished measurement's headline numbers.
func (o ValidateOptions) emitPortfolioValidated(ctx context.Context, em *stageEmitter, v *Validation) error {
	return o.append(ctx, em, func() *knov1.Event {
		ev := &knov1.PortfolioValidated{
			Verdict:           v.GetVerdict(),
			MeasuredCaseCount: v.GetMeasuredCaseCount(),
			NDropped:          v.GetNDropped(),
			NotMeasured:       v.GetNotMeasured(),
		}
		if iv := v.GetHoldoutInterval(); iv != nil {
			ev.HoldoutGain = v.GetHoldoutGain()
			ev.HoldoutInterval = iv
		}
		return &knov1.Event{Payload: &knov1.Event_PortfolioValidated{PortfolioValidated: ev}}
	}, "portfolio-validated")
}

// finishRun closes the run: marks the status and the counts aggregated from
// what is DURABLE, then writes RunFinished last.
func (o ValidateOptions) finishRun(
	ctx context.Context,
	em *stageEmitter,
	run *knov1.Run,
	status knov1.RunStatus,
	counts *valueCounts,
) error {
	run.Status = status
	run.FinishedAt = proto.String(time.Now().Format(time.RFC3339))
	// Aggregated from the store, not from this process's counters: a resumed
	// run's close must report the WHOLE run, the first process's paid rows
	// included, never the tail.
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

// maxAttempts, retryBudget and retryBackoff resolve the retry bounds with the
// same defaults Baseline and Value ship.
func (o ValidateOptions) maxAttempts() int {
	if o.MaxAttempts > 0 {
		return o.MaxAttempts
	}
	return DefaultMaxAttempts
}

func (o ValidateOptions) retryBudget() time.Duration {
	if o.RetryBudget > 0 {
		return time.Duration(o.RetryBudget)
	}
	return DefaultRetryBudget
}

func (o ValidateOptions) retryBackoff() time.Duration {
	if o.RetryBackoff > 0 {
		return time.Duration(o.RetryBackoff)
	}
	return DefaultRetryBackoff
}

package bridge

import (
	"context"
	"sync"
	"time"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
)

// Emitter serializes the bridge's event writes with a monotonic per-run
// sequence — the same discipline core's stageEmitter enforces for Value and
// Validate (see the tuner-bridge plan's Step 8: "new user-visible state is a
// new event type, never a side channel").
//
// A resumed run continues the sequence rather than restarting it: NewEmitter
// seeds from store.Store.MaxEventSequence, matching the contract
// Event.sequence's own godoc states.
type Emitter struct {
	mu    sync.Mutex
	seq   int64
	store store.Store
	runID string
}

// NewEmitter builds an Emitter seeded from the run's recorded event history,
// so a resumed run's events continue the sequence rather than colliding with
// events already recorded before the interruption.
func NewEmitter(ctx context.Context, st store.Store, runID string) (*Emitter, error) {
	last, err := st.MaxEventSequence(ctx, runID)
	if err != nil {
		return nil, err
	}
	return &Emitter{store: st, runID: runID, seq: last}, nil
}

// next reserves the next sequence number.
func (e *Emitter) next() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	return e.seq
}

// append stamps ev with this run's ID, the next sequence, and the current
// timestamp, then records it.
func (e *Emitter) append(ctx context.Context, ev *knov1.Event) error {
	ev.RunId = e.runID
	ev.Sequence = e.next()
	ev.EmittedAt = time.Now().UTC().Format(time.RFC3339)
	return e.store.AppendEvent(ctx, ev)
}

// JobSubmitted records that Tuner.Submit returned a JobRef for one group —
// emitted after the durable row is write-ahead and Submit succeeds, per
// TuningJobSubmitted's own doc. Carries the estimate, never the training
// data.
func (e *Emitter) JobSubmitted(ctx context.Context, group, provider, jobID string, baseModel *knov1.AgentRef, estimatedCostUSDMicros, trainTokens int64) error {
	return e.append(ctx, &knov1.Event{Payload: &knov1.Event_TuningJobSubmitted{
		TuningJobSubmitted: &knov1.TuningJobSubmitted{
			AblationGroup:          group,
			Provider:               provider,
			JobId:                  jobID,
			BaseModel:              baseModel,
			EstimatedCostUsdMicros: estimatedCostUSDMicros,
			TrainTokens:            trainTokens,
		},
	}})
}

// JobStateChanged records a polled job status.
func (e *Emitter) JobStateChanged(ctx context.Context, jobID, group string, state *knov1.JobState) error {
	ev := &knov1.TuningJobStateChanged{
		JobId:         jobID,
		AblationGroup: group,
		Status:        state.GetStatus(),
		Error:         state.GetError(),
	}
	if p := state.Progress; p != nil {
		ev.Progress = p
	}
	if actual := state.GetActualCostUsdMicros(); actual > 0 {
		ev.ActualCostUsdMicros = &actual
	}
	return e.append(ctx, &knov1.Event{Payload: &knov1.Event_TuningJobStateChanged{TuningJobStateChanged: ev}})
}

// GroupMeasured records one group's leave-one-group-out result. See
// BridgeGroupMeasured's doc: never carries a delta without its Interval.
func (e *Emitter) GroupMeasured(ctx context.Context, ev *knov1.BridgeGroupMeasured) error {
	return e.append(ctx, &knov1.Event{Payload: &knov1.Event_BridgeGroupMeasured{BridgeGroupMeasured: ev}})
}

// EndpointChanged records a hosted model's serving endpoint changing state —
// including, per TuningEndpointChanged's doc, the LEAKED state a failed
// Teardown produces. No endpoint URL, no credential.
func (e *Emitter) EndpointChanged(ctx context.Context, jobID, endpointID, group string, state knov1.TuningEndpointState, serveMinutes int32, serveCostUSDMicros int64, errText string) error {
	return e.append(ctx, &knov1.Event{Payload: &knov1.Event_TuningEndpointChanged{
		TuningEndpointChanged: &knov1.TuningEndpointChanged{
			JobId:              jobID,
			EndpointId:         endpointID,
			AblationGroup:      group,
			State:              state,
			ServeMinutes:       serveMinutes,
			ServeCostUsdMicros: serveCostUSDMicros,
			Error:              errText,
		},
	}})
}

// OrphanSpend records money settled outside any reservation the in-process
// Guard is tracking — an abandoned job's estimate (already durable, never
// re-recorded as spend here) or a hosting-sweep true-up. Reuses the existing
// OrphanSpend event exactly as Baseline's emitOrphanSpend does; group takes
// the case_id field's place, per bridge/reconcile.go's note that
// RecordOrphanSpend's contract is run-scoped despite its Case-shaped naming.
func (e *Emitter) OrphanSpend(ctx context.Context, group string, costUSDMicros int64, calls int64) error {
	return e.append(ctx, &knov1.Event{Payload: &knov1.Event_OrphanSpend{
		OrphanSpend: &knov1.OrphanSpend{
			CaseId:        group,
			CostUsdMicros: costUSDMicros,
			Calls:         calls,
		},
	}})
}

// SettlementOvershoot records that settled spend exceeded what was
// authorized — reused for both Step 2(c)'s cost-overrun reconciliation and
// Step 2(g)'s hosting sweep true-up.
func (e *Emitter) SettlementOvershoot(ctx context.Context, group string, reserved, settled, cumulativeOvershoot, delta int64) error {
	return e.append(ctx, &knov1.Event{Payload: &knov1.Event_SettlementOvershoot{
		SettlementOvershoot: &knov1.SettlementOvershoot{
			CaseId:                       group,
			ReservedUsdMicros:            reserved,
			SettledUsdMicros:             settled,
			CumulativeOvershootUsdMicros: cumulativeOvershoot,
			DeltaUsdMicros:               delta,
		},
	}})
}

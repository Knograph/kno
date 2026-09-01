package bridge

import (
	"context"
	"fmt"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// ReconcileTerminal handles the bridge plan's Step 2(c): what happens when a
// submitted job reaches a terminal JobState, compared against the estimate
// that was settled at submission.
//
//   - actual > estimate: the difference is spend nobody authorized. It
//     cannot go through Reservation.Settle a second time — that reservation
//     already closed at submission, and Settle's sync.Once makes a second
//     call a no-op — so it goes through store.RecordOrphanSpend plus
//     Guard.Restore, the path built for exactly this shape: money settled
//     outside any reservation the in-process Guard is tracking.
//   - actual < estimate: NO REFUND. budget's addSpend refuses a negative
//     report by design, and crediting back would let a run spend past what
//     a human consented to on the strength of a cheaper-than-feared
//     invoice. The over-estimate is recorded on the row (ActualCostUSDMicros)
//     so a report can say "estimated $X, billed $Y" — never credited.
//   - actual absent: the estimate stands, unchanged, and nothing is
//     recorded as overshoot or underrun.
//
// A NOTE ON "ABSENT": knov1.JobState.actual_cost_usd_micros is a plain
// int64, not `optional int64` (proto/kno/v1/tuner.proto:110 — see this PR's
// report), so the wire cannot distinguish "the provider never reported a
// cost" from "the provider reported exactly zero". Both map to the SAME
// treatment under Step 2(c) — "estimate stands, never zeroed, never
// guessed" — so this build reads any actual <= 0 as absent. That equivalence
// is what the plan's own text specifies for both cases, so no behavior
// described in Step 2(c) is lost; the only loss is a report being unable to
// print a genuine "billed exactly $0.00" distinctly from "no figure
// reported", which is a smaller gap than the one this comment exists to
// flag precisely rather than silently work around.
//
// Returns the delta recorded through RecordOrphanSpend, zero when none was
// recorded — for a caller that wants to emit a SettlementOvershoot-shaped
// event around this call.
func ReconcileTerminal(
	ctx context.Context,
	st store.Store,
	guard *budget.Guard,
	runID string,
	rec *store.TuningJobRecord,
	state *core.JobState,
) (overshootDelta int64, err error) {
	if rec == nil {
		return 0, fmt.Errorf("bridge: ReconcileTerminal requires a tuning job record")
	}
	actual := state.GetActualCostUsdMicros()

	rec.Status = state.GetStatus()
	rec.ErrorText = state.GetError()
	if actual > 0 {
		v := actual
		rec.ActualCostUSDMicros = &v
	}

	if actual > rec.EstimatedCostUSDMicros {
		overshootDelta = actual - rec.EstimatedCostUSDMicros
		if err := st.RecordOrphanSpend(ctx, runID, budget.Spend{CostUSDMicros: overshootDelta}); err != nil {
			return 0, fmt.Errorf("recording the tuning-job cost overshoot for %s: %w", rec.AblationGroup, err)
		}
		guard.Restore(budget.Spend{CostUSDMicros: overshootDelta})
	}
	// actual <= estimate, including actual <= 0 (read as absent): no
	// further spend is recorded. The estimate already settled at submission
	// stands as the recorded figure.

	if err := st.UpdateTuningJob(ctx, runID, rec); err != nil {
		return 0, fmt.Errorf("recording the terminal tuning job state for %s: %w", rec.AblationGroup, err)
	}
	return overshootDelta, nil
}

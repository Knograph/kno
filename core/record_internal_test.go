package core

import (
	"context"
	"strings"
	"testing"
	"time"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
)

// countingStore records how many events were appended and nothing else.
type countingStore struct {
	store.Store
	appended int
}

func (c *countingStore) AppendEvent(context.Context, *knov1.Event) error {
	c.appended++
	return nil
}

// TestNothingIsAppendedAfterRunFinished.
//
// RunFinished's payload promises it is always the last event, and until now
// nothing enforced it. M2-10c adds a ticker-driven emitter — the first one
// that can append concurrently with close — so the refusal is tested before
// the thing that can trip it exists.
//
// Internal, because the refusal lives in appendEvent and the only honest way
// to reach it is to write RunFinished and then try again. Driving it through
// a run would require the very emitter that does not exist yet.
func TestNothingIsAppendedAfterRunFinished(t *testing.T) {
	t.Parallel()

	st := &countingStore{}
	o := BaselineOptions{RunID: "run-1", Store: st, Now: func() time.Time { return time.Unix(0, 0) }}
	agg := &aggregator{}
	ctx := context.Background()

	if err := o.appendEvent(ctx, agg,
		&knov1.Event{Payload: &knov1.Event_RunFinished{RunFinished: &knov1.RunFinished{}}},
		"run-finished"); err != nil {
		t.Fatalf("writing RunFinished: %v", err)
	}
	if st.appended != 1 {
		t.Fatalf("%d events appended, want 1", st.appended)
	}

	err := o.appendEvent(ctx, agg,
		&knov1.Event{Payload: &knov1.Event_StageProgress{StageProgress: &knov1.StageProgress{}}},
		"stage-progress")
	if err == nil {
		t.Fatal("an event was appended after RunFinished; the schema promises " +
			"RunFinished is last, and a consumer cannot detect the violation")
	}
	if !strings.Contains(err.Error(), "RunFinished") {
		t.Errorf("the refusal says %q, which does not name the reason", err)
	}
	if st.appended != 1 {
		t.Errorf("%d events reached the store; the refusal must happen before the "+
			"write, not after it", st.appended)
	}
	// And it must not burn a sequence number either.
	if got := agg.next(); got != 2 {
		t.Errorf("next sequence is %d, want 2 — the refused append consumed one", got)
	}
}

// TestBaselineWiresEveryInvokerHook.
//
// The extracted budget-and-retry core takes its event emission as nil-able
// hooks, and a nil hook silently emits nothing. That is right for a stage with
// no such event and wrong as a way to discover that a stage forgot one — the
// two events in question are the ones that explain where money went, and their
// absence looks identical to a run that had nothing to report.
//
// Nil-safety stays (a panic on a money path is worse), so the enforcement is a
// test per stage. This is Baseline's. See docs/debt.md#77.
func TestBaselineWiresEveryInvokerHook(t *testing.T) {
	t.Parallel()

	iv := BaselineOptions{}.invoker(&aggregator{})
	if iv.OnOvershoot == nil {
		t.Error("Baseline wires no OnOvershoot hook, so a settlement overshoot — " +
			"money spent past its reservation — would go unreported and look " +
			"identical to a run that never overshot")
	}
	if iv.OnRetry == nil {
		t.Error("Baseline wires no OnRetry hook, so a run obeying a provider's " +
			"backoff would be indistinguishable from a hung one")
	}
}

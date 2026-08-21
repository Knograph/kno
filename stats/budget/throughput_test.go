package budget_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/knograph/kno/stats/budget"
)

// TestPessimisticReservationForfeitsHeadroom measures what a pessimistic
// reservation actually costs a run, because three separate arithmetic claims
// about this number have now failed measurement during the M2 plan's review.
//
// The forfeiture is `N x pessimistic` of headroom held back at the boundary --
// NOT `cap / pessimistic` Cases, which is the arithmetic of a guard where
// reservations accumulate. They do not: Settle releases.
//
// The hold matters. An earlier measurement settled in the same instant it
// reserved, so N reservations never overlapped, and it reported 98.2% where
// the real figure under concurrency is 95.1%. A provider call holds a
// reservation open for its whole duration; a test that does not is measuring
// something else.
//
// Asserted as a BOUND, not a fixed count: the exact number varies with
// goroutine scheduling by more than a point, and a test pinned to it would be
// flaky on arrival.
func TestPessimisticReservationForfeitsHeadroom(t *testing.T) {
	t.Parallel()

	const (
		capUSDMicros = 5_000_000 // $5.00
		pessimistic  = 32_800    // $0.0328 -- 4096 output tokens at $8/Mtok
		actual       = 2_000     // $0.002  -- what the call really cost
		hold         = 200 * time.Microsecond
	)

	for _, concurrency := range []int{1, 8, 32} {
		t.Run(name(concurrency), func(t *testing.T) {
			t.Parallel()

			g := budget.New(budget.Limits{MaxCostUSDMicros: capUSDMicros}, nil, 0)
			spendUntilDenied(g, concurrency, pessimistic, actual, hold)

			spent := g.Spent().CostUSDMicros

			// The guard denies at spent + N*pessimistic > cap, so the run
			// forfeits at most that much headroom and never exceeds the cap.
			lower := int64(capUSDMicros) - int64(concurrency)*pessimistic
			if spent > capUSDMicros {
				t.Errorf("spent %d exceeds the cap of %d", spent, capUSDMicros)
			}
			if spent < lower {
				t.Errorf("spent %d is below %d; a pessimistic reservation should "+
					"forfeit at most concurrency x pessimistic (%d) of headroom, "+
					"not strand the rest of the cap",
					spent, lower, int64(concurrency)*pessimistic)
			}
			t.Logf("concurrency %d: spent %d of %d (%.1f%%), forfeited at most %d",
				concurrency, spent, capUSDMicros,
				float64(spent)/capUSDMicros*100, int64(concurrency)*pessimistic)
		})
	}
}

func name(n int) string {
	switch n {
	case 1:
		return "serial"
	case 8:
		return "concurrency 8"
	default:
		return "concurrency 32"
	}
}

// spendUntilDenied runs workers until the guard refuses, mirroring how the
// executor drives Baseline: authorize, hold for the call, settle at actual.
func spendUntilDenied(g *budget.Guard, workers int, reserve, actual int64, hold time.Duration) {
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var once sync.Once

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				res, err := g.Authorize(context.Background(),
					budget.Estimate{Calls: 1, CostUSDMicros: reserve})
				if err != nil {
					once.Do(func() { close(stop) })
					return
				}
				time.Sleep(hold)
				res.Settle(budget.Spend{Calls: 1, CostUSDMicros: actual})
			}
		}()
	}
	wg.Wait()
}

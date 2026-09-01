package bridge

// This file is the bridge's hosting lifecycle: Deploy, the per-minute
// settle-forward tick, unconditional Teardown, the live-endpoint cap, and
// the resume-time endpoint sweep — the tuner-bridge plan's Step 2(f)/(g).
// See bridge/doc.go for the package doc.
//
// This is the SECOND spend shape the bridge introduces, distinct from
// Submit's one-time commitment: hosting bills incrementally, per replica per
// minute, including while idle, and — unlike a submitted job — CAN be
// stopped. Everything here exists because a leaked endpoint keeps billing
// after Kno exits, which docs/debt.md#156 named as the risk this file repays.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// DeployParams bundles one group's endpoint deployment.
type DeployParams struct {
	RunID, AblationGroup string
	Store                store.Store
	Tuner                core.Tuner
	Emitter              *Emitter
	Ref                  *core.JobRef

	// Now is when the deploy attempt is recorded. Injectable for tests;
	// time.Now when zero.
	Now time.Time
}

// DeployGroup calls Tuner.Deploy for one finished job's model.
//
// Write-ahead: DeployedAt is recorded on the durable row BEFORE Deploy is
// called, and EndpointID only after it returns successfully — mirroring
// SubmitGroup's write-ahead discipline for the same reason. A crash between
// Deploy's HTTP success and the EndpointID write leaves a row with
// DeployedAt set and EndpointID nil: SweepEndpoints's ListEndpoints
// fallback is what finds that endpoint on resume, since the row alone
// cannot say whether one was created.
func DeployGroup(ctx context.Context, p DeployParams) (*core.Endpoint, error) {
	rec, err := findRecord(ctx, p.Store, p.RunID, p.AblationGroup)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("bridge: DeployGroup found no tuning job row for %s", p.AblationGroup)
	}

	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	rec.DeployedAt = now.UTC().Format(time.RFC3339)
	if err := p.Store.UpdateTuningJob(ctx, p.RunID, rec); err != nil {
		return nil, fmt.Errorf("recording the deploy attempt for %s: %w", p.AblationGroup, err)
	}
	if p.Emitter != nil {
		if err := p.Emitter.EndpointChanged(ctx, rec.ProviderJobID, "", p.AblationGroup,
			knov1.TuningEndpointState_TUNING_ENDPOINT_STATE_DEPLOYING, 0, 0, ""); err != nil {
			return nil, fmt.Errorf("emitting the deploying event for %s: %w", p.AblationGroup, err)
		}
	}

	ep, err := p.Tuner.Deploy(ctx, p.Ref)
	if err != nil {
		return nil, fmt.Errorf("deploying the %s group's model: %w", p.AblationGroup, err)
	}

	id := ep.ID
	rec.EndpointID = &id
	if err := p.Store.UpdateTuningJob(ctx, p.RunID, rec); err != nil {
		return nil, fmt.Errorf("recording the deployed endpoint for %s: %w", p.AblationGroup, err)
	}
	if p.Emitter != nil {
		state := knov1.TuningEndpointState_TUNING_ENDPOINT_STATE_DEPLOYING
		if ep.Ready {
			state = knov1.TuningEndpointState_TUNING_ENDPOINT_STATE_READY
		}
		if err := p.Emitter.EndpointChanged(ctx, rec.ProviderJobID, ep.ID, p.AblationGroup, state, 0, 0, ""); err != nil {
			return nil, fmt.Errorf("emitting the endpoint-ready event for %s: %w", p.AblationGroup, err)
		}
	}
	return ep, nil
}

// SettleServeParams bundles one settle-forward tick for one group's
// endpoint.
type SettleServeParams struct {
	RunID, AblationGroup string
	Store                store.Store
	Guard                *budget.Guard
	Price                pricing.ServePrice
	Replicas             int

	// ReadyAt is when the endpoint became ready — the billing clock's zero.
	ReadyAt time.Time

	// Now is the tick's observation time. Injectable for tests.
	Now time.Time
}

// SettleServeTick settles ONE tick's worth of hosting minutes into the
// durable row and the budget guard.
//
// Computes WHOLE minutes elapsed since ReadyAt, minus minutes the row
// already carries (rec.ServeMinutes) — never a running total recomputed
// from scratch, so a caller ticking every real minute settles exactly one
// minute per call, and a caller replaying a longer gap (a resume finding a
// still-live endpoint) settles the whole gap in one call, both through the
// identical arithmetic.
//
// Authorized and settled through the SAME budget guard every other bridge
// spend uses, and settled IMMEDIATELY — deliberately NOT settle-at-
// submission like SubmitGroup: hosting is stoppable, so holding a
// pessimistic reservation across it would refuse affordable work, and a
// per-minute settle loses at most one minute of accounting to a crash. See
// the tuner-bridge plan's Step 2(f).
func SettleServeTick(ctx context.Context, p SettleServeParams) (minutesSettled int32, err error) {
	rec, err := findRecord(ctx, p.Store, p.RunID, p.AblationGroup)
	if err != nil {
		return 0, err
	}
	if rec == nil {
		return 0, fmt.Errorf("bridge: SettleServeTick found no tuning job row for %s", p.AblationGroup)
	}
	if p.ReadyAt.IsZero() {
		return 0, nil
	}
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	elapsedMinutes := int32(now.Sub(p.ReadyAt) / time.Minute) //nolint:gosec // bounded by --bridge-max-serve-minutes, which the caller enforces
	if elapsedMinutes <= rec.ServeMinutes {
		return 0, nil
	}
	delta := elapsedMinutes - rec.ServeMinutes
	cost := pricing.SettleServeMinutes(p.Price, int(delta), p.Replicas)

	res, err := p.Guard.Authorize(ctx, budget.Estimate{CostUSDMicros: cost})
	if err != nil {
		return 0, err
	}
	res.Settle(budget.Spend{CostUSDMicros: cost})

	rec.ServeMinutes = elapsedMinutes
	rec.ServeCostUSDMicros += cost
	if err := p.Store.UpdateTuningJob(ctx, p.RunID, rec); err != nil {
		return 0, fmt.Errorf("recording serve minutes for %s: %w", p.AblationGroup, err)
	}
	return delta, nil
}

// TeardownParams bundles one group's endpoint teardown.
type TeardownParams struct {
	RunID, AblationGroup string
	Store                store.Store
	Tuner                core.Tuner
	Emitter              *Emitter
	Endpoint             *core.Endpoint
}

// TeardownGroup stops a deployed endpoint's billing and records that it
// stopped.
//
// MUST be called on every exit path once DeployGroup has returned
// successfully — success, eval-pass failure, a budget or serve-minute cap,
// a timeout, or cancellation; see core.Tuner.Teardown's doc. Callers use
// this with `defer`, per the tuner-bridge plan's Step 2(f): "Teardown is
// unconditional and defer-shaped."
//
// A FAILED teardown is never swallowed: the row keeps its EndpointID with a
// nil TornDownAt (exactly the shape kno doctor and the resume-time sweep
// both look for), a TuningEndpointChanged event with state LEAKED is
// emitted naming the provider and endpoint id, and the error is returned —
// the caller must fail the run rather than report it complete. This is
// deliberately loud: a leaked endpoint keeps billing after Kno exits, and
// silence is the one failure mode that keeps costing money with nobody
// watching.
func TeardownGroup(ctx context.Context, p TeardownParams) error {
	rec, err := findRecord(ctx, p.Store, p.RunID, p.AblationGroup)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("bridge: TeardownGroup found no tuning job row for %s", p.AblationGroup)
	}

	tornDownErr := p.Tuner.Teardown(ctx, p.Endpoint)
	id, provider := endpointID(p.Endpoint), endpointProvider(p.Endpoint)
	if tornDownErr != nil {
		if p.Emitter != nil {
			// The event write failing on top of a teardown failure must not
			// swallow the teardown failure — it is what makes the leak
			// loud in the first place.
			_ = p.Emitter.EndpointChanged(ctx, rec.ProviderJobID, id, p.AblationGroup,
				knov1.TuningEndpointState_TUNING_ENDPOINT_STATE_LEAKED,
				rec.ServeMinutes, rec.ServeCostUSDMicros,
				fmt.Sprintf("provider %s endpoint %s: %v", provider, id, tornDownErr))
		}
		return fmt.Errorf("tearing down the %s group's endpoint %s (provider %s): %w — "+
			"THIS ENDPOINT MAY STILL BE LIVE AND BILLING; check the provider's console",
			p.AblationGroup, id, provider, tornDownErr)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	rec.TornDownAt = &now
	if err := p.Store.UpdateTuningJob(ctx, p.RunID, rec); err != nil {
		return fmt.Errorf("recording the torn-down endpoint for %s: %w", p.AblationGroup, err)
	}
	if p.Emitter != nil {
		if err := p.Emitter.EndpointChanged(ctx, rec.ProviderJobID, id, p.AblationGroup,
			knov1.TuningEndpointState_TUNING_ENDPOINT_STATE_TORN_DOWN,
			rec.ServeMinutes, rec.ServeCostUSDMicros, ""); err != nil {
			return fmt.Errorf("emitting the torn-down event for %s: %w", p.AblationGroup, err)
		}
	}
	return nil
}

// endpointID and endpointProvider are nil-safe field accessors for a plain
// core.Endpoint struct, which — unlike TuningJob and JobRef — is a Go type
// with no proto Get* convention. See core.Endpoint's doc for why.
func endpointID(ep *core.Endpoint) string {
	if ep == nil {
		return ""
	}
	return ep.ID
}

func endpointProvider(ep *core.Endpoint) string {
	if ep == nil {
		return ""
	}
	return ep.Provider
}

// LiveEndpointLimiter caps how many endpoints may be deployed at once — the
// tuner-bridge plan's --bridge-max-live-endpoints, default 1: deploy one
// model, run its eval passes, tear it down, then deploy the next. A
// semaphore rather than relying on the orchestration loop's own sequencing,
// so the cap has teeth even if a future caller processes groups
// concurrently — see the plan's rejected alternative "keep all N+1
// endpoints live for the whole bridge".
type LiveEndpointLimiter struct {
	slots chan struct{}
}

// NewLiveEndpointLimiter builds a limiter allowing at most max concurrent
// endpoints. max <= 0 is treated as 1 — the plan's default, and never
// unlimited: an unbounded default would let a caller multiply an
// idle-billed per-minute meter by N+1 without ever having said so.
func NewLiveEndpointLimiter(maxLive int) *LiveEndpointLimiter {
	if maxLive <= 0 {
		maxLive = 1
	}
	return &LiveEndpointLimiter{slots: make(chan struct{}, maxLive)}
}

// Acquire blocks until a deploy slot is free or ctx is done.
func (l *LiveEndpointLimiter) Acquire(ctx context.Context) error {
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release frees a deploy slot. Safe to call even if Acquire was never
// called successfully is NOT guaranteed — callers must pair every
// successful Acquire with exactly one Release, the same discipline a mutex
// Unlock requires.
func (l *LiveEndpointLimiter) Release() { <-l.slots }

// SweepResult is what SweepEndpoints found and settled for one row.
type SweepResult struct {
	AblationGroup string
	// TornDown reports whether an endpoint was found live and torn down.
	TornDown bool
	// OvershootUSDMicros is what RecordOrphanSpend recorded for minutes
	// accrued since the last settled tick, zero if nothing was owed.
	OvershootUSDMicros int64
}

// SweepEndpoints is the tuner-bridge plan's Step 2(g): a resumed run's
// FIRST action, before any Submit or Deploy, sweeps every row this run's
// own store carries with a live-or-unknown endpoint.
//
// Two cases, per row:
//
//  1. EndpointID recorded, TornDownAt nil: the row itself says an endpoint
//     exists and was never confirmed torn down. Teardown is attempted
//     directly — no ListEndpoints call needed, the ID is already known.
//  2. EndpointID nil, DeployedAt set: Deploy may have succeeded at the
//     provider before the row recorded its EndpointID (see DeployGroup's
//     write-ahead doc). tuner.ListEndpoints(ctx, suffix) resolves the
//     ambiguity: a match is torn down like case 1; no match means Deploy
//     never reached the provider, or its endpoint already expired, and
//     nothing further is owed.
//
// Minutes accrued between the last settled tick and the observed teardown
// are recorded through store.RecordOrphanSpend plus guard.Restore, with a
// SettlementOvershoot event — the same mechanism Step 2(c) uses for a
// cost overrun, applied to the second spend dimension. An endpoint the
// provider no longer lists (case 2, no match) settles at
// maxServeMinutes — the CONSERVATIVE direction, per the plan's edge-case
// table, rather than assuming zero extra minutes were billed.
func SweepEndpoints(
	ctx context.Context,
	st store.Store,
	tuner core.Tuner,
	guard *budget.Guard,
	em *Emitter,
	runID string,
	maxServeMinutes int32,
) ([]SweepResult, error) {
	jobs, err := st.TuningJobs(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("listing tuning jobs to sweep for %s: %w", runID, err)
	}

	var results []SweepResult
	for _, rec := range jobs {
		if rec.TornDownAt != nil {
			continue // already confirmed torn down
		}

		var ep *core.Endpoint
		switch {
		case rec.EndpointID != nil:
			ep = &core.Endpoint{ID: *rec.EndpointID, Provider: rec.Provider}
		case rec.DeployedAt != "":
			// Case 2: Deploy may have succeeded before the row recorded its
			// EndpointID. Resolve by listing.
			eps, err := tuner.ListEndpoints(ctx, rec.Suffix)
			if err != nil {
				return results, fmt.Errorf("listing provider endpoints to sweep %s: %w", rec.AblationGroup, err)
			}
			if len(eps) == 0 {
				// No match: settle the conservative bound and move on — see
				// the plan's "resume finds a run's endpoint gone" edge case.
				delta, overshoot, err := settleSweptMinutes(ctx, st, guard, em, runID, rec, maxServeMinutes)
				if err != nil {
					return results, err
				}
				results = append(results, SweepResult{AblationGroup: rec.AblationGroup, OvershootUSDMicros: overshoot})
				_ = delta
				continue
			}
			ep = eps[0]
			id := ep.ID
			rec.EndpointID = &id
			if err := st.UpdateTuningJob(ctx, runID, rec); err != nil {
				return results, fmt.Errorf("recording the swept endpoint id for %s: %w", rec.AblationGroup, err)
			}
		default:
			continue // never deployed
		}

		if err := tuner.Teardown(ctx, ep); err != nil {
			return results, fmt.Errorf("sweeping (tearing down) the %s group's endpoint %s: %w — "+
				"THIS ENDPOINT MAY STILL BE LIVE AND BILLING; check the provider's console",
				rec.AblationGroup, ep.ID, err)
		}

		_, overshoot, err := settleSweptMinutes(ctx, st, guard, em, runID, rec, maxServeMinutes)
		if err != nil {
			return results, err
		}

		now := time.Now().UTC().Format(time.RFC3339)
		rec.TornDownAt = &now
		if err := st.UpdateTuningJob(ctx, runID, rec); err != nil {
			return results, fmt.Errorf("recording the swept teardown for %s: %w", rec.AblationGroup, err)
		}
		if em != nil {
			if err := em.EndpointChanged(ctx, rec.ProviderJobID, ep.ID, rec.AblationGroup,
				knov1.TuningEndpointState_TUNING_ENDPOINT_STATE_TORN_DOWN,
				rec.ServeMinutes, rec.ServeCostUSDMicros, ""); err != nil {
				return results, fmt.Errorf("emitting the swept teardown event for %s: %w", rec.AblationGroup, err)
			}
		}
		results = append(results, SweepResult{AblationGroup: rec.AblationGroup, TornDown: true, OvershootUSDMicros: overshoot})
	}
	return results, nil
}

// settleSweptMinutes true-ups the minutes a swept endpoint accrued beyond
// what the row already settled, at maxServeMinutes — the conservative
// bound, since a swept endpoint's real last-tick time is unknown to this
// process. Records the delta through RecordOrphanSpend + Guard.Restore, the
// same shape ReconcileTerminal uses for a cost overrun, and emits
// SettlementOvershoot when nonzero.
//
// rec.ServeMinutes is updated to the swept total for the record's own
// honesty (a reader, and kno doctor, should see how long the endpoint
// actually ran) — but rec.ServeCostUSDMicros is DELIBERATELY left
// untouched: that column feeds store.SettledSpend's SUM unconditionally
// (see its own comment — "a tick IS its own settlement"), and the swept
// delta is recorded through RecordOrphanSpend instead, exactly as
// ReconcileTerminal leaves EstimatedCostUSDMicros untouched and records an
// actual>estimate overrun through the same call. Adding the delta to BOTH
// columns would double-count it in SettledSpend's sum.
func settleSweptMinutes(
	ctx context.Context, st store.Store, guard *budget.Guard, em *Emitter,
	runID string, rec *store.TuningJobRecord, maxServeMinutes int32,
) (deltaMinutes int32, overshootUSDMicros int64, err error) {
	if maxServeMinutes <= rec.ServeMinutes {
		return 0, 0, nil
	}
	deltaMinutes = maxServeMinutes - rec.ServeMinutes
	// The price is not available here — SweepEndpoints runs at resume
	// before any pricing lookup this run's flags would repeat. The
	// unsettled COST is therefore computed from what the row already knows
	// its rate to be: ServeCostUSDMicros / ServeMinutes, the same rate
	// already paid for the minutes settled so far. A row with zero minutes
	// settled (Deploy succeeded but no tick ever ran) has no rate to infer
	// and true-ups at zero cost with the minutes still recorded — the
	// conservative-on-money, honest-about-minutes-owed compromise; the
	// gap is visible in ServeMinutes even when it cannot be priced.
	var costPerMinute int64
	if rec.ServeMinutes > 0 {
		costPerMinute = rec.ServeCostUSDMicros / int64(rec.ServeMinutes)
	}
	delta := costPerMinute * int64(deltaMinutes)

	rec.ServeMinutes = maxServeMinutes
	if err := st.UpdateTuningJob(ctx, runID, rec); err != nil {
		return 0, 0, fmt.Errorf("recording the swept serve minutes for %s: %w", rec.AblationGroup, err)
	}
	if delta <= 0 {
		return deltaMinutes, 0, nil
	}

	if err := st.RecordOrphanSpend(ctx, runID, budget.Spend{CostUSDMicros: delta}); err != nil {
		return 0, 0, fmt.Errorf("recording the swept hosting overshoot for %s: %w", rec.AblationGroup, err)
	}
	guard.Restore(budget.Spend{CostUSDMicros: delta})
	if em != nil {
		if err := em.SettlementOvershoot(ctx, rec.AblationGroup, 0, delta, delta, delta); err != nil {
			return 0, 0, fmt.Errorf("emitting the swept overshoot event for %s: %w", rec.AblationGroup, err)
		}
	}
	return deltaMinutes, delta, nil
}

// ErrServeMinutesExceeded is returned by a caller's serve loop (not by this
// package's functions directly) when --bridge-max-serve-minutes is reached
// before an eval pass completes — see the tuner-bridge plan's edge case
// "endpoint never becomes ready" and acceptance criterion 34. Exported so
// bridge/run.go and the CLI can classify it distinctly from a hard error.
var ErrServeMinutesExceeded = errors.New("bridge: --bridge-max-serve-minutes reached before the group's eval passes completed")

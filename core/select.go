package core

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/interval"
	"github.com/knograph/kno/stats/portfolio"
	"github.com/knograph/kno/store"
	"google.golang.org/protobuf/proto"
)

// defaultShingleOverlap is the Jaccard threshold on 3-gram shingles above
// which two knowledge Assets are judged redundant. A number, not a rule —
// see the plan's A12 for the accepted limitation.
const defaultShingleOverlap = 0.6

// SelectOptions configure the Select stage.
//
// The stage reads ONLY the Value run's recorded Valuations — it takes no
// Evals, so the holdout cannot be reached through it, and every decision
// below is a pure function of what the store holds.
type SelectOptions struct {
	// RunID identifies this Select run.
	RunID string

	// ValueRunID is the Value run whose recorded Valuations this builds on.
	// The source run's status travels to the Portfolio: a Portfolio built
	// from a budget-stopped run is a different statement from one built from
	// a completed run.
	ValueRunID string

	// Store is where Valuations are read and the Portfolio is written.
	Store store.Store

	// Pool supplies Asset content and destinations. Optional: without it the
	// REDUNDANT and WRONG_MECHANISM rules cannot run, and the result names
	// which rules were degraded — the budget rules and every measurement-
	// based reason still decide.
	Pool Pool

	// Budget is the constraint set the Portfolio is built under. Every cap
	// is honored per selected Asset and checked before the Asset joins.
	Budget *knov1.Budget

	// AllowPartial accepts a source run that did not complete (BUDGET_STOPPED
	// or INTERRUPTED) and builds from its recorded Valuations, carrying the
	// source status on the Portfolio. Refused without this flag: a partial
	// source would rank an incomplete measurement set as if it were the
	// whole answer.
	AllowPartial bool

	// Level is the confidence level every interval this stage decides with
	// is computed at. Zero means the stage default, 0.95.
	Level float64
}

// SelectResult is what a Select run produced.
type SelectResult struct {
	// RunID identifies the run.
	RunID string

	// Portfolio is the constructed Portfolio, source status included.
	Portfolio *knov1.Portfolio

	// Status is how the run ended.
	Status knov1.RunStatus

	// DegradedRules names the rejection rules that could not run because no
	// Pool was given — REDUNDANT and WRONG_MECHANISM need content and
	// destinations. Reported, never silent.
	DegradedRules []string
}

// Select executes the stage: rank the recorded Valuations on delta_per_cost,
// decide each Asset in precedence order, and write the Portfolio.
//
// Greedy on delta_per_cost with per-item feasibility checks — a deliberately
// simple construction with no approximation guarantee, and the plan says so.
// Every keep/reject interval is Bonferroni-corrected for the number of
// Assets screened, and the portfolio-level gain is a single corrected claim
// combined under the shared-draw covariance discipline (stats/portfolio):
// never the sum of independent intervals, because routed slices overlap and
// every delta pairs against the shared baseline draw.
//
// "Include nothing new" is a legal, first-class outcome: an empty selection
// with a full rejection log is a completed run.
func (o SelectOptions) Select(ctx context.Context) (*SelectResult, error) {
	level := o.Level
	if level == 0 {
		level = defaultLevel
	}
	if err := o.validate(level); err != nil {
		return nil, err
	}

	source, err := o.Store.GetRun(ctx, o.ValueRunID)
	if err != nil {
		return nil, fmt.Errorf("loading the source Value run %s: %w", o.ValueRunID, err)
	}
	if got := source.GetStage(); got != knov1.Stage_STAGE_VALUE {
		return nil, errs.ErrInvalidInput.
			WithFix("pass the run ID of a `kno value` run").
			Wrap(fmt.Errorf("run %s is a %s run, not a Value run", o.ValueRunID, got))
	}
	if s := source.GetStatus(); s != knov1.RunStatus_RUN_STATUS_COMPLETED {
		if !o.AllowPartial {
			return nil, errs.ErrInvalidInput.
				WithFix("re-run Value to completion, or pass --allow-partial to select " +
					"from the recorded Valuations anyway").
				Wrap(fmt.Errorf("source run %s ended %s (%s); ranking an incomplete "+
					"measurement set as if it were the whole answer would mislead",
					o.ValueRunID, s, source.GetIncompleteReason()))
		}
	}

	valuations, err := o.Store.Valuations(ctx, o.ValueRunID)
	if err != nil {
		return nil, fmt.Errorf("reading Valuations for %s: %w", o.ValueRunID, err)
	}

	// The pool's Assets are borrowed by the iterator; the decision code
	// retains them, so each is cloned into the map.
	var assets map[string]*Asset
	if o.Pool != nil {
		assets, err = loadAssetsByID(ctx, o.Pool)
		if err != nil {
			return nil, err
		}
	}

	run := &knov1.Run{
		Id:              o.RunID,
		Stage:           knov1.Stage_STAGE_SELECT,
		CreatedAt:       time.Now().Format(time.RFC3339),
		Status:          knov1.RunStatus_RUN_STATUS_RUNNING,
		Budget:          o.Budget,
		GoalName:        source.GetGoalName(),
		GoalDirection:   source.GetGoalDirection(),
		GoalScoreDomain: source.GetGoalScoreDomain(),
		DevCaseCount:    source.GetDevCaseCount(),
	}
	if err := o.Store.CreateRun(ctx, run); err != nil {
		return nil, fmt.Errorf("creating the run: %w", err)
	}
	em := &selectEmitter{}
	if err := o.emitRunStarted(ctx, em); err != nil {
		return nil, err
	}

	p, degraded, err := o.decide(ctx, valuations, assets, level)
	if err != nil {
		return nil, err
	}
	p.RunId = o.RunID
	p.Budget = o.Budget
	p.SourceRunId = o.ValueRunID
	p.SourceStatus = source.GetStatus()
	p.SourceIncompleteReason = source.GetIncompleteReason()

	if err := o.Store.WritePortfolio(ctx, o.RunID, p); err != nil {
		return nil, fmt.Errorf("writing the Portfolio: %w", err)
	}
	if err := o.append(ctx, em, func() *knov1.Event {
		return &knov1.Event{
			Payload: &knov1.Event_PortfolioSelected{PortfolioSelected: &knov1.PortfolioSelected{
				//nolint:gosec // bounded by the Portfolio's entries
				Selected: int32(len(p.GetSelected())),
				//nolint:gosec // bounded by the Portfolio's entries
				Rejected:             int32(len(p.GetRejected())),
				DevEstimatedGain:     p.GetDevEstimatedGain(),
				DevEstimatedInterval: p.GetDevEstimatedInterval(),
				TotalCostUsdMicros:   p.GetTotalCost().GetAcquisitionUsdMicros(),
			}},
		}
	}, "portfolio-selected"); err != nil {
		return nil, err
	}

	status := knov1.RunStatus_RUN_STATUS_COMPLETED
	run.Status = status
	run.FinishedAt = proto.String(time.Now().Format(time.RFC3339))
	if err := o.Store.FinishRun(ctx, run); err != nil {
		return nil, fmt.Errorf("closing the run: %w", err)
	}
	if err := o.append(ctx, em, func() *knov1.Event {
		return &knov1.Event{
			Payload: &knov1.Event_RunFinished{RunFinished: &knov1.RunFinished{
				Status: status,
			}},
		}
	}, "run-finished"); err != nil {
		return nil, err
	}
	return &SelectResult{RunID: o.RunID, Portfolio: p, Status: status, DegradedRules: degraded}, nil
}

// validate refuses what can be refused before any decision is computed.
func (o SelectOptions) validate(level float64) error {
	switch {
	case o.RunID == "":
		return errs.ErrInvalidInput.Wrap(errors.New("select: a run ID is required"))
	case o.ValueRunID == "":
		return errs.ErrInvalidInput.
			WithFix("pass --value-run-id, or run `kno value` first").
			Wrap(errors.New("select: a Value run to build on is required"))
	case o.Store == nil:
		return errs.ErrInvalidInput.Wrap(errors.New("select: a store is required"))
	case o.Budget == nil:
		return errs.ErrInvalidInput.
			WithFix("pass at least one budget cap: --max-context-tokens, " +
				"--max-training-examples, --max-cost-usd").
			Wrap(errors.New("select: a budget is required"))
	case math.IsNaN(level) || level <= 0.5 || level >= 1:
		return errs.ErrInvalidInput.Wrap(errors.New("select: the confidence level is invalid"))
	}
	return nil
}

// loadAssetsByID loads a pool into a map, keyed by Asset ID.
//
// The pool's Assets are borrowed by the iterator for one iteration, so each
// is cloned — both Select and Export retain them past the iteration.
func loadAssetsByID(ctx context.Context, pool Pool) (map[string]*Asset, error) {
	it, err := pool.Assets(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening the pool: %w", err)
	}
	out := map[string]*Asset{}
	for a, err := range it {
		if err != nil {
			return nil, fmt.Errorf("reading the pool: %w", err)
		}
		if a == nil || a.GetId() == "" {
			continue
		}
		out[a.GetId()] = proto.Clone(a).(*Asset)
	}
	return out, nil
}

// decidedKnowledge is one selected knowledge Asset, kept by content so the
// REDUNDANT rule has what it compares against.
type decidedKnowledge struct {
	assetID string
	content []byte
}

// spend is the greedy run's accumulated budget accounting, keyed by
// destination so each cap is charged what that destination actually costs.
type spend struct {
	contextTokens  int64
	training       int64
	knowledgeBytes int64
	costUsdMicros  int64
}

// decide is the pure decision core: measured Valuations ranked by
// delta_per_cost and decided in precedence order, unmeasured ones rejected
// with the reason Value recorded. Deterministic by construction — every
// ordering below ends in the Asset ID — so two runs over the same store
// produce byte-identical Portfolios.
func (o SelectOptions) decide(
	_ context.Context,
	valuations []*Valuation,
	assets map[string]*Asset,
	level float64,
) (*knov1.Portfolio, []string, error) {
	var degraded []string
	if o.Pool == nil {
		degraded = append(degraded, "REDUNDANT", "WRONG_MECHANISM")
	}

	var measured, unmeasured []*Valuation
	for _, v := range valuations {
		if v.GetNotMeasured() == knov1.RejectionReason_REJECTION_REASON_UNSPECIFIED {
			measured = append(measured, v)
		} else {
			unmeasured = append(unmeasured, v)
		}
	}
	sort.Slice(measured, func(i, j int) bool {
		return rankLess(measured[i], measured[j])
	})
	sort.Slice(unmeasured, func(i, j int) bool {
		return unmeasured[i].GetAssetId() < unmeasured[j].GetAssetId()
	})

	nScreened := len(measured)
	p := &knov1.Portfolio{}
	spent := spend{}
	var selectedKnowledge []decidedKnowledge

	for _, v := range measured {
		asset := assets[v.GetAssetId()]
		corrected := portfolio.Correct(v.GetDeltaInterval(), nScreened)
		if corrected == nil {
			// A recorded interval this stage cannot correct gets no decision
			// from it — the refusal is the honest answer.
			p.Rejected = append(p.Rejected, &Rejection{
				AssetId:   v.GetAssetId(),
				Reason:    knov1.RejectionReason_REJECTION_REASON_UNDERPOWERED,
				Detail:    "the recorded interval cannot be corrected for multiplicity",
				Valuation: v,
			})
			continue
		}

		dest := destinationFor(asset, v)
		reason, detail, redundantWith := rejectReason(v, corrected, asset, dest, selectedKnowledge, level, o.Budget, spent)

		if reason == knov1.RejectionReason_REJECTION_REASON_UNSPECIFIED {
			// Fits the budget under every rule: select, and charge what the
			// destination costs.
			charge(v, asset, dest, &spent)
			rank := int32(len(p.GetSelected()) + 1) //nolint:gosec // bounded by the pool
			entry := &PortfolioEntry{
				AssetId:     v.GetAssetId(),
				Destination: dest,
				Valuation:   proto.Clone(v).(*Valuation),
				Rank:        rank,
			}
			if scale, ok := routedScale(v); ok {
				entry.NRoutedScale = &scale
			}
			// The content hash Validate checks before it spends anything.
			//
			// Without it, a Pool edited between `select` and `validate`
			// produces a holdout number for a set that is not the set the
			// report names, undetectably — Asset carries no content hash of
			// its own. Written only when a Pool was supplied: absence disables
			// the check rather than failing it, because a Portfolio selected
			// without a Pool is not evidence of tampering.
			if asset != nil {
				sum := sha256.Sum256(asset.GetContent())
				entry.ContentHash = sum[:]
			}
			p.Selected = append(p.Selected, entry)
			if asset != nil && kindOf(v) == knov1.Kind_KIND_KNOWLEDGE {
				selectedKnowledge = append(selectedKnowledge, decidedKnowledge{
					assetID: v.GetAssetId(),
					content: asset.GetContent(),
				})
			}
			continue
		}
		rej := &Rejection{
			AssetId:   v.GetAssetId(),
			Reason:    reason,
			Detail:    detail,
			Valuation: v,
		}
		if reason == knov1.RejectionReason_REJECTION_REASON_REDUNDANT {
			rej.RedundantWithAssetIds = redundantWith
		}
		p.Rejected = append(p.Rejected, rej)
	}
	for _, v := range unmeasured {
		p.Rejected = append(p.Rejected, &Rejection{
			AssetId:   v.GetAssetId(),
			Reason:    v.GetNotMeasured(),
			Valuation: v,
		})
	}

	// The portfolio-level claim: one corrected interval over the whole
	// selection, combined under the shared-draw covariance discipline — the
	// deltas are positively correlated through the shared baseline draw, so
	// the half-widths combine linearly, never as independent intervals.
	var gain, half float64
	total := &knov1.CostVector{}
	for _, e := range p.GetSelected() {
		v := e.GetValuation()
		var s float64
		if scale, ok := routedScale(v); ok {
			s = scale
		}
		iv := portfolio.Correct(v.GetDeltaInterval(), nScreened)
		if iv == nil {
			// Unreachable: every selected entry passed the correction above.
			continue
		}
		gain += v.GetDeltaGoal() * s
		half += halfWidth(iv) * s
		combineCost(total, v.GetCost())
	}
	if len(p.GetSelected()) > 0 && half > 0 {
		p.DevEstimatedGain = gain
		p.DevEstimatedInterval = &Interval{
			Low:       gain - half,
			High:      gain + half,
			Level:     correctedLevel(level, nScreened),
			Method:    "portfolio-greedy-shared",
			Sidedness: knov1.Sidedness_SIDEDNESS_TWO_SIDED,
		}
	}
	p.TotalCost = total

	// Rejections ordered by Asset ID: the log is a deliverable, and ordering
	// it the same way every run does is what makes it diffable.
	sort.Slice(p.Rejected, func(i, j int) bool {
		return p.Rejected[i].GetAssetId() < p.Rejected[j].GetAssetId()
	})
	return p, degraded, nil
}

// rejectReason decides one measured Asset in precedence order and returns
// the reason to reject it with, or UNSPECIFIED to select it. Precedence is
// REGRESSION, NO_EFFECT, REDUNDANT, COST_DOMINATED, WRONG_MECHANISM — the
// strongest claim wins, and an Asset rejected by an earlier rule never gets
// a weaker reason.
//
// Interval bounds print at four decimal places, matching the value table and
// the report. They used to print with %v, i.e. all 17 digits — which was
// false precision on a bound derived from a t-quantile, and not merely ugly:
// math.Exp and math.Log are architecture-specific, so the bisection that
// computes the quantile lands one ULP apart on arm64 and amd64 and the tail
// digits genuinely differ by platform. uknoAI/kno-benchmarks caught that as a
// cross-platform diff on identical inputs. Four places is more precision than
// the measurement carries and is the same on every machine.
func rejectReason(
	v *Valuation,
	corrected *Interval,
	asset *Asset,
	dest knov1.Destination,
	selectedKnowledge []decidedKnowledge,
	level float64,
	budget *knov1.Budget,
	spent spend,
) (knov1.RejectionReason, string, []string) {
	// REGRESSION: the whole net interval sits at or below zero — the Asset
	// helped its slice and hurt the controls — AND the control arm was
	// powered. An underpowered harm test that looks like a passed one is
	// worse than an absent one, so the gate stands between noise and the
	// strongest reason in the enum.
	if !v.GetControlUnderpowered() {
		if net := netInterval(v, corrected, level); net != nil && net.GetHigh() <= 0 {
			return knov1.RejectionReason_REJECTION_REASON_REGRESSION, fmt.Sprintf(
				"net delta %+.4f, CI [%+.4f, %+.4f] at or below zero",
				netCenter(net), net.GetLow(), net.GetHigh(),
			), nil
		}
	}
	// NO_EFFECT: the corrected interval crosses zero — the measurement this
	// Asset's place would rest on is indistinguishable from nothing.
	if corrected.GetLow() <= 0 && corrected.GetHigh() >= 0 {
		return knov1.RejectionReason_REJECTION_REASON_NO_EFFECT, fmt.Sprintf(
			"delta %+.4f, CI [%+.4f, %+.4f] crosses zero",
			v.GetDeltaGoal(), corrected.GetLow(), corrected.GetHigh(),
		), nil
	}
	// REDUNDANT: within knowledge-kind only, and only against Assets already
	// selected — shingle overlap on content is meaningless across kinds and
	// across destinations, and first-seen-wins keeps the selection
	// deterministic.
	if asset != nil && kindOf(v) == knov1.Kind_KIND_KNOWLEDGE {
		if with := redundantWith(asset.GetContent(), selectedKnowledge); len(with) > 0 {
			return knov1.RejectionReason_REJECTION_REASON_REDUNDANT,
				"shingle overlap above the redundancy threshold", with
		}
	}
	// COST_DOMINATED: does not fit the budget, or a better-ranked Asset took
	// the room it would need. Feasibility is checked per item, so a later,
	// cheaper Asset can still fit — greedy has no shortcut here.
	if over := overBudget(v, asset, dest, budget, spent); over != "" {
		return knov1.RejectionReason_REJECTION_REASON_COST_DOMINATED, over, nil
	}
	// WRONG_MECHANISM: real effect, wrong vehicle. A knowledge Asset destined
	// for the tuning set would need retention it does not have and could not
	// patch staleness in.
	if asset != nil && kindOf(v) == knov1.Kind_KIND_KNOWLEDGE &&
		dest == knov1.Destination_DESTINATION_TUNING_SET {
		return knov1.RejectionReason_REJECTION_REASON_WRONG_MECHANISM,
			"a knowledge Asset in the tuning set would be unreliably retained and cannot be patched when stale", nil
	}
	return knov1.RejectionReason_REJECTION_REASON_UNSPECIFIED, "", nil
}

// netInterval combines the treatment and control deltas into one corrected
// net judgement, or returns nil when the record cannot support one.
//
// The control interval is one-sided (a harm bound), so its two-sided
// half-width at the judgement's level is derived from the recorded bound and
// the control's own degrees of freedom — the center of the bound is the
// recorded delta_control.
func netInterval(v *Valuation, corrected *Interval, level float64) *Interval {
	ctrl := v.GetControlInterval()
	if ctrl == nil || v.GetDeltaInterval().GetSidedness() != knov1.Sidedness_SIDEDNESS_TWO_SIDED {
		return nil
	}
	nT, nC := int(v.GetNRouted()), int(v.GetNControl())
	if nT <= 0 || nC <= 0 || ctrl.GetNPairs() < 2 {
		return nil
	}
	df := int(ctrl.GetNPairs()) - 1
	// The harm bound's one-sided half at its level, widened to the two-sided
	// half at the judgement's level by the quantile ratio.
	halfC := (v.GetDeltaControl() - ctrl.GetLow()) *
		interval.Quantile(level, knov1.Sidedness_SIDEDNESS_TWO_SIDED, df) /
		interval.Quantile(ctrl.GetLevel(), knov1.Sidedness_SIDEDNESS_LOWER, df)
	if math.IsNaN(halfC) || math.IsInf(halfC, 0) || halfC <= 0 {
		return nil
	}
	// Fresh control arms pair each trial against its own draw; a recorded-
	// baseline arm shares the draw with every other delta — the covariance
	// the conservative combination exists for. Unknown means recorded, the
	// conservative direction.
	shared := !v.GetFreshControlArm()
	return portfolio.NetLoss(
		portfolio.NetDelta{Mean: v.GetDeltaGoal(), Half: halfWidth(corrected), N: nT},
		portfolio.NetDelta{Mean: v.GetDeltaControl(), Half: halfC, N: nC},
		shared, level,
	)
}

// charge adds one selected Asset to the running spend, charged by what its
// destination costs: the context cap counts context tokens, the tuning cap
// counts examples, the knowledge-base cap counts bytes, and the cost cap
// counts acquisition dollars.
func charge(v *Valuation, asset *Asset, dest knov1.Destination, spent *spend) {
	cost := v.GetCost()
	var contextTokens, ftTokens, acquire int64
	if cost != nil {
		contextTokens, ftTokens, acquire = cost.GetContextTokens(), cost.GetFtTokens(), cost.GetAcquisitionUsdMicros()
	}
	// The cost cap is charged on every destination: acquisition dollars
	// are dollars wherever the Asset lands.
	spent.costUsdMicros += acquire
	switch dest {
	case knov1.Destination_DESTINATION_TUNING_SET:
		spent.training++
	case knov1.Destination_DESTINATION_KNOWLEDGE_BASE:
		if asset != nil {
			spent.knowledgeBytes += int64(len(asset.GetContent()))
		}
	default: // CONTEXT
		spent.contextTokens += contextTokens
	}
	// ft_tokens stay out of the budget: the tuning cap counts EXAMPLES, and
	// ft_tokens is the tokenizer's contribution, which debt #68 names as too
	// biased to rank on. They still ride in total_cost.
	_ = ftTokens
}

// overBudget reports which cap the Asset would violate, or "" when it fits.
func overBudget(v *Valuation, asset *Asset, dest knov1.Destination, budget *knov1.Budget, spent spend) string {
	cost := v.GetCost()
	var contextTokens, ftTokens, acquire int64
	if cost != nil {
		contextTokens, ftTokens, acquire = cost.GetContextTokens(), cost.GetFtTokens(), cost.GetAcquisitionUsdMicros()
	}
	switch dest {
	case knov1.Destination_DESTINATION_TUNING_SET:
		if budget.GetMaxTrainingExamples() > 0 &&
			spent.training+1 > budget.GetMaxTrainingExamples() {
			return fmt.Sprintf("the tuning set holds %d of %d allowed examples",
				spent.training, budget.GetMaxTrainingExamples())
		}
	case knov1.Destination_DESTINATION_KNOWLEDGE_BASE:
		if budget.GetMaxKnowledgeBaseBytes() > 0 {
			bytes := int64(0)
			if asset != nil {
				bytes = int64(len(asset.GetContent()))
			}
			if spent.knowledgeBytes+bytes > budget.GetMaxKnowledgeBaseBytes() {
				return fmt.Sprintf("the knowledge base holds %d of %d allowed bytes",
					spent.knowledgeBytes, budget.GetMaxKnowledgeBaseBytes())
			}
		}
	default: // CONTEXT
		if budget.GetMaxContextTokens() > 0 &&
			spent.contextTokens+contextTokens > budget.GetMaxContextTokens() {
			return fmt.Sprintf("context holds %d of %d allowed tokens",
				spent.contextTokens, budget.GetMaxContextTokens())
		}
	}
	if budget.GetMaxCostUsdMicros() > 0 && spent.costUsdMicros+acquire > budget.GetMaxCostUsdMicros() {
		return fmt.Sprintf("spend would reach %d of %d allowed micro-USD",
			spent.costUsdMicros+acquire, budget.GetMaxCostUsdMicros())
	}
	// ft_tokens is charged nowhere; see charge.
	_ = ftTokens
	return ""
}

// destinationFor is the mechanism routing this stage applies: the Asset's own
// destination when the pool supplied one, else the kind's home — and for
// knowledge Kind, the home follows the InjectionMode that produced the
// number (docs/debt.md#133). A context-mode delta is a claim about being in
// the prompt, so its Destination is the prompt; a knowledge-mode delta is a
// claim about being retrieved, so its Destination is the index. Routing a
// context-bound Asset into a knowledge base would apply an upper bound
// through a retriever that was never measured — the exact conflation
// InjectionMode exists as a required field to prevent. KIND_BEHAVIOR is
// unaffected by mode: it always routes to the tuning set.
func destinationFor(asset *Asset, v *Valuation) knov1.Destination {
	if asset != nil {
		if d := asset.GetDestination(); d != knov1.Destination_DESTINATION_UNSPECIFIED {
			return d
		}
	}
	switch kindOf(v) {
	case knov1.Kind_KIND_BEHAVIOR:
		return knov1.Destination_DESTINATION_TUNING_SET
	case knov1.Kind_KIND_KNOWLEDGE:
		if v.GetMode() == knov1.InjectionMode_INJECTION_MODE_KNOWLEDGE {
			return knov1.Destination_DESTINATION_KNOWLEDGE_BASE
		}
	}
	return knov1.Destination_DESTINATION_CONTEXT
}

// kindOf reads the kind from the Valuation, falling back to the Asset's.
func kindOf(v *Valuation) knov1.Kind {
	if k := v.GetKind(); k != knov1.Kind_KIND_UNSPECIFIED {
		return k
	}
	return knov1.Kind_KIND_KNOWLEDGE
}

// routedScale is the debt #65 scaling factor: the effect is uniform on the
// routed slice and zero elsewhere, so a tagged delta over n_routed of n_dev
// Cases scales by n_routed / n_dev. Absent n_routed (or n_dev) means no
// scale — the delta is used as-is, flagged by the field's absence.
func routedScale(v *Valuation) (float64, bool) {
	nR, nD := v.GetNRouted(), v.GetNDev()
	if nR <= 0 || nD <= 0 {
		return 0, false
	}
	return float64(nR) / float64(nD), true
}

// halfWidth of a two-sided interval.
func halfWidth(iv *Interval) float64 {
	return (iv.GetHigh() - iv.GetLow()) / 2
}

// netCenter of a two-sided interval.
func netCenter(iv *Interval) float64 {
	return (iv.GetLow() + iv.GetHigh()) / 2
}

// correctedLevel is the Bonferroni level the corrected intervals carry.
func correctedLevel(level float64, nScreened int) float64 {
	if nScreened < 2 {
		return level
	}
	return 1 - (1-level)/float64(nScreened)
}

// rankLess orders candidates by delta_per_cost descending, then delta
// descending, then Asset ID ascending — the deterministic tie-break.
func rankLess(a, b *Valuation) bool {
	if a.GetDeltaPerCost() != b.GetDeltaPerCost() {
		return a.GetDeltaPerCost() > b.GetDeltaPerCost()
	}
	if a.GetDeltaGoal() != b.GetDeltaGoal() {
		return a.GetDeltaGoal() > b.GetDeltaGoal()
	}
	return a.GetAssetId() < b.GetAssetId()
}

// redundantWith reports which already-selected contents this Asset's content
// duplicates, by Jaccard overlap of 3-gram shingles.
func redundantWith(content []byte, selected []decidedKnowledge) []string {
	if len(content) == 0 {
		return nil
	}
	mine := shingles(content)
	var with []string
	for _, other := range selected {
		if len(other.content) == 0 {
			continue
		}
		if shingleOverlap(mine, shingles(other.content)) >= defaultShingleOverlap {
			with = append(with, other.assetID)
		}
	}
	return with
}

// shingles tokenizes content into lowercase word 3-grams.
func shingles(content []byte) map[string]struct{} {
	words := strings.FieldsFunc(string(content), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	})
	out := make(map[string]struct{}, len(words))
	for i := 0; i+2 < len(words); i++ {
		out[strings.ToLower(words[i])+" "+strings.ToLower(words[i+1])+" "+strings.ToLower(words[i+2])] = struct{}{}
	}
	return out
}

// shingleOverlap is the Jaccard similarity of two shingle sets.
func shingleOverlap(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter, union := 0, len(a)
	for s := range b {
		if _, ok := a[s]; ok {
			inter++
		} else {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// combineCost adds one Asset's carrying cost into the running total.
func combineCost(total *knov1.CostVector, c *knov1.CostVector) {
	if c == nil {
		return
	}
	total.ContextTokens += c.GetContextTokens()
	total.FtTokens += c.GetFtTokens()
	total.AcquisitionUsdMicros += c.GetAcquisitionUsdMicros()
}

// selectEmitter serializes event writes the way the other stages' emitters
// do: sequence order equals insertion order, and the hot-path write failure
// is remembered rather than returned.
type selectEmitter struct {
	mu     sync.Mutex
	seq    int64
	closed bool
}

func (o SelectOptions) append(ctx context.Context, em *selectEmitter, build func() *knov1.Event, what string) error {
	em.mu.Lock()
	defer em.mu.Unlock()
	if em.closed {
		return fmt.Errorf("appending %s event: the run already emitted RunFinished", what)
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

func (em *selectEmitter) next() int64 {
	em.seq++
	return em.seq
}

func (o SelectOptions) emitRunStarted(ctx context.Context, em *selectEmitter) error {
	return o.append(ctx, em, func() *knov1.Event {
		return &knov1.Event{
			Payload: &knov1.Event_RunStarted{RunStarted: &knov1.RunStarted{
				Stage: knov1.Stage_STAGE_SELECT,
			}},
		}
	}, "run-started")
}

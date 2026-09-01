package judge

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/stats/interval"
)

// DefaultMinKappa is the agreement floor, and the argument for it is NOT
// Landis-Koch. (Their "substantial" band starts at 0.61. The coincidence is
// named here so a reader does not mistake it for the reasoning.)
//
// The derivation is two steps, and both are in
// docs/what-the-numbers-mean.md in full:
//
//  1. On a balanced set with symmetric error rate epsilon, a judge attenuates
//     every measured delta by exactly (1 − 2·epsilon), and its kappa IS that
//     factor: p_o = 1 − epsilon, p_e = 0.5, so kappa = 1 − 2·epsilon. Kappa is
//     therefore denominated in the units of the thing this tool sells.
//  2. Power for a paired comparison scales with the square of the effect size,
//     so attenuating an effect by a costs 1/a² times as many Cases for the
//     same power. Choosing "a judge may cost at most 3x your eval budget"
//     gives a >= 1/sqrt(3) = 0.577, rounded up.
//
// It is a published price, not a convention: a judge at the floor triples what
// a measurement costs relative to a perfect scorer. A user who thinks 3x is
// too generous raises --min-kappa and knows what they are buying.
const DefaultMinKappa = 0.60

// MaxSymmetryGap is how far sensitivity and specificity may diverge before the
// floor's derivation stops holding.
//
// Step 1 above assumes non-differential error. Under asymmetric error kappa is
// no longer the attenuation factor and can mask a direction-biased judge — the
// judge that never says "fail" is the costly failure mode — so the assumption
// is a gate in its own right, reported separately so a failure names which
// assumption broke.
const MaxSymmetryGap = 0.20

// MaxErrorRate is the share of records a judge may fail outright before the
// run is not a usable calibration.
//
// Five percent and the same words as the baseline gate
// (core/baseline_close.go), deliberately: two thresholds for "too many things
// broke to trust this" would be two numbers to keep in sync.
const MaxErrorRate = 0.05

// The three verdicts. Three, not two, and for the reason core/gaps.go has
// IMPROVED / GAP / UNKNOWN: "we did not really look" must not read like "we
// looked and found nothing".
const (
	VerdictPass          = "PASS"
	VerdictFail          = "FAIL"
	VerdictIndeterminate = "INDETERMINATE"

	// VerdictNotApplicable is a graded Goal, which is reported and not gated.
	VerdictNotApplicable = "NOT_APPLICABLE"
)

// Options configures one calibration run.
type Options struct {
	// Goal is what is being calibrated.
	Goal core.Goal

	// GoalName is what it is called on the command line and in the baseline.
	GoalName string

	// Set is the loaded calibration set.
	Set *Set

	// Fixtures supplies recorded judge responses. Required in replay for a
	// Goal that has prompts; ignored for a Goal that has none, which has
	// nothing recorded because it calls nothing.
	Fixtures *FixtureStore

	// Live calls the judge instead of replaying. Requires Guard when the Goal
	// implements Costed.
	Live bool

	// Guard authorizes and settles every judge call. Nil in replay: there is
	// no spend to authorize, so no guard is constructed.
	Guard *budget.Guard

	// MinKappa is the floor. Zero means DefaultMinKappa.
	MinKappa float64

	// Level, Resamples and Seed configure the bootstrap. Zero values take the
	// stats/interval defaults.
	Level     float64
	Resamples int
	Seed      uint64
}

// Disagreement is one record the judge got wrong.
//
// This is the artifact that makes a prompt edit a directed act instead of a
// guess, so it carries the rubric and the judge's own rationale beside the two
// verdicts.
type Disagreement struct {
	RecordID  string
	Human     bool
	Judge     bool
	Rationale string
	Rubric    string
	Input     string
}

// Result is what one calibration run produced.
type Result struct {
	GoalName   string
	SetName    string
	SetVersion int
	SetSHA     string
	PromptSHA  string
	JudgeModel string
	Replay     bool

	Domain core.ScoreDomain

	NRecords int
	NScored  int
	NErrored int

	Agreement     Agreement
	KappaInterval *knov1.Interval
	InterHuman    Agreement
	Graded        *Graded

	MinKappa float64
	Verdict  string
	Cause    string
	Fix      string

	Disagreements []Disagreement

	// Judge and Errored are the per-record vectors, kept so the baseline can
	// record them and the ratchet can pair against them.
	Judge   []bool
	Errored []bool

	Ratchet *Ratchet

	Spend         budget.Spend
	Guarded       bool
	BudgetStopped bool
}

// Failed reports whether this result should fail a gate.
func (r *Result) Failed() bool {
	return r.Verdict == VerdictFail || r.Verdict == VerdictIndeterminate
}

// Calibrate scores every record with the Goal and reports its agreement with
// the human labels.
//
// It is Goal-agnostic on purpose: this is the harness and the gate, not a
// judge. The day the first judge prompt lands, the gate is already pointed at
// it rather than arriving later and grandfathering whatever shipped.
func Calibrate(ctx context.Context, opts Options) (*Result, error) {
	if opts.Goal == nil || opts.Set == nil {
		return nil, errs.ErrInvalidInput.
			WithFix("pass a Goal and a loaded calibration set").
			Wrap(errors.New("calibrate: nil goal or set"))
	}
	if opts.MinKappa == 0 {
		opts.MinKappa = DefaultMinKappa
	}

	res := &Result{
		GoalName:   opts.GoalName,
		SetName:    opts.Set.Name,
		SetVersion: opts.Set.Version,
		SetSHA:     opts.Set.ContentSHA256,
		PromptSHA:  PromptSHA(opts.Goal),
		Replay:     !opts.Live,
		Domain:     opts.Goal.Domain(),
		NRecords:   len(opts.Set.Records),
		MinKappa:   opts.MinKappa,
		Guarded:    opts.Guard != nil,
	}

	scores, err := scoreAll(ctx, opts, res)
	if err != nil {
		return nil, err
	}
	if res.BudgetStopped {
		// A kappa over the records that happened to fit under the cap is a
		// kappa over a population nobody chose. The count is reported and the
		// statistic is not.
		res.Verdict = VerdictIndeterminate
		res.Cause = fmt.Sprintf("the run stopped at its budget cap after %d of %d records",
			res.NScored, res.NRecords)
		res.Fix = "raise --max-cost-usd, or calibrate against a smaller set. " +
			"No agreement statistic is reported: one computed over the records that " +
			"happened to fit under a cap describes a population nobody chose"
		return res, nil
	}

	human := opts.Set.Reference()
	res.InterHuman = InterHuman(opts.Set.Records)
	collectDisagreements(opts.Set, scores, res)

	if res.Domain == knov1.ScoreDomain_SCORE_DOMAIN_UNIT_INTERVAL {
		res.Graded = gradeAll(opts.Set, scores, res.Errored)
		res.Verdict = VerdictNotApplicable
		res.Cause = "kappa is undefined on continuous scores"
		res.Fix = "gating a graded judge needs an anchored scale the calibration format " +
			"does not yet carry; see docs/debt.md#152"
		return res, nil
	}

	var judged, reference []bool
	for i := range opts.Set.Records {
		if res.Errored[i] {
			continue
		}
		judged = append(judged, res.Judge[i])
		reference = append(reference, human[i])
	}
	res.Agreement = Agree(judged, reference)
	res.KappaInterval = interval.Percentile(len(judged), func(idx []int) float64 {
		return KappaOver(judged, reference, idx)
	}, interval.Bootstrap{
		Resamples: opts.Resamples,
		Level:     opts.Level,
		Seed:      opts.Seed,
		Support:   &interval.Support{Low: -1, High: 1},
	})

	decide(res)
	return res, nil
}

// scoreAll runs the Goal over every record, through the guard when there is
// one, and records which records errored.
func scoreAll(ctx context.Context, opts Options, res *Result) ([]*core.Score, error) {
	scores := make([]*core.Score, len(opts.Set.Records))
	res.Judge = make([]bool, len(opts.Set.Records))
	res.Errored = make([]bool, len(opts.Set.Records))

	for i, rec := range opts.Set.Records {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("calibrating %s: %w", rec.ID, err)
		}
		score, err := scoreOne(ctx, opts, rec, res)
		if errors.Is(err, errs.ErrBudgetExceeded) {
			res.BudgetStopped = true
			break
		}
		if err != nil {
			if isFatal(err) {
				return nil, err
			}
			res.Errored[i] = true
			continue
		}
		if !inDomain(score, res.Domain) {
			// A value outside the declared Domain is an ERROR, not a verdict.
			// Reading a 0.5 as a pass on a binary Goal would launder a broken
			// judge into a number.
			res.Errored[i] = true
			continue
		}
		scores[i] = score
		res.Judge[i] = score.GetPassed()
		res.NScored++
		if score.GetJudgeModel() != "" {
			res.JudgeModel = score.GetJudgeModel()
		}
	}
	for _, e := range res.Errored {
		if e {
			res.NErrored++
		}
	}
	if opts.Guard != nil {
		res.Spend = opts.Guard.Spent()
	}
	return scores, nil
}

// scoreOne produces one judgement, from a fixture or from the Goal.
func scoreOne(ctx context.Context, opts Options, rec Record, res *Result) (*core.Score, error) {
	if !opts.Live && res.PromptSHA != NoPromptSHA {
		if opts.Fixtures == nil {
			return nil, errs.ErrInvalidInput.
				WithFix("pass --fixtures, or run `make record-calibration`").
				Wrap(fmt.Errorf("replaying a prompted goal with no fixture store"))
		}
		return opts.Fixtures.Score(opts.GoalName, res.PromptSHA, rec.ID)
	}

	costed, spends := opts.Goal.(Costed)
	if !spends || opts.Guard == nil {
		return opts.Goal.Score(ctx, rec.Case, rec.Response)
	}

	est, err := costed.EstimateScore(ctx, rec.Case)
	if err != nil {
		return nil, fmt.Errorf("estimating the judge call for %s: %w", rec.ID, err)
	}
	reservation, err := opts.Guard.Authorize(ctx, est)
	if err != nil {
		return nil, err
	}
	score, err := opts.Goal.Score(ctx, rec.Case, rec.Response)
	if err != nil {
		reservation.Release()
		return nil, err
	}
	reservation.Settle(budget.Spend{Calls: 1, CostUSDMicros: est.CostUSDMicros, Tokens: est.Tokens})
	return score, nil
}

// isFatal separates a broken run from a judge that failed on one record.
//
// A missing fixture, a refused registration and a cancelled context are the
// run being wrong. A judge returning an error on a record is DATA: it is
// excluded from kappa, counted separately, and reported.
func isFatal(err error) bool {
	return errors.Is(err, errs.ErrInvalidInput) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// inDomain checks a Score against the Domain its Goal declared.
func inDomain(s *core.Score, domain core.ScoreDomain) bool {
	v := s.GetValue()
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	switch domain {
	case knov1.ScoreDomain_SCORE_DOMAIN_BINARY:
		return v == 0 || v == 1
	case knov1.ScoreDomain_SCORE_DOMAIN_UNIT_INTERVAL:
		return v >= 0 && v <= 1
	default:
		return true
	}
}

// collectDisagreements records every record the judge and the humans differ on.
func collectDisagreements(set *Set, scores []*core.Score, res *Result) {
	for i, rec := range set.Records {
		if res.Errored[i] || res.Judge[i] == rec.Adjudicated.Passed {
			continue
		}
		var rationale string
		if scores[i] != nil {
			rationale = scores[i].GetRationale()
		}
		res.Disagreements = append(res.Disagreements, Disagreement{
			RecordID:  rec.ID,
			Human:     rec.Adjudicated.Passed,
			Judge:     res.Judge[i],
			Rationale: rationale,
			Rubric:    rec.Case.GetRubric(),
			Input:     rec.Case.GetInput(),
		})
	}
}

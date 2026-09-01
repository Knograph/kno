package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/judge"
	"github.com/knograph/kno/stats/budget"
)

// `kno judge calibrate` answers one question: is this judge good enough to
// trust a number produced through it?
//
// CLAUDE.md and CONTRIBUTING.md have both stated, in the present tense, that
// "judges are tested against the human-labeled calibration set with agreement
// thresholds"; DESIGN.md lists this command; what-the-numbers-mean.md tells
// the user to run it before trusting a judged number. None of it existed. This
// command is the mechanism those sentences describe, landed BEFORE the first
// judge prompt so the gate is already pointed at it rather than arriving later
// and grandfathering whatever shipped.
//
// It is Goal-agnostic. It takes any registered core.Goal and reports its
// agreement with human labels, which today means the mechanism is real and its
// content is vacuous: the only calibratable Goal in this build is exact-match,
// and the docs say exactly that rather than claiming coverage that does not
// exist.
//
// --replay is the default and makes NO provider call: no transport, no
// credential, no request. That is what makes it a contributor's first move
// rather than a thing they need a key for.

// judgeFlags are the options `kno judge calibrate` accepts.
type judgeFlags struct {
	goalName          string
	setPath           string
	setName           string
	fixturesPath      string
	baselinePath      string
	writeBaseline     bool
	all               bool
	live              bool
	replay            bool
	replaySet         bool
	minKappa          float64
	showDisagreements bool
	maxCostUSD        float64
	maxCalls          int64
	yes               bool
	jsonOut           bool
}

// newJudgeCmd builds `kno judge` and its one subcommand.
func newJudgeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "judge",
		Short: "Work with the judges that score outcomes",
		Long: `Judges are the epistemic foundation: every number produced through one is
bounded by how well it agrees with a human.

This build ships the calibration harness and the gate. It ships no judge --- the
only Goal it can calibrate is exact-match, which uses no model. When the first
judge prompt lands, the gate is already here.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newJudgeCalibrateCmd())
	return cmd
}

func newJudgeCalibrateCmd() *cobra.Command {
	var f judgeFlags

	cmd := &cobra.Command{
		Use:   "calibrate",
		Short: "Report how well a goal agrees with human labels",
		Long: `Score a human-labeled calibration set with a goal and report the agreement,
with its confidence interval, against a floor.

The gated statistic is Cohen's kappa, not accuracy. On a set that is 85% "good"
a judge that answers "good" unconditionally scores 0.85 raw agreement and is
worthless; it scores a kappa of exactly 0. Raw agreement is reported beside it
because it is the number a non-statistician reads correctly at a glance, and
seeing the two together is the lesson.

The floor is kappa >= 0.60, and it is derived rather than borrowed. On a
balanced set with symmetric error, kappa IS the factor by which the judge
attenuates every delta you measure through it --- so a judge at the floor
roughly triples the number of Cases you need for the same statistical power.
docs/what-the-numbers-mean.md carries the arithmetic.

Three verdicts, not two. An interval that straddles the floor is INDETERMINATE
and fails: "we cannot tell" is not "it is fine", and the fix is more records.

--replay is the default and costs nothing: no provider is contacted, no
credential is read, no request is made.

Examples:
  kno judge calibrate
  kno judge calibrate --goal exact-match --show-disagreements
  kno judge calibrate --set judge/testdata/calibration/straddle
  kno judge calibrate --json | jq -r .verdict`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f.replaySet = cmd.Flags().Changed("replay")
			return runJudgeCalibrate(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.goalName, "goal", "exact-match", "goal to calibrate")
	flags.StringVar(&f.setPath, "set", "",
		"directory holding a calibration set; empty uses the set built into this binary")
	flags.StringVar(&f.setName, "set-name", judge.DefaultSetName,
		"which built-in calibration set to use when --set is not given")
	flags.StringVar(&f.fixturesPath, "fixtures", "judge/testdata/fixtures",
		"recorded judge responses to replay")
	flags.StringVar(&f.baselinePath, "baseline", "",
		"compare against a recorded calibration and gate on the paired difference")
	flags.BoolVar(&f.writeBaseline, "write-baseline", false,
		"rewrite --baseline from this run. Review the diff like code")
	flags.BoolVar(&f.all, "all", false,
		"calibrate every (set, goal) pair listed in --baseline")
	flags.BoolVar(&f.replay, "replay", true,
		"score from recorded judge responses; makes no provider call")
	flags.BoolVar(&f.live, "live", false, "call the judge instead of replaying. Spends money")
	flags.Float64Var(&f.minKappa, "min-kappa", judge.DefaultMinKappa,
		"the agreement floor the interval is compared against")
	flags.BoolVar(&f.showDisagreements, "show-disagreements", false,
		"print every record the goal and the humans disagree on")
	flags.Float64Var(&f.maxCostUSD, "max-cost-usd", 0,
		"stop before spending more than this (0 is unlimited). --live only")
	flags.Int64Var(&f.maxCalls, "max-calls", 0,
		"stop after this many judge calls (0 is unlimited). --live only")
	flags.BoolVar(&f.yes, "yes", false, "skip the spend confirmation")
	flags.BoolVar(&f.jsonOut, "json", false, "machine-readable output")
	return cmd
}

// runJudgeCalibrate is the whole command.
func runJudgeCalibrate(ctx context.Context, out io.Writer, f judgeFlags) error {
	if err := f.validate(); err != nil {
		return err
	}

	baseline, err := f.loadBaseline()
	if err != nil {
		return err
	}

	targets, err := f.targets(baseline)
	if err != nil {
		return err
	}

	results := make([]*judge.Result, 0, len(targets))
	for _, t := range targets {
		res, err := f.calibrateOne(ctx, out, t, baseline)
		if err != nil {
			return err
		}
		results = append(results, res)
	}

	if f.writeBaseline {
		if err := rewriteBaseline(f.baselinePath, baseline, results); err != nil {
			return err
		}
	}

	if f.jsonOut {
		if err := writeJSON(out, newJudgeCalibrateJSON(results, f)); err != nil {
			return err
		}
	} else if err := renderCalibrations(out, results, f.showDisagreements); err != nil {
		return err
	}

	for _, res := range results {
		if res.BudgetStopped {
			return errs.ErrBudgetExceeded.Wrap(errors.New(res.Cause))
		}
	}
	for _, res := range results {
		if res.Failed() {
			return (&errs.Actionable{
				Code:     "CALIBRATION_FAILED",
				Message:  fmt.Sprintf("%s did not clear the agreement floor on %s", res.GoalName, res.SetName),
				Fix:      res.Fix,
				ExitCode: errs.ExitError,
			}).Wrap(errors.New(res.Cause))
		}
	}
	return nil
}

// target is one (goal, set) pair to calibrate.
type target struct {
	goalName string
	setPath  string
	setName  string
}

// validate refuses the flag combinations that would produce a number nobody
// asked for.
func (f judgeFlags) validate() error {
	// --replay defaults to true because a free, offline run is the right
	// default for the command a contributor reaches for first. That makes
	// "both were passed" invisible in the values alone, so the conflict is
	// detected from whether --replay was named explicitly.
	if f.live && f.replaySet && f.replay {
		return errs.ErrInvalidInput.
			WithFix("pass one of --replay or --live: two sources of truth for one number " +
				"is not a mode, it is a coin toss").
			Wrap(errors.New("--replay and --live are mutually exclusive"))
	}
	if !f.live {
		if f.maxCostUSD != 0 || f.maxCalls != 0 {
			return errs.ErrInvalidInput.
				WithFix("drop the cap, or pass --live: a replay makes no provider call, " +
					"so a spend cap on it would suggest there is spend to cap").
				Wrap(errors.New("--max-cost-usd and --max-calls apply to --live only"))
		}
	}
	if f.minKappa <= 0 || f.minKappa >= 1 {
		return errs.ErrInvalidInput.
			WithFix("--min-kappa is a floor on Cohen's kappa and lives in (0, 1); " +
				"the derived default is 0.60").
			Wrap(fmt.Errorf("--min-kappa is %.2f", f.minKappa))
	}
	if f.writeBaseline && f.baselinePath == "" {
		return errs.ErrInvalidInput.
			WithFix("pass --baseline naming the file to rewrite").
			Wrap(errors.New("--write-baseline needs --baseline"))
	}
	if f.all && f.baselinePath == "" {
		return errs.ErrInvalidInput.
			WithFix("pass --baseline: --all calibrates the pairs a baseline lists").
			Wrap(errors.New("--all needs --baseline"))
	}
	if f.maxCostUSD < 0 || f.maxCalls < 0 {
		return errs.ErrInvalidInput.
			WithFix("pass a positive cap, or omit it for no cap").
			Wrap(errors.New("a negative cap would disable the limit, not tighten it"))
	}
	return nil
}

// loadBaseline reads the recorded calibration, when one was named.
//
// A missing file is an error when the baseline is being READ — a gate whose
// reference is absent has nothing to gate against, and passing quietly is how
// a ratchet stops ratcheting. It is not an error when the run is WRITING one:
// that is how the first baseline comes to exist.
func (f judgeFlags) loadBaseline() (*judge.Baseline, error) {
	if f.baselinePath == "" {
		return nil, nil //nolint:nilnil // no baseline named is not an error
	}
	b, err := judge.LoadBaseline(f.baselinePath)
	if err != nil && f.writeBaseline && errors.Is(err, fs.ErrNotExist) {
		return &judge.Baseline{Path: f.baselinePath}, nil
	}
	return b, err
}

// targets resolves what to calibrate.
func (f judgeFlags) targets(baseline *judge.Baseline) ([]target, error) {
	if !f.all {
		return []target{{goalName: f.goalName, setPath: f.setPath, setName: f.setName}}, nil
	}
	if len(baseline.Entries) == 0 {
		return nil, errs.ErrInvalidInput.
			WithFix("record a calibration first: `make record-calibration`").
			Wrap(fmt.Errorf("%s lists no entries", baseline.Path))
	}
	out := make([]target, 0, len(baseline.Entries))
	for _, e := range baseline.Entries {
		out = append(out, target{goalName: e.GoalName, setName: e.SetName})
	}
	return out, nil
}

// calibrateOne runs the harness for one target.
func (f judgeFlags) calibrateOne(
	ctx context.Context, out io.Writer, t target, baseline *judge.Baseline,
) (*judge.Result, error) {
	g, err := resolveGoal(t.goalName)
	if err != nil {
		return nil, err
	}
	set, err := loadCalibrationSet(t)
	if err != nil {
		return nil, err
	}

	opts := judge.Options{
		Goal:     g,
		GoalName: t.goalName,
		Set:      set,
		Live:     f.live,
		MinKappa: f.minKappa,
	}
	if judge.PromptSHA(g) != judge.NoPromptSHA {
		opts.Fixtures = judge.NewFixtureStore(f.fixturesPath)
	}
	if f.live {
		opts.Guard = f.newGuard(out)
	}

	res, err := judge.Calibrate(ctx, opts)
	if err != nil {
		return nil, err
	}
	if baseline != nil {
		applyRatchet(baseline, set, res)
	}
	return res, nil
}

// newGuard builds the budget guard for a live run.
//
// Constructed only on the live path. A replay makes no provider call, so there
// is no spend to authorize and no guard: a guard on a path that cannot spend
// is a claim that it might.
func (f judgeFlags) newGuard(out io.Writer) *budget.Guard {
	return budget.New(
		budget.Limits{
			MaxCostUSDMicros: usdToMicros(f.maxCostUSD),
			MaxLLMCalls:      f.maxCalls,
		},
		confirmFunc(out, f.yes, f.jsonOut, &consentRecorder{}),
		usdToMicros(confirmThresholdUSD),
	)
}

// loadCalibrationSet reads a set from disk, or from the binary.
func loadCalibrationSet(t target) (*judge.Set, error) {
	if t.setPath != "" {
		return judge.Load(filepath.Clean(t.setPath))
	}
	name := t.setName
	if name == "" {
		name = judge.DefaultSetName
	}
	return judge.Builtin(name)
}

// applyRatchet compares a result against its recorded calibration.
func applyRatchet(baseline *judge.Baseline, set *judge.Set, res *judge.Result) {
	prev, ok := baseline.Find(set.Name, res.GoalName)
	if !ok {
		return
	}
	r := judge.CompareToBaseline(prev, set, res.Judge, res.Errored, defaultBootstrap())
	res.Ratchet = &r
	if prev.PromptSHA != res.PromptSHA {
		res.Ratchet.NotComparable = fmt.Sprintf(
			"the prompt moved: recorded %s, this run %s", prev.PromptSHA, res.PromptSHA)
		res.Ratchet.Comparable = false
	}
	if prev.JudgeModel != res.JudgeModel {
		res.Ratchet.ModelChanged = true
	}
	if r.Regressed && !res.Failed() {
		res.Verdict = judge.VerdictFail
		res.Cause = fmt.Sprintf(
			"kappa regressed against the recorded baseline: %.3f to %.3f, "+
				"paired 95%% CI on the difference [%.3f, %.3f]",
			r.BaselineKappa, r.Kappa, r.Diff.GetLow(), r.Diff.GetHigh())
		res.Fix = "improve the prompt, or record the new baseline in the same PR and say " +
			"in the body what was traded --- the same convention as .coverage-baseline"
	}
}

// rewriteBaseline records this run as the new reference.
//
// The escape hatch, and it is deliberately an escape hatch with an audit
// trail rather than a wall. A wall would be routed around by deleting the
// records a judge fails, which the set's content hash makes visible; a
// reviewable diff carrying the old kappa, the new kappa and the set version
// is the same convention .coverage-baseline already uses in this repository,
// with the same instruction: review it like code.
func rewriteBaseline(path string, prev *judge.Baseline, results []*judge.Result) error {
	entries := map[string]judge.BaselineEntry{}
	order := []string{}
	add := func(e judge.BaselineEntry) {
		key := e.SetName + "\x00" + e.GoalName
		if _, ok := entries[key]; !ok {
			order = append(order, key)
		}
		entries[key] = e
	}
	if prev != nil {
		for _, e := range prev.Entries {
			add(e)
		}
	}
	for _, res := range results {
		if res.BudgetStopped {
			continue
		}
		add(judge.BaselineEntry{
			SetName:       res.SetName,
			SetVersion:    res.SetVersion,
			ContentSHA256: res.SetSHA,
			GoalName:      res.GoalName,
			PromptSHA:     res.PromptSHA,
			JudgeModel:    res.JudgeModel,
			Kappa:         res.Agreement.Kappa,
			NRecords:      res.NRecords,
			Verdicts:      judge.VerdictVector(res.Judge, res.Errored),
		})
	}

	out := make([]judge.BaselineEntry, 0, len(order))
	for _, key := range order {
		out = append(out, entries[key])
	}
	b, err := judge.EncodeBaseline(out)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return errs.ErrInvalidInput.
			WithFix("check the path is writable").
			Wrap(fmt.Errorf("writing %s: %w", path, err))
	}
	return nil
}

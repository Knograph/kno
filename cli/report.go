package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
)

// reportFlags are the options `kno report` accepts.
type reportFlags struct {
	dbPath      string
	valueRunID  string
	selectRunID string
	exportRunID string
	watch       bool
	jsonOut     bool
}

func newReportCmd() *cobra.Command {
	var f reportFlags

	cmd := &cobra.Command{
		Use:   "report",
		Short: "The one-page verdict across the recorded stages",
		Long: `Compose the recorded stages into one page: the Baseline the Value run paired
against, every Asset's verdict with its interval, the Portfolio Select built
(with its dev estimate and the caveat that it is not yet validated on
holdout, and the rejection log by reason), and the gaps Export recorded: the
failure clusters nothing in the pool improved.

The page reads only recorded aggregates. No LLM calls, no evals re-read, no
trace content. A Baseline that Value's own rules would refuse as a reference
is reported as unusable, with the fix — never composed into a page that
claims a reference the record forbids.

--watch re-renders the page every 2 seconds while the Value run is not
terminal, and exits 0 the moment it is. It needs a terminal, and it cannot
combine with --json: a redraw loop cannot frame one JSON document per
snapshot.`,
		Example: `  # The whole story so far
  kno report --value-run-id <id> --select-run-id <id> --export-run-id <id>

  # Watch a live Value run reach its terminal status
  kno report --value-run-id <id> --watch`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReport(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.valueRunID, "value-run-id", "",
		"run ID of the recorded Value run to report on (required; run `kno value` first)")
	flags.StringVar(&f.selectRunID, "select-run-id", "",
		"run ID of the Select run whose Portfolio and rejection log the page shows")
	flags.StringVar(&f.exportRunID, "export-run-id", "",
		"run ID of the Export run whose gaps record the page renders")
	flags.StringVar(&f.dbPath, "db", "kno.db", "where runs and traces are stored")
	flags.BoolVar(&f.watch, "watch", false, "re-render every 2s until the Value run is terminal, then exit 0")
	flags.BoolVar(&f.jsonOut, "json", false, "machine-readable output")

	if err := cmd.MarkFlagRequired("value-run-id"); err != nil {
		panic(fmt.Sprintf("cli: marking --value-run-id required: %v", err))
	}
	return cmd
}

// reportWatchInterval is the redraw cadence of --watch: a plain ticker, not
// an event loop. The page is a document, not a UI; every 2 seconds is a
// deliberate compromise between "fresh" and "twitchy" for a loop that
// exists to notice a run becoming terminal.
const reportWatchInterval = 2 * time.Second

// runReport executes the report command.
func runReport(ctx context.Context, out io.Writer, f reportFlags) error {
	// The two refusals happen before any store work: a watch loop cannot
	// frame one JSON document per snapshot, and a redraw loop in a pipe is
	// garbage no reader asked for.
	if f.watch && f.jsonOut {
		return errs.ErrInvalidInput.
			WithFix("drop one of --watch or --json: a redraw loop cannot frame one JSON document per snapshot").
			Wrap(errors.New("report: --watch and --json cannot combine"))
	}

	db, err := store.NewSQLite(ctx, f.dbPath)
	if err != nil {
		return errs.ErrInvalidInput.WithFix("check --db is readable").Wrap(err)
	}
	defer func() { _ = db.Close() }()

	// Compose once before anything renders, so a refusal (unknown run, dirty
	// Baseline reference) fails fast in both modes instead of printing half
	// a page and then dying.
	data, err := composeReport(ctx, db, f)
	if err != nil {
		return err
	}
	if !f.watch {
		return renderReport(out, f.jsonOut, data)
	}
	if !watchTerminal(out) {
		return errWatchNeedsTerminal()
	}
	return watchReport(ctx, out, db, f, time.NewTicker(reportWatchInterval).C)
}

// watchTerminal reports whether --watch has a terminal to redraw into.
//
// stdout only: the watch reads nothing from stdin, so the consent prompt's
// both-ends rule does not apply. `kno report --watch < /dev/null` on a
// terminal is exactly the shape a wrapper script would use.
func watchTerminal(out io.Writer) bool {
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd())
}

// errWatchNeedsTerminal is the --watch refusal when stdout is not a
// terminal. Exit 2, the consent prompt's precedent for a terminal-only
// interaction that cannot happen: rendering snapshot after snapshot into a
// file would be a document nobody can read, and a pipe cannot host redraws.
func errWatchNeedsTerminal() error {
	a := &errs.Actionable{
		Code:     "WATCH_NEEDS_TERMINAL",
		Message:  "report: --watch needs a terminal to redraw into",
		Fix:      "run it in a terminal, or drop --watch — the one-shot page renders anywhere",
		ExitCode: 2,
	}
	return a.Wrap(errors.New("stdout is not a terminal"))
}

// watchReport re-renders the page every tick while the Value run is not
// terminal, and exits 0 the moment it is.
//
// Each render is a best-effort snapshot: the store's WAL never tears a row,
// so every read is internally consistent even though two consecutive reads
// can disagree about the run's progress. The final render — the one the
// watch exits on — is the authoritative page. A watched run that is already
// terminal renders once and exits 0.
func watchReport(ctx context.Context, out io.Writer, db store.Store, f reportFlags, tick <-chan time.Time) error {
	for {
		data, err := composeReport(ctx, db, f)
		if err != nil {
			// Refusals are stable facts about the store: a run that exists
			// does not stop existing, and a run that is not a Value run does
			// not become one. Fail rather than loop on a hard error.
			return err
		}
		if err := renderReport(out, false, data); err != nil {
			return err
		}
		if terminalStatus(data.ValueRun.GetStatus()) {
			return nil
		}
		select {
		case <-ctx.Done():
			return errs.ErrInterrupted.Wrap(fmt.Errorf(
				"the watch was interrupted before %s reached a terminal status",
				data.ValueRun.GetId(),
			))
		case <-tick:
		}
	}
}

// terminalStatus reports whether a run status is final. Only RUNNING is
// not: a run row whose status is unreadable (UNSPECIFIED) must not hang the
// watch forever, and the page's status cell already shows what it shows.
func terminalStatus(s knov1.RunStatus) bool {
	return s != knov1.RunStatus_RUN_STATUS_RUNNING
}

// reportData is one composed snapshot of the recorded stages.
//
// Everything here is an aggregate or a header: response blobs are trace
// content and never reach the page, in either renderer.
type reportData struct {
	ValueRun *knov1.Run
	// Baseline is the run the Value run paired against — the reference the
	// page's deltas mean anything against. Non-nil always: a dirty reference
	// is refused before the page composes.
	Baseline *knov1.Run
	// BaselineScore is the mean over the Baseline's readable scores; nil
	// when the reference recorded none.
	BaselineScore   *float64
	BaselineScored  int
	BaselineErrored int

	// Valuations is the Value run's complete recorded per-Asset record,
	// ordered by Asset ID.
	Valuations []*knov1.Valuation

	// SelectRun is nil when no Select run was named. Portfolio is nil when
	// the named run has not recorded one yet (a running Select run): the
	// page says so rather than refusing a snapshot.
	SelectRun *knov1.Run
	Portfolio *knov1.Portfolio

	// ExportRun is nil when no Export run was named. Gaps is nil when the
	// named run recorded none — "no cluster data for this run", never a
	// guess.
	ExportRun *knov1.Run
	Gaps      *knov1.Gaps

	// Spend is what the pipeline cost: Store.SettledSpend for each of the two
	// metered runs the page always names, plus the total. Read from the store
	// rather than from a guard, because report runs none and the processes
	// that did are gone. Composed once here so both renderers show the same
	// numbers.
	Spend reportSpend
}

// composeReport reads every recorded stage the flags name and composes the
// snapshot both renderers share.
func composeReport(ctx context.Context, db store.Store, f reportFlags) (*reportData, error) {
	d := &reportData{}

	valueRun, err := db.GetRun(ctx, f.valueRunID)
	if err != nil {
		return nil, errs.ErrInvalidInput.
			WithFix("run `kno value` first — every run ID the page shows comes from " +
				"the report lines of `kno baseline`, `kno value`, `kno select`, and `kno export`").
			Wrap(fmt.Errorf("loading value run %s: %w", f.valueRunID, err))
	}
	if got := valueRun.GetStage(); got != knov1.Stage_STAGE_VALUE {
		return nil, errs.ErrInvalidInput.
			WithFix("pass the run ID of a `kno value` run").
			Wrap(fmt.Errorf("run %s is a %s run, not a value run", f.valueRunID, got))
	}
	d.ValueRun = valueRun

	baseline, err := loadBaseline(ctx, db, valueRun.GetBaselineRunId())
	if err != nil {
		return nil, err
	}
	d.Baseline = baseline
	if scores, err := db.ScoreSum(ctx, baseline.GetId()); err != nil {
		return nil, fmt.Errorf("summing the baseline's scores: %w", err)
	} else if scores.Counted > 0 {
		mean := float64(scores.Sum) / float64(scores.Counted)
		d.BaselineScore = &mean
		d.BaselineScored = scores.Counted
	}
	// Errored travels separately: ScoreSum's denominator counts only scored
	// outcomes, and the page should say what the reference's error slice
	// was, not hide it. CaseObservations is the same aggregation the value
	// loop itself reads.
	if obs, err := db.CaseObservations(ctx, baseline.GetId()); err == nil {
		d.BaselineErrored = int(obs.Errored)
	}

	if d.Valuations, err = db.Valuations(ctx, f.valueRunID); err != nil {
		return nil, fmt.Errorf("loading the value run's valuations: %w", err)
	}

	if f.selectRunID != "" {
		selectRun, err := db.GetRun(ctx, f.selectRunID)
		if err != nil {
			return nil, errs.ErrInvalidInput.
				WithFix("run `kno select` first — run IDs come from the Select report line").
				Wrap(fmt.Errorf("loading select run %s: %w", f.selectRunID, err))
		}
		if got := selectRun.GetStage(); got != knov1.Stage_STAGE_SELECT {
			return nil, errs.ErrInvalidInput.
				WithFix("pass the run ID of a `kno select` run").
				Wrap(fmt.Errorf("run %s is a %s run, not a select run", f.selectRunID, got))
		}
		d.SelectRun = selectRun
		if p, err := db.Portfolio(ctx, f.selectRunID); err != nil {
			if !errors.Is(err, store.ErrPortfolioNotFound) {
				return nil, fmt.Errorf("loading the portfolio of %s: %w", f.selectRunID, err)
			}
			// Absent on a RUNNING Select run — it records when it finishes —
			// and on a run that never recorded one. The page says so; a
			// snapshot should not refuse over an artifact that exists for
			// the watching case.
		} else {
			// The chain is part of the honesty contract: a Portfolio whose
			// source is not the Value run this page reports on was built
			// against a different measurement set, and mixing the two would
			// splice two stories into one page.
			if src := p.GetSourceRunId(); src != "" && src != f.valueRunID {
				return nil, errs.ErrInvalidInput.
					WithFix("pass --select-run-id of a Select run built on the same Value run, " +
						"or drop --select-run-id to report without a Portfolio").
					Wrap(fmt.Errorf("select run %s was built on value run %s, not %s",
						f.selectRunID, src, f.valueRunID))
			}
			d.Portfolio = p
		}
	}

	// The pipeline total, composed before either renderer runs.
	//
	// Both stages here ran a budget guard, always: --value-run-id is REQUIRED
	// and loadBaseline refuses an empty baseline ID, so there is no page that
	// references zero metered runs. Select and Export are absent from the
	// object rather than present at zero — they run no guard, and a plausible
	// zero beside two real figures is exactly the confusion this reporting
	// exists to remove.
	//
	// A store error here is fatal rather than degraded. A cost figure is not
	// decoration: rendering the page with a silent zero because the sum could
	// not be read is how a report earns distrust.
	baselineSpend, err := db.SettledSpend(ctx, baseline.GetId())
	if err != nil {
		return nil, fmt.Errorf("reading the baseline run's settled spend: %w", err)
	}
	valueSpend, err := db.SettledSpend(ctx, f.valueRunID)
	if err != nil {
		return nil, fmt.Errorf("reading the value run's settled spend: %w", err)
	}
	d.Spend = newReportSpend(
		baseline.GetId(), baselineSpend,
		f.valueRunID, valueSpend,
		valueRun.GetIncompleteReason() != "" || baseline.GetIncompleteReason() != "",
	)

	if f.exportRunID != "" {
		exportRun, err := db.GetRun(ctx, f.exportRunID)
		if err != nil {
			return nil, errs.ErrInvalidInput.
				WithFix("run `kno export` first — run IDs come from the Export report line").
				Wrap(fmt.Errorf("loading export run %s: %w", f.exportRunID, err))
		}
		if got := exportRun.GetStage(); got != knov1.Stage_STAGE_EXPORT {
			return nil, errs.ErrInvalidInput.
				WithFix("pass the run ID of a `kno export` run").
				Wrap(fmt.Errorf("run %s is a %s run, not an export run", f.exportRunID, got))
		}
		d.ExportRun = exportRun
		if g, err := db.Gaps(ctx, f.exportRunID); err != nil {
			if !errors.Is(err, store.ErrGapsNotFound) {
				return nil, fmt.Errorf("loading the gaps of %s: %w", f.exportRunID, err)
			}
			// No cluster data for this run: the plan pre-dates the cluster
			// snapshot, or the source Value run recorded none. The page says
			// exactly that, never a guess.
		} else {
			d.Gaps = g
		}
	}

	return d, nil
}

// loadBaseline loads the reference and mirrors Value's fingerprint rules
// for whether it is usable as one.
//
// The rules are core/value.go's, read here so the report refuses the same
// Baseline Value would refuse. The report is a reader, not the referee:
// the fix lines point at the same remedies the stage names.
func loadBaseline(ctx context.Context, db store.Store, baselineRunID string) (*knov1.Run, error) {
	if baselineRunID == "" {
		return nil, errs.ErrInvalidInput.
			WithFix("run `kno baseline` and `kno value --baseline-run-id <id>` first").
			Wrap(errors.New("the value run paired against no baseline"))
	}
	run, err := db.GetRun(ctx, baselineRunID)
	if err != nil {
		return nil, errs.ErrInvalidInput.
			WithFix("run `kno baseline` first").
			Wrap(fmt.Errorf("loading baseline run %s: %w", baselineRunID, err))
	}
	if got := run.GetStage(); got != knov1.Stage_STAGE_BASELINE {
		return nil, errs.ErrInvalidInput.
			WithFix("pass the run ID of a `kno baseline` run").
			Wrap(fmt.Errorf("run %s is a %s run, not a baseline", baselineRunID, got))
	}
	// A baseline that ended because too many Cases errored is not a
	// reference: its recorded scores cover whichever Cases happened to
	// succeed, and deltas against them are measured on a slice selected by
	// transport failures.
	if run.GetErrorRateExceeded() || run.GetIncompleteReason() != "" {
		reason := run.GetIncompleteReason()
		if reason == "" {
			reason = "error rate exceeded"
		}
		return nil, errs.ErrInvalidInput.
			WithFix("re-run the baseline until it completes, then value against that run").
			Wrap(fmt.Errorf("baseline run %s is unusable as a reference: %s; its recorded "+
				"scores cover only the Cases that happened to succeed, so any delta "+
				"against them is measured on a slice selected by failures",
				baselineRunID, reason))
	}
	// A blended-model baseline is refused, never averaged with: the page
	// would compare every Asset against a control that was a different agent
	// on different Cases.
	if models := run.GetCaseExecution().GetResolvedModels(); len(models) > 1 {
		return nil, errs.ErrInvalidInput.
			WithFix("re-run the baseline pinned to one model before reporting").
			Wrap(fmt.Errorf("baseline run %s resolved %d models (%v); a delta against a "+
				"blended control is not an estimator of anything",
				baselineRunID, len(models), models))
	}
	return run, nil
}

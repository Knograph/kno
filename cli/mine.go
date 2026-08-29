package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/adapters/evals/mine"
	"github.com/knograph/kno/core/errs"
)

// mineFlags are the options `kno mine` accepts.
type mineFlags struct {
	logs              []string
	format            string
	agentName         string
	mode              string
	out               string
	review            bool
	minCases          int
	maxQuestionTokens int64
}

func newMineCmd() *cobra.Command {
	var f mineFlags

	cmd := &cobra.Command{
		Use:   "mine",
		Short: "Turn production transcripts into a weak-label eval set",
		Long: `Mine turns chat logs, ticket threads, and support conversations into
weak-label eval cases: wherever a human corrected the agent, that exchange
becomes a Case whose expected outcome is what the human said it should be.

The mined cases.jsonl is a WEAK-LABEL import, not a synthetic-data generator.
Every record carries provenance (derived, a derivation note, and the
transcript it came from) that survives ingestion, and ` + "`kno baseline`" + `
reports how many weak-label Cases a run measured — so a mined eval set cannot
pass for a hand-authored one.

Goal-mode declaration: mined Cases carry no rubric. They are honest under
judge goals; under exact-match they are honest only when --mode resolution
shaped the expected to a short answer, because a resolution is what the
agent had to converge to while an immediate reply was never told to it.

Modes:
  resolution (default)  a thread's final human message is the expected —
                        the answer that closed it
  immediate             the human reply after each agent answer is the
                        expected, with chit-chat filtered: gratitude,
                        acknowledgment, escalation, quote-back, retraction,
                        and counter-question are each counted and dropped

Formats (auto sniffs each file):
  jsonl-chat  one JSON message per line: id (required), role (assistant,
              agent, user, human), content, optional timestamp and
              thread_id
  markdown    **Speaker:** lines (--agent-name marks the agent), H1/---
              thread boundaries, optional ISO timestamps; prints a speaker
              inventory and a per-file pairing summary
  csv         a question,answer header row; missing columns are fatal

With --review, each mined case is presented for keep / edit / drop on the
terminal (refused without one); decisions are written to a manifest beside
the output, and re-mining reads the manifest back — a curated drop can
never resurrect.`,
		Example: `  # Mine a chat export into an eval set
  kno mine --logs ./convos/ --format auto --agent-name AI --out cases.jsonl

  # Review the mined cases before keeping them
  kno mine --logs chat.jsonl --format jsonl-chat --review --out cases.jsonl

  # Gate CI on the yield
  kno mine --logs ./convos --min-cases 50 --out cases.jsonl`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMine(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), f)
		},
	}

	flags := cmd.Flags()
	flags.StringSliceVar(&f.logs, "logs", nil, "transcript file or directory (repeatable) (required)")
	flags.StringVar(&f.format, "format", "auto", "transcript format: auto, jsonl-chat, markdown, or csv")
	flags.StringVar(&f.agentName, "agent-name", "", "the agent's speaker name in markdown transcripts (required for markdown)")
	flags.StringVar(&f.mode, "mode", "resolution", "how expectations are shaped: resolution or immediate")
	flags.StringVar(&f.out, "out", "cases.jsonl", "where to write the mined cases")
	flags.BoolVar(&f.review, "review", false, "review each mined case on the terminal before writing (requires a TTY)")
	flags.IntVar(&f.minCases, "min-cases", 0, "fail if fewer than this many cases are mined (for CI gating)")
	flags.Int64Var(&f.maxQuestionTokens, "max-question-tokens", mine.DefaultMaxQuestionTokens,
		"drop a question whose token count (the pricing approximation) exceeds this")
	return cmd
}

// stdinIsTTY is a seam for tests: the review loop must not be run against a
// pipe, and a real command reads the actual stdin's mode.
var stdinIsTTY = func() bool {
	st, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func runMine(ctx context.Context, in io.Reader, out, _ io.Writer, f mineFlags) error {
	if len(f.logs) == 0 {
		return errs.ErrInvalidInput.WithFix("pass --logs with a transcript file or directory").
			Wrap(fmt.Errorf("no --logs given"))
	}
	mode := mine.ModeResolution
	switch f.mode {
	case "resolution":
	case "immediate":
		mode = mine.ModeImmediate
	default:
		return errs.ErrInvalidInput.WithFix("pass --mode resolution or --mode immediate").
			Wrap(fmt.Errorf("unknown --mode %q", f.mode))
	}
	switch f.format {
	case "auto", "jsonl-chat", "markdown", "csv":
	default:
		return errs.ErrInvalidInput.WithFix("pass --format auto, jsonl-chat, markdown, or csv").
			Wrap(fmt.Errorf("unknown --format %q", f.format))
	}
	if f.maxQuestionTokens <= 0 {
		return errs.ErrInvalidInput.WithFix("pass a positive --max-question-tokens").
			Wrap(fmt.Errorf("--max-question-tokens %d is not a usable cap", f.maxQuestionTokens))
	}
	if f.review && !stdinIsTTY() {
		return errs.ErrInvalidInput.WithFix("run on a terminal, or drop --review to keep everything mined").
			Wrap(fmt.Errorf("--review needs a terminal to answer its keep/edit/drop prompts"))
	}

	manifestPath := f.out + ".review.json"
	cases, counts, err := mine.Mine(ctx, mine.Options{
		Logs:              f.logs,
		Format:            f.format,
		AgentName:         f.agentName,
		Mode:              mode,
		MaxQuestionTokens: f.maxQuestionTokens,
		Manifest:          manifestPath,
	})
	if err != nil {
		return classifyMineError(err)
	}
	if f.review {
		kept, _, err := mine.Review(cases, manifestPath, in, out)
		if err != nil {
			return classifyMineError(err)
		}
		cases = kept
	}

	if f.minCases > 0 && len(cases) < f.minCases {
		return errs.ErrInvalidInput.WithFix(
			fmt.Sprintf("mine more transcripts, or lower --min-cases to %d or fewer", len(cases)),
		).
			Wrap(fmt.Errorf("mined %d cases, below --min-cases %d", len(cases), f.minCases))
	}

	if err := writeMinedOutput(f.out, cases); err != nil {
		return err
	}
	printMineSummary(out, f.out, counts, len(cases))
	return nil
}

// classifyMineError maps a mine error onto the CLI's error grammar. The
// markdown --agent-name refusal names its own flag; everything else is an
// input problem in a named file.
func classifyMineError(err error) error {
	if errors.Is(err, mine.ErrAgentNameRequired) {
		return errs.ErrInvalidInput.
			WithFix("pass --agent-name with the agent's speaker name, exactly as the transcript writes it").
			Wrap(err)
	}
	return errs.ErrInvalidInput.WithFix("fix the reported file, then re-run").Wrap(err)
}

// writeMinedOutput writes the mined cases one per line.
//
// Written only after --min-cases has passed, so a failing gate leaves no
// half-set behind that a later pipeline step could pick up.
func writeMinedOutput(path string, cases []mine.Case) error {
	f, err := os.Create(path) //nolint:gosec // the output is an explicit user-supplied path: writing it is the command's contract
	if err != nil {
		return errs.ErrInvalidInput.WithFix("pass --out with a path the current user can write").
			Wrap(fmt.Errorf("creating %s: %w", path, err))
	}
	defer func() { _ = f.Close() }()
	for _, c := range cases {
		if err := mine.EncodeOutput(f, c); err != nil {
			return err
		}
	}
	return nil
}

// printMineSummary prints the run's ledger: what was mined, what was
// filtered (per modal class), what the cap and the dedup map dropped, what
// the review manifest preserved, the per-file pairing counts, and the
// markdown speaker inventory. A run that produced fewer cases than its
// transcripts promised is explained, not mysterious.
func printMineSummary(out io.Writer, outPath string, counts mine.Counts, total int) {
	if total == 0 {
		_, _ = fmt.Fprintf(out, "mined 0 Cases; this source has no weak labels\n")
		return
	}
	_, _ = fmt.Fprintf(out, "mined %d Cases into %s\n", total, outPath)

	var parts []string
	filteredTotal := 0
	// Iterate the classes in declaration order so the report is stable.
	for _, class := range []mine.FilterClass{
		mine.FilterGratitude, mine.FilterAcknowledgment, mine.FilterEscalation,
		mine.FilterQuoteBack, mine.FilterCounterQuestion, mine.FilterRetraction,
	} {
		if n := counts.Filtered[class]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, class))
			filteredTotal += n
		}
	}
	if filteredTotal > 0 {
		_, _ = fmt.Fprintf(out, "  filtered  %d human replies without answers: %s\n", filteredTotal, strings.Join(parts, ", "))
	}
	if counts.Deduped > 0 {
		_, _ = fmt.Fprintf(out, "  deduped   %d duplicate exchanges across the logs\n", counts.Deduped)
	}
	if counts.OverCap > 0 {
		_, _ = fmt.Fprintf(out, "  over-cap  %d questions exceeded the token cap and were dropped, never truncated\n", counts.OverCap)
	}
	if counts.PreservedDrops > 0 {
		_, _ = fmt.Fprintf(out, "  preserved %d curated drops from the review manifest\n", counts.PreservedDrops)
	}
	for _, p := range counts.Pairing {
		if p.AgentMessages == 0 && p.HumanReplies == 0 {
			continue
		}
		_, _ = fmt.Fprintf(out, "  %s: %d agent messages, %d human replies, %d mined (%d filtered)\n",
			p.Path, p.AgentMessages, p.HumanReplies, p.Mined, p.Filtered)
	}
	if len(counts.Speakers) > 0 {
		type named struct {
			name string
			n    int
		}
		var names []named
		for name, n := range counts.Speakers {
			names = append(names, named{name, n})
		}
		sort.Slice(names, func(i, j int) bool {
			if names[i].n != names[j].n {
				return names[i].n > names[j].n
			}
			return names[i].name < names[j].name
		})
		var spoken []string
		for _, s := range names {
			spoken = append(spoken, fmt.Sprintf("%s (%d)", s.name, s.n))
		}
		_, _ = fmt.Fprintf(out, "  speakers  %s\n", strings.Join(spoken, ", "))
	}
}

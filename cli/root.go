package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/core/errs"
)

// version is stamped at build time by goreleaser. "dev" when built by hand.
var version = "dev"

// NewRootCmd builds the kno command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "kno",
		Short: "Measure which of your data earns its place in an LLM agent",
		Long: `kno measures the marginal value of every data asset you are considering
feeding an LLM agent, then tells you which ones earn their place, what they
cost, and where each belongs.

The loop is: baseline, value, select, validate, export. Today only baseline
runs; the rest arrive milestone by milestone.`,
		Version:      version,
		SilenceUsage: true,
		// The top level renders errors in the CLI's grammar. Cobra printing
		// its own too would show the same failure twice, in two shapes.
		SilenceErrors: true,
	}

	root.AddCommand(newBaselineCmd())
	return root
}

// Execute runs the CLI and returns the process exit code.
//
// It returns the code rather than calling os.Exit so that main stays a
// one-liner and every path here is testable — an Execute that exited could
// only be tested by spawning a subprocess.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// Ctrl-C cancels the context rather than killing the process, so a run
	// checkpoints what it finished instead of losing it. A second Ctrl-C
	// restores the default behavior and exits immediately.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	if err := root.ExecuteContext(ctx); err != nil {
		renderError(stderr, err)
		return errs.ExitCodeOf(err)
	}
	return errs.ExitOK
}

// renderError prints a failure in the grammar: what failed, why, the fix.
//
// The same three-part shape errs.Actionable serializes for the API, so a
// terminal message and an SDK exception cannot drift apart.
func renderError(w io.Writer, err error) {
	// A failure to report a failure is not worth reporting: stderr is already
	// gone, and the exit code still carries the outcome.
	_, _ = fmt.Fprintf(w, "\nerror: %s\n", err.Error())
}

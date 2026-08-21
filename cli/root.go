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
	root.AddCommand(newPurgeCmd())
	return root
}

// Execute runs the CLI and returns the process exit code.
//
// It returns the code rather than calling os.Exit so that main stays a
// one-liner and every path here is testable — an Execute that exited could
// only be tested by spawning a subprocess.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// Ctrl-C cancels the context rather than killing the process, so a run
	// checkpoints what it finished instead of losing it.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)

	// A second Ctrl-C must kill the process. NotifyContext keeps swallowing
	// signals until stop is called, so deferring stop to the end of Execute
	// would silently eat every further Ctrl-C during the shutdown drain —
	// exactly when a user staring at an apparently hung command reaches for
	// it. Unregistering as soon as the first signal lands restores the default
	// behavior for the next one.
	// stop cancels ctx as well as unregistering, so the deferred call both
	// releases the watcher and covers the normal-exit path. Waiting keeps
	// Execute free of a goroutine still running after it returns.
	wait := restoreDefaultOnFirstSignal(ctx, stop)
	defer func() {
		stop()
		wait()
	}()

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

// restoreDefaultOnFirstSignal unregisters the signal handler as soon as ctx is
// cancelled, and returns a function that waits for that watcher to finish.
//
// Extracted so the behavior is testable without delivering a real second
// signal, which would kill the test process — which is also why the bug it
// fixes survived review the first time.
func restoreDefaultOnFirstSignal(ctx context.Context, stop context.CancelFunc) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		stop()
	}()
	return func() { <-done }
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

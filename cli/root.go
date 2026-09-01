package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/core/errs"
)

// version, commit and date are stamped at build time by goreleaser, via
// -X ldflags naming these symbols (see .goreleaser.yaml). A hand-built binary
// leaves them at their defaults and says "dev", which is the honest answer:
// nothing about it is reproducible from a tag.
//
// The linker addresses a symbol by its package path and name, not by Go
// visibility, so these stay unexported. Nothing outside this package should be
// able to read a build stamp as if it were API.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// buildIdentity is what a binary knows about its own origin.
type buildIdentity struct {
	// Version is the release tag, or "dev" for a build that came from neither
	// a release nor a module download.
	Version string
	// Commit is the revision it was built from, suffixed "-dirty" when the
	// working tree had uncommitted changes.
	Commit string
	// Date is the commit timestamp, not the build time — two builds of one
	// commit should not disagree about when it happened.
	Date string
}

// identity resolves the build stamp, preferring the ldflags a release sets and
// falling back to the module and VCS metadata the Go toolchain embeds.
//
// The fallback is the whole point. Most installs will be `go install`, which
// sets no ldflags — without this they would report "dev" forever, including in
// `kno doctor --json`, which exists to be pasted into a bug report. A version
// field that says "dev" for every non-release install is a support burden
// dressed up as a contract.
func identity() buildIdentity {
	id := buildIdentity{Version: version, Commit: commit, Date: date}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return id
	}

	// "(devel)" is what the toolchain reports for a local build. It is not a
	// version, and printing it would be a downgrade from "dev".
	if id.Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		id.Version = info.Main.Version
	}

	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if id.Commit == "" {
				id.Commit = s.Value
			}
		case "vcs.time":
			if id.Date == "" {
				id.Date = s.Value
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if dirty && id.Commit != "" {
		id.Commit += "-dirty"
	}
	return id
}

// String renders the identity for `kno --version`.
//
// `kno doctor --json` deliberately reports only the Version field. That field
// is a jq contract aimed at a person's pipeline (ADR-0001), and appending a
// commit hash to a value consumers parse as a version is a breaking change
// wearing a cosmetic disguise.
func (b buildIdentity) String() string {
	detail := make([]string, 0, 2)
	if b.Commit != "" {
		detail = append(detail, b.Commit)
	}
	if b.Date != "" {
		detail = append(detail, b.Date)
	}
	if len(detail) == 0 {
		return b.Version
	}
	return b.Version + " (" + strings.Join(detail, ", ") + ")"
}

// NewRootCmd builds the kno command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "kno",
		Short: "Measure which of your data earns its place in an LLM agent",
		Long: `kno measures the marginal value of every data asset you are considering
feeding an LLM agent, then tells you which ones earn their place, what they
cost, and where each belongs.

The loop is: baseline, value, select, validate, export. Which of them this
release implements is in the README's Status tables, and in docs/status.json
beside them --- one declaration, not a copy of the list maintained here.`,
		Version:      identity().String(),
		SilenceUsage: true,
		// The top level renders errors in the CLI's grammar. Cobra printing
		// its own too would show the same failure twice, in two shapes.
		SilenceErrors: true,
	}

	root.AddCommand(newInitCmd())
	root.AddCommand(newDemoCmd())
	root.AddCommand(newEvalCmd())
	root.AddCommand(newBaselineCmd())
	root.AddCommand(newMineCmd())
	root.AddCommand(newValueCmd())
	root.AddCommand(newSelectCmd())
	root.AddCommand(newValidateCmd())
	root.AddCommand(newExportCmd())
	root.AddCommand(newBridgeCmd())
	root.AddCommand(newReportCmd())
	root.AddCommand(newJudgeCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newPurgeCmd())
	return root
}

// Execute runs the CLI and returns the process exit code.
//
// It returns the code rather than calling os.Exit so that main stays a
// one-liner and every path here is testable — an Execute that exited could
// only be tested by spawning a subprocess. stdin is the consent prompt's and
// the wizard's input: a terminal in an interactive run, whatever the caller
// provides otherwise.
func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
	root.SetIn(stdin)
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

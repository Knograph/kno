package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Scheme is the agent-ref scheme this adapter serves.
const Scheme = agentref.SchemeExec

// DefaultTimeout bounds one invocation when Options.Timeout is zero.
//
// The same figure the HTTP transport applies to a provider call, applied for
// the same reason: a hung call costs the run's whole budget in wall-clock
// while paying nothing back, and a default means a hung script is bounded
// even by a user who did not think about deadlines.
const DefaultTimeout = 120 * time.Second

// DefaultOutputCapBytes is the default ceiling on a child's stdout.
//
// The same memory bound the HTTP transport applies to a provider response
// (32MiB), applied to the answer. Output beyond it is an errored Case naming
// the cap.
const DefaultOutputCapBytes = 32 << 20

// DefaultStderrCapBytes is the ceiling on the error context a child may store.
//
// Deliberately far smaller than the output cap: stderr becomes stored error
// text, and a script debug-printing its stdin must not be able to turn Case
// content into an error record.
const DefaultStderrCapBytes = 64 << 10

// Options configures the adapter.
type Options struct {
	// Command is the ref target, the string after "exec:".
	//
	// Split into argv on whitespace and spawned directly — no shell. A
	// command containing a shell metacharacter (`| & ; < > ( ) $ ` \`) is
	// refused here, at construction, before any Case runs.
	Command string

	// Env grants KEY=VALUE pairs appended to the child's environment
	// allowlist. Nothing is inherited beyond PATH, HOME, TMPDIR, and these
	// grants; a grant whose name collides with the allowlist overrides it.
	Env []string

	// Timeout bounds one invocation. Zero means DefaultTimeout.
	Timeout time.Duration

	// CostPerCallUSDMicros is the declared price of one invocation, stamped
	// on every Response with UsageEstimated. Zero declares the command free.
	CostPerCallUSDMicros int64

	// OutputCapBytes bounds one invocation's stdout. Zero means
	// DefaultOutputCapBytes.
	OutputCapBytes int64
}

// Agent invokes a command once per Case.
//
// Safe for concurrent use: everything it holds is computed once in New and
// read-only afterwards, and each Invoke spawns its own process.
type Agent struct {
	opts    Options
	argv    []string
	env     []string
	timeout time.Duration
	outCap  int64
}

// New validates the options and returns an Agent.
//
// Everything that can fail here fails here, before any Case runs: an empty
// command, a ref containing a shell metacharacter, a command not on the PATH,
// or a malformed env grant. Each refusal carries the fix line.
func New(opts Options) (*Agent, error) {
	command := strings.TrimSpace(opts.Command)
	if command == "" {
		return nil, errs.ErrInvalidInput.WithFix(
			"write the command after exec:, for example exec:my-agent --flag",
		).Wrap(fmt.Errorf("exec: no command"))
	}
	if err := checkMetachars(command); err != nil {
		return nil, err
	}
	argv := strings.Fields(command)
	if len(argv) == 0 {
		return nil, errs.ErrInvalidInput.WithFix(
			"write the command after exec:, for example exec:my-agent --flag",
		).Wrap(fmt.Errorf("exec: no command"))
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		return nil, errs.ErrInvalidInput.WithFix(fmt.Sprintf(
			"make %q findable on $PATH, or write it as a path the child can execute",
			argv[0],
		)).Wrap(fmt.Errorf("exec: %s is not on the PATH: %w", argv[0], err))
	}
	env, err := buildEnv(opts.Env)
	if err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout < 0 {
		return nil, errs.ErrInvalidInput.WithFix(
			"pass a positive --timeout",
		).Wrap(fmt.Errorf("exec: timeout %s is negative", timeout))
	}
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	outCap := opts.OutputCapBytes
	if outCap <= 0 {
		outCap = DefaultOutputCapBytes
	}
	return &Agent{opts: opts, argv: argv, env: env, timeout: timeout, outCap: outCap}, nil
}

// shellMetachars is the refused set.
//
// POSIX defines a metacharacter as one of:
// `| & ; < > ( ) $ \` " ' space tab newline`. Whitespace is this adapter's
// argument separator, and quotes are deliberately passed through literally
// (see splitCommand), so the refused set is the metacharacters minus those
// two families — the ones that can never mean anything here because there is
// no shell, and a user who typed one almost certainly expects the shell
// meaning. A silent difference is worse than a loud refusal.
const shellMetachars = "|&;<>()$`\\"

// checkMetachars refuses a command containing a shell metacharacter.
func checkMetachars(command string) error {
	for _, r := range command {
		if r == 0 {
			return errs.ErrInvalidInput.WithFix(
				"write the command as plain text; a NUL byte cannot be part of an argument",
			).Wrap(fmt.Errorf("exec: the command contains a NUL byte"))
		}
		if strings.ContainsRune(shellMetachars, r) {
			return errs.ErrInvalidInput.WithFix(
				"rewrite the command without shell operators — exec: runs the command " +
					"directly, with no shell, so a pipe, redirect, or expansion would be " +
					"passed through literally; wrap the logic in a script and name the script",
			).Wrap(fmt.Errorf("exec: the command contains %q, a shell metacharacter", r))
		}
	}
	return nil
}

// splitCommand turns the ref target into argv.
//
// The rule is deliberately not shell syntax: the string is split on runs of
// whitespace, and NOTHING else is interpreted. A quoted argument is not
// unquoted — `"hello world"` in the ref arrives as the two arguments `"hello`
// and `world"`, quote characters and all, which is what the help text
// promises. There is no shell between the ref and the process, so there is no
// way to express a shell-escaped or quoted argument; a command that needs one
// is a script, and the ref names the script.
func splitCommand(command string) []string { return strings.Fields(command) }

// envAllowlist is what a child receives before Options.Env grants.
//
// Deliberately small: PATH so the command can be resolved, HOME and TMPDIR
// because scripts and their children need them. Nothing else passes — a
// credential exported in the parent's environment must not be visible to the
// child, which is the plugin posture CLAUDE.md mandates and this adapter is
// the ancestor of.
var envAllowlist = []string{"PATH", "HOME", "TMPDIR"}

// buildEnv assembles the child's environment.
//
// The allowlist is snapshotted at construction, then grants are applied
// last-wins. The result is sorted so the child's environment reads
// identically on every run — environment ORDER is meaningless to a process,
// but a map's iteration order is not meaningless to a diff.
func buildEnv(grants []string) ([]string, error) {
	env := make(map[string]string, len(envAllowlist)+len(grants))
	for _, name := range envAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	for _, g := range grants {
		key, value, ok := strings.Cut(g, "=")
		if !ok || key == "" {
			// The grant is NOT echoed. A user who put a credential where a
			// variable name belongs would otherwise see it in the error,
			// which reaches stderr and therefore CI logs.
			return nil, errs.ErrInvalidInput.WithFix(
				"write each --exec-env grant as KEY=VALUE — a bare variable name is " +
					"refused on purpose, because a pass-through grant would re-open the " +
					"ambient-credential door the allowlist closes",
			).Wrap(fmt.Errorf("an --exec-env grant is not in KEY=VALUE form"))
		}
		env[key] = value
	}
	out := make([]string, 0, len(env))
	for k := range env {
		out = append(out, k+"="+env[k])
	}
	sort.Strings(out)
	return out, nil
}

// Invoke runs one Case against the command.
//
// One process per Case, no retry — exactly like a provider adapter makes one
// request per Case. A hung or deterministically-failing command is NOT
// retryable (core retries only ErrRateLimited and ErrTransportTransient, and
// nothing here wraps either): a local script that hung once will hang again,
// and retrying multiplies wall-clock by MaxAttempts for nothing.
func (a *Agent) Invoke(ctx context.Context, c *core.Case) (*core.Response, error) {
	if c == nil {
		return nil, fmt.Errorf("exec: nil case")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// The per-call deadline. The PARENT context is kept apart from the call
	// context because the two cancellations have opposite meanings: a parent
	// cancellation is a run shutting down and must stay unrecorded so --resume
	// picks the Case up, while a per-call timeout is a Case failure that must
	// be recorded as an errored Case.
	callCtx := ctx
	cancel := func() {}
	if a.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, a.timeout)
	}
	defer cancel()

	//nolint:gosec // G204: the argv is the user's OWN exec: ref, split by a documented rule and refused at construction when it contains shell metacharacters; arbitrary subprocess per Case is the entire contract of this adapter
	cmd := exec.CommandContext(callCtx, a.argv[0], a.argv[1:]...)
	cmd.Stdin = strings.NewReader(c.GetInput())
	stdout := newCapped(a.outCap)
	stderr := newCapped(DefaultStderrCapBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = a.env
	setpgid(cmd)
	// Own the kill: CommandContext's default cancels only the direct child,
	// leaving a script's children to outlive the deadline. TERM, then KILL
	// after a grace, at the process-group level.
	cmd.Cancel = func() error {
		killGroup(cmd, killGrace)
		return nil
	}
	cmd.WaitDelay = waitDelay

	start := time.Now()
	if err := cmd.Start(); err != nil {
		// Nothing ran: the binary was deleted or became unexecutable between
		// New's LookPath and this call. There is no output to record.
		return nil, ErrFailed.Wrap(fmt.Errorf("exec: %s: %w", a.argv[0], err))
	}
	waitErr := cmd.Wait()
	latency := time.Since(start)

	// A run shutting down, not a Case failing. Unwrapped, so core records
	// nothing and --resume picks the Case up.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if callCtx.Err() != nil {
		// The per-call deadline fired and the group kill ran. The command did
		// not answer, and no amount of retrying changes a script that hangs.
		out := a.failure(c, latency, stderr)
		return out, ErrTimedOut.WithFix(fmt.Sprintf(
			"raise --timeout (currently %s), or make the command finish faster; "+
				"a hung command is not retried",
			a.timeout,
		)).Wrap(fmt.Errorf("exec: %s did not finish within %s", a.argv[0], a.timeout))
	}

	if stdout.overflowed {
		out := a.failure(c, latency, stderr)
		out.Output = stdout.text()
		return out, ErrOutputTooLarge.WithFix(fmt.Sprintf(
			"make the command emit at most %d bytes of stdout per Case, or raise the "+
				"cap with Options.OutputCapBytes (default %d)",
			a.outCap, DefaultOutputCapBytes,
		)).Wrap(fmt.Errorf("exec: %s produced more than %d bytes of output",
			a.argv[0], a.outCap))
	}

	if waitErr != nil {
		out := a.failure(c, latency, stderr)
		return out, ErrFailed.Wrap(exitCause(a.argv, waitErr, stderr))
	}

	return &core.Response{
		CaseId:         c.GetId(),
		Output:         stdout.text(),
		LatencyMs:      latency.Milliseconds(),
		CostUsdMicros:  a.opts.CostPerCallUSDMicros,
		UsageEstimated: a.opts.CostPerCallUSDMicros > 0,
	}, nil
}

// failure builds the errored-Case response, carrying whatever the command
// already produced.
func (a *Agent) failure(c *core.Case, latency time.Duration, stderr *capped) *core.Response {
	return &core.Response{
		CaseId:         c.GetId(),
		Error:          stderr.text(),
		LatencyMs:      latency.Milliseconds(),
		CostUsdMicros:  a.opts.CostPerCallUSDMicros,
		UsageEstimated: a.opts.CostPerCallUSDMicros > 0,
	}
}

// exitCause renders what a failed command said, bounded.
//
// The exit status plus the capped stderr. The stderr is the only part a human
// can act on, and the cap is what keeps a script that debug-prints its stdin
// from turning Case content into stored error text.
func exitCause(argv []string, err error, stderr *capped) error {
	var ee *exec.ExitError
	status := "failed"
	if errors.As(err, &ee) {
		status = fmt.Sprintf("exited with status %d", ee.ExitCode())
	}
	msg := fmt.Sprintf("exec: %s %s", argv[0], status)
	if s := stderr.text(); s != "" {
		msg += ": " + s
	}
	if stderr.overflowed {
		msg += fmt.Sprintf(" [stderr truncated at %d bytes]", DefaultStderrCapBytes)
	}
	return errors.New(msg)
}

// capped is a writer that holds at most cap bytes and records overflow.
//
// The child must never block because we stopped reading: Write always
// consumes the whole slice, dropping whatever does not fit. Overflow is
// reported separately so the caller can decide what it means — for stdout it
// is an errored Case, for stderr it is a note that the context is incomplete.
type capped struct {
	buf        bytes.Buffer
	cap        int64
	overflowed bool
}

func newCapped(n int64) *capped { return &capped{cap: n} }

func (w *capped) Write(p []byte) (int, error) {
	room := w.cap - int64(w.buf.Len())
	if room <= 0 {
		w.overflowed = true
		return len(p), nil
	}
	if int64(len(p)) > room {
		w.buf.Write(p[:int(room)])
		w.overflowed = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

func (w *capped) text() string { return w.buf.String() }

// Spends reports whether this agent can cost the user money.
//
// True exactly when the user declared a per-Call cost; zero means free, and
// free is a DECLARATION, not a default — the same shape the fake uses, and
// the reason core's consent path asks nothing of a free exec run. The
// ranking consequence is stated in the package doc: a zero-cost arm is
// unranked-by-cost in Select, never silently outranked by a division by zero.
func (a *Agent) Spends() bool { return a.opts.CostPerCallUSDMicros > 0 }

// Capabilities reports what this adapter supports.
//
// ContextInject is deliberately NOT declared in v0.1: stdin already carries
// the Case, environment variables cannot carry an Asset at ~128KB, and the
// framing decision will be inherited by the Ring-2 plugin protocol —
// designing it now, before that protocol's shape exists, is the trap. The
// Value stage therefore refuses exec arms for injected measurement, which is
// correct.
func (a *Agent) Capabilities() *core.Capabilities {
	return &knov1.Capabilities{
		ContextInject:  false,
		KnowledgeWrite: false,
		Stream:         false,
		TokenCounts:    false,
	}
}

var (
	_ core.Agent   = (*Agent)(nil)
	_ core.Capable = (*Agent)(nil)
)

// Package exec is the Agent adapter behind a shell command: `exec:<command>`
// runs the command once per Case — the agent is "any executable".
//
// # The ref string becomes argv directly
//
// The command is the target of the ref, the string after "exec:". It is split
// into argv on whitespace and spawned DIRECTLY — no `sh -c`, no shell at all
// between the ref and the process. Shell interpretation of a ref is a
// command-injection door, so the split rule is deliberately NOT shell syntax
// and is pinned by tests: runs of whitespace separate arguments; quotes are
// not interpreted (a quoted argument arrives literally, quote characters and
// all); and a ref containing a shell metacharacter (`| & ; < > ( ) $ ` \`) is
// REFUSED at construction rather than run with a meaning the user did not
// intend. A command that needs pipes, redirection, expansion, or quoting is a
// script, and the ref names the script.
//
// # The child's environment is an allowlist
//
// The child receives exactly PATH, HOME, TMPDIR, and the Options.Env grants —
// nothing else. A key exported in the parent's environment must not be
// visible to the child; this adapter is the plugin protocol's ancestor, and
// CLAUDE.md's plugin posture ("plugins receive only what config explicitly
// grants them") is inherited here. There is no output redaction, because
// there are no ambient credentials to echo. For the same reason, and because
// exec reaches no URL, the transport's private-address and plain-HTTP checks
// do not apply here: there is no endpoint to check, and the env allowlist is
// the credential boundary. AllowPrivateAddress and AllowInsecureBaseURL are
// deliberately absent from Options.
//
// # Lifecycle
//
// Each Invoke spawns one process, in its own process group. On cancellation
// or a per-call timeout the whole group gets TERM, then KILL after a one
// second grace, so a script's children cannot outlive the deadline (Unix;
// Windows has no process groups, and the kill covers the direct child only —
// a documented platform difference). Stdout — the answer, judged by the Goal
// — is capped at DefaultOutputCapBytes; stderr — the error context — is
// capped at DefaultStderrCapBytes, small enough that a script debug-printing
// its stdin cannot turn Case content into stored error text. A nonzero exit
// is an errored Case with provider-failure classification, never a score of
// zero; output beyond the cap is an errored Case naming the cap. A hung or
// deterministically-failing command is NOT retryable: core retries only
// ErrRateLimited and ErrTransportTransient, and nothing here wraps either — a
// local script that hung once will hang again, and retrying multiplies
// wall-clock by MaxAttempts for nothing.
//
// # Cost and consent
//
// CostPerCallUSDMicros declares the price of one invocation; zero is a
// declaration that the command is free, not an absence of opinion. Spends()
// reports true exactly when a cost was declared, which is what makes core's
// consent path ask nothing of a free run. With a declared cost, every
// Response carries the scalar with UsageEstimated set, so the report and the
// settled spend are honest; without one, Responses carry zero and core's
// Value-stage ranking treats the arm as unranked-by-cost — a free arm cannot
// silently outrank paid arms, because there is no division by zero to outrank
// them (the bias class docs/debt.md#65 and #68 exist to make visible).
//
// # Capabilities
//
// ContextInject is deliberately NOT declared in v0.1: stdin already carries
// the Case, environment variables cannot carry an Asset at ~128KB, and the
// framing decision will be inherited by the Ring-2 plugin protocol —
// designing it now, before that protocol's shape exists, is the trap. The
// Value stage therefore refuses exec arms for injected measurement, which is
// correct.
//
// No estimator: exec is not a core.Estimator, and the planning path for a
// non-Estimator (the run-scoped --cost-per-call-usd scalar, via core's
// estimate fallback) is the one this adapter compiles against.
package exec

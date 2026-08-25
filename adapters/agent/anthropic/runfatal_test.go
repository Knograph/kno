package anthropic_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/knograph/kno/adapters/agent/anthropic"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
)

// runFatal reads the escalation the way core does — a structural assertion, so
// nothing crosses the package boundary in either direction.
func runFatal(err error) bool {
	var rf interface{ RunFatal() bool }
	return errors.As(err, &rf) && rf.RunFatal()
}

// invokeAgainst points an Agent at a server answering status with body.
func invokeAgainst(t *testing.T, status int, body string) error {
	t.Helper()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	a := newAgent(t, srv)

	_, err := a.Invoke(context.Background(), &core.Case{Id: "c", Input: "q", Expected: "a"})
	if err == nil {
		t.Fatalf("status %d produced no error", status)
	}
	return err
}

// errBody builds an Anthropic error envelope.
func errBody(kind, msg string, extra ...string) string {
	code := ""
	if len(extra) > 0 {
		code = fmt.Sprintf(`,"details":{"error_code":%q}`, extra[0])
	}
	return fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q%s}}`, kind, msg, code)
}

// TestTheConditionsThatCannotChangeWithinARunAreRunFatal repays
// docs/debt.md#47.
//
// core.IsFatal treated only ErrBudgetExceeded as fatal, so conditions that
// cannot change within a run were classified per-Case: a wrong
// ANTHROPIC_API_KEY on a 10,000-Case run made 10,000 requests and settled
// 10,000 calls against --max-calls before telling the user anything — which is
// precisely what ErrAuthentication's own godoc claims it prevents.
//
// The adapter classifies and core escalates: the adapter does not own the run,
// and core never saw the status code.
func TestTheConditionsThatCannotChangeWithinARunAreRunFatal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{
			name:   "a rejected credential",
			status: http.StatusUnauthorized,
			body:   errBody("authentication_error", "invalid x-api-key"),
			want:   true,
		},
		{
			name:   "a forbidden credential",
			status: http.StatusForbidden,
			body:   errBody("permission_error", "not permitted"),
			want:   true,
		},
		{
			name:   "an unpaid account",
			status: http.StatusPaymentRequired,
			body:   errBody("billing_error", "payment required"),
			want:   true,
		},
		{
			name:   "a model that does not exist",
			status: http.StatusNotFound,
			body:   errBody("not_found_error", "model not found"),
			want:   true,
		},
		{
			// Keyed on the STRUCTURED error_code, not on prose.
			name:   "the provider's own spend cap",
			status: http.StatusTooManyRequests,
			body:   errBody("rate_limit_error", "spend cap", "enforced_spend_limit_reached"),
			want:   true,
		},
		{
			// The one escalation resting on English prose. Accepted because it
			// fails OPEN: a reword drops back to per-Case handling, which is
			// the behavior that shipped before this. Ledgered as
			// docs/debt.md#61 with a canary as the repayment trigger.
			name:   "a self-set spend limit",
			status: http.StatusBadRequest,
			body:   errBody("invalid_request_error", "You have reached your specified API usage limits"),
			want:   true,
		},

		// The other direction, and the more expensive one to get wrong.
		// Escalating any of these converts a recoverable run into a dead one.
		{
			name:   "an ordinary rate limit",
			status: http.StatusTooManyRequests,
			body:   errBody("rate_limit_error", "slow down"),
			want:   false,
		},
		{
			name:   "a server error",
			status: http.StatusInternalServerError,
			body:   errBody("api_error", "internal"),
			want:   false,
		},
		{
			name:   "an overloaded provider",
			status: 529,
			body:   errBody("overloaded_error", "overloaded"),
			want:   false,
		},
		{
			name:   "a prompt past the context window",
			status: http.StatusRequestEntityTooLarge,
			body:   errBody("invalid_request_error", "prompt is too long"),
			want:   false,
		},
		{
			name:   "an ordinary refusal",
			status: http.StatusBadRequest,
			body:   errBody("invalid_request_error", "temperature must be <= 1"),
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := runFatal(invokeAgainst(t, tt.status, tt.body)); got != tt.want {
				t.Errorf("run-fatal = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestTheEscalationDoesNotDestroyTheCodeItWraps.
//
// This is the test docs/debt.md#39 exists for, and its absence is how that
// entry happened. A marker wrapper that omits Unwrap — or embeds an
// *errs.Actionable and inherits the promoted one, which returns the
// Actionable's OWN cause — jumps straight past the Actionable in the chain.
// errors.Is still answers through the promoted Is, so a sentinel check looks
// fine; errors.As(err, **errs.Actionable) answers FALSE, and core.codeOf then
// records a generic "AGENT_ERROR" in the persisted Outcome, on the event
// stream, and in --json.
//
// Measured consequence, from #39: "the type that exists to carry one extra
// fact silently destroyed the fact it was wrapping." Asserting RunFatal() is
// true does not catch it — the marker is the outermost type either way.
func TestTheEscalationDoesNotDestroyTheCodeItWraps(t *testing.T) {
	t.Parallel()

	err := invokeAgainst(t, http.StatusUnauthorized,
		errBody("authentication_error", "invalid x-api-key"))

	var a *errs.Actionable
	if !errors.As(err, &a) {
		t.Fatal("the run-fatal marker hid the Actionable: errors.As cannot " +
			"reach it, so the persisted Outcome, the event stream, and --json " +
			"all record a generic code instead of this adapter's")
	}
	if a.Code != anthropic.ErrAuthentication.Code {
		t.Errorf("code = %q, want %q — the escalation must add a fact, never "+
			"replace the classification", a.Code, anthropic.ErrAuthentication.Code)
	}

	// The exit code follows the same chain, so a CI gate branching on it sees
	// what the adapter classified rather than what the wrapper is.
	if got := errs.ExitCodeOf(err); got != anthropic.ErrAuthentication.ExitCode {
		t.Errorf("exit code = %d, want %d", got, anthropic.ErrAuthentication.ExitCode)
	}

	// And the user-facing message is unchanged: the marker carries a fact for
	// core, never a sentence for a human.
	if !errors.Is(err, anthropic.ErrAuthentication) {
		t.Error("the sentinel no longer matches")
	}
}

// TestARefusedDestinationIsRunFatalHereToo.
//
// Config is read once, so the policy that refused this request refuses every
// one after it. Untested until now: mutating the escalation to an identity
// function left the whole suite green, so this condition — listed in the
// CHANGELOG and in docs/debt.md#47 as one of the six — could have been deleted
// from both adapters without a failure.
func TestARefusedDestinationIsRunFatalHereToo(t *testing.T) {
	t.Parallel()

	elsewhere, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// 302, not 307. GetBody is cleared so net/http cannot replay a request,
	// and a 307/308 is therefore never followed — it comes back as the ANSWER
	// and CheckRedirect is never consulted.
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	})
	a := newAgent(t, srv)

	_, err := a.Invoke(context.Background(), &core.Case{Id: "c", Input: "q", Expected: "a"})
	if err == nil {
		t.Fatal("a cross-host redirect was followed")
	}
	if !runFatal(err) {
		t.Error("a refused destination is not run-fatal, so every remaining " +
			"Case re-offers the credential and gets the same refusal")
	}
	var act *errs.Actionable
	if !errors.As(err, &act) || act.Fix == "" {
		t.Error("the refusal carries no Actionable with a fix line")
	}
}

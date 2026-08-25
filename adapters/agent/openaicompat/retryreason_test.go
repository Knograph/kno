package openaicompat_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// reasonOf reads the retry reason an adapter attached, the same way core does —
// a structural assertion, so nothing crosses the package boundary.
func reasonOf(err error) (knov1.RetryReason, bool) {
	var rr interface{ RetryReason() knov1.RetryReason }
	if !errors.As(err, &rr) {
		return knov1.RetryReason_RETRY_REASON_UNSPECIFIED, false
	}
	return rr.RetryReason(), true
}

// invokeAgainstStatus points an Agent at a server that always answers status.
func invokeAgainstStatus(t *testing.T, status int) error {
	t.Helper()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"nope","type":"server_error"}}`))
	})
	a := newAgent(t, srv)

	_, err := a.Invoke(context.Background(), &core.Case{Id: "c", Input: "q", Expected: "a"})
	if err == nil {
		t.Fatalf("status %d produced no error", status)
	}
	return err
}

// TestA408ReportsATimeoutRatherThanAServerError repays docs/debt.md#53.
//
// A 408 and a 5xx share the ErrTransportTransient sentinel — both are "the
// provider failed to answer", both retryable — so core, which could classify
// only from the sentinel, reported a timed-out request as
// RETRY_REASON_PROVIDER_UNAVAILABLE, whose schema definition is "the provider
// returned a 5xx". RETRY_REASON_TIMEOUT was defined and nothing ever emitted
// it. The old label was vague; the replacement was specific and sometimes
// wrong, which is worse.
//
// The status code is knowledge only this layer has, so the reason rides on the
// error rather than being re-derived above.
func TestA408ReportsATimeoutRatherThanAServerError(t *testing.T) {
	t.Parallel()

	timeout := invokeAgainstStatus(t, http.StatusRequestTimeout)
	got, ok := reasonOf(timeout)
	if !ok {
		t.Fatal("a 408 carries no retry reason, so core still cannot tell it " +
			"from a 5xx")
	}
	if got != knov1.RetryReason_RETRY_REASON_TIMEOUT {
		t.Errorf("408 reason = %v, want TIMEOUT", got)
	}

	// A 5xx keeps the sentinel-derived classification. Attaching TIMEOUT there
	// would trade one wrong label for another.
	serverErr := invokeAgainstStatus(t, http.StatusBadGateway)
	if r, ok := reasonOf(serverErr); ok && r == knov1.RetryReason_RETRY_REASON_TIMEOUT {
		t.Error("a 502 was reported as a timeout")
	}

	// Both stay retryable. The reason is a label on a decision already made,
	// never the decision.
	for _, err := range []error{timeout, serverErr} {
		if !errors.Is(err, errs.ErrTransportTransient) {
			t.Errorf("%v stopped being retryable", err)
		}
	}

	// And neither is run-fatal: a provider that failed to answer once may
	// answer the next call, which is the whole difference from a rejected
	// credential.
	var rf interface{ RunFatal() bool }
	for _, err := range []error{timeout, serverErr} {
		if errors.As(err, &rf) && rf.RunFatal() {
			t.Errorf("%v was escalated to run-fatal; a transient failure must "+
				"not kill a recoverable run", err)
		}
	}
}

// TestARejectedCredentialIsRunFatalAndA404Too covers the escalations this
// adapter makes, and the ones it must not.
func TestARejectedCredentialIsRunFatalAndA404Too(t *testing.T) {
	t.Parallel()

	runFatal := func(err error) bool {
		var rf interface{ RunFatal() bool }
		return errors.As(err, &rf) && rf.RunFatal()
	}

	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{"a rejected credential", http.StatusUnauthorized, true},
		{"a forbidden credential", http.StatusForbidden, true},
		{"a model that does not exist", http.StatusNotFound, true},
		// The other direction, and the more expensive one to get wrong:
		// escalating these converts a recoverable run into a dead one.
		{"a server error", http.StatusBadGateway, false},
		{"a timeout", http.StatusRequestTimeout, false},
		{"a rate limit", http.StatusTooManyRequests, false},
		{"an ordinary refusal", http.StatusBadRequest, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := runFatal(invokeAgainstStatus(t, tt.status)); got != tt.want {
				t.Errorf("run-fatal = %v, want %v for status %d", got, tt.want, tt.status)
			}
		})
	}
}

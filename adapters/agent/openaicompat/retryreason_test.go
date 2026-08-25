package openaicompat_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/adapters/agent/openaicompat"
	"github.com/knograph/kno/adapters/agent/pricing"
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

// TestAKeyBoundElsewhereIsRunFatalAndCarriesAFix.
//
// A refused destination or a key bound to another host is config, and config is
// read once — so the policy that refused this request refuses every one after
// it. Untested until now: mutating the escalation to an identity function left
// the whole suite green, so "a refused destination or key-binding mismatch"
// could have been deleted from both adapters without a failure.
//
// The Actionable matters as much as the marker. As the RUN-ending error this
// reaches codeOf and ExitCodeOf, so a bare transport error would record
// "AGENT_ERROR" with the unclassified exit code and give the user no fix line
// for a misconfiguration that has an obvious one.
func TestAKeyBoundElsewhereIsRunFatalAndCarriesAFix(t *testing.T) {
	t.Parallel()

	// A server that redirects off its own host. The transport refuses to
	// follow it rather than re-offering the credential elsewhere.
	//
	// 302, not 307. GetBody is cleared so net/http cannot silently replay a
	// request, and without it a 307/308 is never followed at all — it comes
	// back as the ANSWER, so CheckRedirect is never consulted and this would
	// test the wrong path. The first version of this test used 307 and failed
	// for that reason.
	elsewhere := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	})
	a := newAgent(t, srv)

	_, err := a.Invoke(context.Background(), &core.Case{Id: "c", Input: "q", Expected: "a"})
	if err == nil {
		t.Fatal("a cross-host redirect was followed")
	}

	var rf interface{ RunFatal() bool }
	if !errors.As(err, &rf) || !rf.RunFatal() {
		t.Error("a refused destination is not run-fatal, so every remaining " +
			"Case re-offers the credential and gets the same refusal")
	}

	var act *errs.Actionable
	if !errors.As(err, &act) {
		t.Fatal("the refusal carries no Actionable, so it reaches the user as " +
			"AGENT_ERROR with the unclassified exit code and no fix line")
	}
	if act.Fix == "" {
		t.Error("the refusal names no fix")
	}
}

// TestAnUnpricedModelIsRunFatalHere covers the openaicompat half of
// docs/debt.md#46, which the ledger claimed for "both adapters" while only
// anthropic's was tested — mutating either of this package's two escalation
// sites left the suite green.
func TestAnUnpricedModelIsRunFatalHere(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// A model with no row in the price table, and no override supplied.
	ref, err := agentref.Parse("openai:model-not-in-the-table-9@" + srv.URL)
	if err != nil {
		t.Fatalf("parsing the agent ref: %v", err)
	}
	a, err := openaicompat.New(openaicompat.Options{
		Ref:                  ref,
		HTTPClient:           srv.Client(),
		MaxOutputTokens:      256,
		AllowInsecureBaseURL: true,
		AllowPrivateAddress:  true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = a.Estimate(context.Background(), &core.Case{Id: "c", Input: "q", Expected: "a"})
	if err == nil {
		t.Fatal("an unpriced model produced an estimate")
	}
	var rf interface{ RunFatal() bool }
	if !errors.As(err, &rf) || !rf.RunFatal() {
		t.Error("an unpriced model is not run-fatal here, so a capped run " +
			"refuses every Case one at a time and reports an error rate " +
			"rather than a pricing problem")
	}
	if !errors.Is(err, pricing.ErrUnpriced) {
		t.Errorf("the escalation destroyed the classification it wraps: %v", err)
	}
}

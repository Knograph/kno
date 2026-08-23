package anthropic_test

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/anthropic"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestAnUnpricedModelReportsMeasuredTokensAndDoesNotClaimAnEstimate.
//
// Two facts that must not be collapsed. The TOKENS are measured — the provider
// reported them — and only the RATE is missing, so the cost is unknown rather
// than predicted.
//
// usage_estimated says "cost_usd_micros is the engine's own prediction", and
// case.proto forbids pairing that flag with a zero ("never zero-cost", because
// a zero settlement is what made a dollar cap unenforceable in M1). Setting it
// here would be a false claim in exactly the direction the flag exists to
// prevent, so the code does not set it, and the two now agree.
//
// Reachable only with no dollar cap: Estimate reads the same table through the
// same fallback to the requested model, so a model this cannot price is one
// Estimate could not price either, and core.estimate refuses such a Case
// before Authorize whenever a cap is set.
func TestAnUnpricedModelReportsMeasuredTokensAndDoesNotClaimAnEstimate(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","model":"claude-not-in-the-table-9",
		            "content":[{"type":"text","text":"an answer"}],"stop_reason":"end_turn",
		            "usage":{"input_tokens":10,"output_tokens":4}}`)
	})
	a := newAgent(t, srv, func(o *anthropic.Options) { o.Model = "claude-not-in-the-table-9" })

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// The tokens are real and are reported; only the money is unknown.
	if resp.GetPromptTokens() != 10 || resp.GetCompletionTokens() != 4 {
		t.Errorf("tokens = %d/%d, want 10/4", resp.GetPromptTokens(), resp.GetCompletionTokens())
	}
	if resp.GetCostUsdMicros() != 0 {
		t.Errorf("cost = %d for a model with no price", resp.GetCostUsdMicros())
	}
	if resp.GetUsageEstimated() {
		t.Error("usage_estimated is set alongside a zero cost, which case.proto " +
			"forbids: the flag promises the number is the engine's prediction, and " +
			"there is no prediction for a model with no row in the table")
	}
}

// TestAnUnpricedModelIsAlsoUnestimable.
//
// The claim the test above rests on. If Estimate could price a model that
// settle cannot, the unpriced settlement path would be reachable WITH a cost
// cap — and a zero settlement under a cap is the M1 overshoot, restored.
func TestAnUnpricedModelIsAlsoUnestimable(t *testing.T) {
	t.Parallel()

	a := estimator(t, func(o *anthropic.Options) { o.Model = "claude-not-in-the-table-9" })
	if _, err := a.Estimate(t.Context(), aCase("q")); err == nil {
		t.Fatal("Estimate priced a model settle cannot, so an unpriced Case would " +
			"pass core's cap check and then settle at zero")
	}
}

// TestADatedModelIDPricesAsItsBaseRow.
//
// The one thing this adapter's cost figures assume about pricing.Lookup: a
// provider resolving `claude-sonnet-4-6` to a DATED id must reach the same
// table row, or every Case in a run where the alias resolves silently loses its
// price and settles at zero.
//
// Pinned as an equality rather than as "greater than zero", so a change to how
// suffixes are matched shows up here as a number rather than as a run that
// quietly stops costing anything. A version suffix inherits its base row; a
// VARIANT suffix (a word — "-fast", "-pro") is a different product at a
// different price, and is deliberately not asserted here because this adapter
// never requests one.
func TestADatedModelIDPricesAsItsBaseRow(t *testing.T) {
	t.Parallel()

	const usage = `"usage":{"input_tokens":1000,"output_tokens":1000}`
	// claude-sonnet-4-6 at $3 in / $15 out per MTok.
	const want = int64(3000 + 15000)

	for _, resolved := range []string{"claude-sonnet-4-6", "claude-sonnet-4-6-20260101"} {
		t.Run(resolved, func(t *testing.T) {
			t.Parallel()

			srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				answer(w, `{"id":"m","model":"`+resolved+`","stop_reason":"end_turn",`+
					`"content":[{"type":"text","text":"ok"}],`+usage+`}`)
			})
			a := newAgent(t, srv)

			resp, err := a.Invoke(t.Context(), aCase("q"))
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if resp.GetCostUsdMicros() != want {
				t.Errorf("cost = %d micro-USD, want %d; a dated id must reach the same "+
					"row as the alias it pins", resp.GetCostUsdMicros(), want)
			}
			if resp.GetUsageEstimated() {
				t.Error("a priced model with a real usage block was reported as estimated")
			}
		})
	}
}

// TestTerminalStatusesEachCarryTheirOwnFix.
//
// The fix line is the only part of an error a user can act on. One switch
// mixing classification with advice is where the two drift apart, so each
// branch is asserted for the thing it is supposed to say.
func TestTerminalStatusesEachCarryTheirOwnFix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		body    string
		wantFix string
	}{
		{
			name:    "payment required",
			status:  http.StatusPaymentRequired,
			body:    `{"type":"error","error":{"type":"billing_error","message":"card declined"}}`,
			wantFix: "payment details",
		},
		{
			name:    "unknown model",
			status:  http.StatusNotFound,
			body:    `{"type":"error","error":{"type":"not_found_error","message":"model: claude-typo"}}`,
			wantFix: "model name",
		},
		{
			name:    "request too large",
			status:  http.StatusRequestEntityTooLarge,
			body:    `{"type":"error","error":{"type":"request_too_large","message":"too big"}}`,
			wantFix: "context window",
		},
		{
			name:   "a spend limit the user set themselves",
			status: http.StatusBadRequest,
			body: `{"type":"error","error":{"type":"invalid_request_error",` +
				`"message":"You have reached your specified API usage limits. Access resumes 2026-09-01."}}`,
			wantFix: "spend limit",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			a := newAgent(t, srv)

			_, err := a.Invoke(t.Context(), aCase("q"))
			if !errors.Is(err, anthropic.ErrProvider) {
				t.Fatalf("err = %v, want ErrProvider", err)
			}
			if !strings.Contains(err.Error(), tc.wantFix) {
				t.Errorf("the fix does not mention %q: %v", tc.wantFix, err)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("the error does not carry the status: %v", err)
			}
		})
	}
}

// TestABlankHistoryTurnIsDroppedRatherThanSent.
//
// The API rejects an empty text block. A blank turn in an eval file is not
// worth failing a Case over, and sending it would fail every Case that has one.
func TestABlankHistoryTurnIsDroppedRatherThanSent(t *testing.T) {
	t.Parallel()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv)

	c := &knov1.Case{Id: "c", Input: "the question", History: []*knov1.Turn{
		{Role: knov1.Role_ROLE_USER, Content: ""},
		{Role: knov1.Role_ROLE_ASSISTANT, Content: ""},
	}}
	if _, err := a.Invoke(t.Context(), c); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	body := rec.body(t, 0)
	if strings.Contains(body, `"content":""`) {
		t.Errorf("an empty text block was sent, which the API rejects; body was %s", body)
	}
	if !strings.Contains(body, `"messages":[{"role":"user","content":"the question"}]`) {
		t.Errorf("dropping the blank turns did not leave the input; body was %s", body)
	}
}

// TestAHistoryTurnWithNoRoleIsRefused.
//
// A turn whose role is unspecified has no defined place in the conversation,
// and guessing one would send the provider a prompt the Case does not describe.
func TestAHistoryTurnWithNoRoleIsRefused(t *testing.T) {
	t.Parallel()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv)

	c := &knov1.Case{Id: "c", Input: "q", History: []*knov1.Turn{
		{Role: knov1.Role_ROLE_UNSPECIFIED, Content: "who said this?"},
	}}
	if _, err := a.Invoke(t.Context(), c); err == nil {
		t.Fatal("a turn with no role was accepted")
	}
	if rec.calls() != 0 {
		t.Error("a Case Kno will not send reached the network")
	}
}

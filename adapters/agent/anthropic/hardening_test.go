package anthropic_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestOnlyAPreOutputRefusalSettlesAtZero.
//
// "Pre-output" is the load-bearing half of the exemption, and it is the half
// nothing pinned before. Anthropic bills a MID-output refusal for the text it
// already produced; the two differ only in whether there is text, so a check
// that reads the usage block and ignores the body settles a real, billed
// refusal at zero.
//
// Both rows carry the SAME zero usage block, so the only thing that can make
// them differ is the text.
func TestOnlyAPreOutputRefusalSettlesAtZero(t *testing.T) {
	t.Parallel()

	const zeroUsage = `"usage":{"input_tokens":0,"output_tokens":0,` +
		`"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`

	tests := map[string]struct {
		content       string
		wantEstimated bool
	}{
		"no output at all":        {content: `[]`, wantEstimated: false},
		"output already produced": {content: `[{"type":"text","text":"I started to answer and then stopped."}]`, wantEstimated: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				answer(w, `{"id":"m","model":"claude-sonnet-4-6","stop_reason":"refusal",`+
					`"content":`+tc.content+`,`+zeroUsage+`}`)
			})
			a := newAgent(t, srv)

			resp, err := a.Invoke(t.Context(), aCase("q"))
			if err != nil {
				t.Fatalf("a refusal became an error: %v", err)
			}
			if !resp.GetRefused() {
				t.Fatal("refused is false")
			}
			if got := resp.GetUsageEstimated(); got != tc.wantEstimated {
				t.Errorf("usage_estimated = %v, want %v; a refusal that produced text was "+
					"billed for it, and a zero usage block cannot be taken at face value "+
					"when the body contradicts it", got, tc.wantEstimated)
			}
			if tc.wantEstimated && resp.GetCostUsdMicros() == 0 {
				t.Error("a refusal that produced billed output settled at zero")
			}
		})
	}
}

// TestAZeroSettlementNeedsInputTokensToBePresentNotJustZero.
//
// The exemption reads "input_tokens is present and explicitly zero". Absent is
// not zero: an empty usage object and a schema that moves the field under a
// wrapper both decode to zero, and inferring "not billed" from either settles
// every refused Case at $0 without even marking it inferred.
func TestAZeroSettlementNeedsInputTokensToBePresentNotJustZero(t *testing.T) {
	t.Parallel()

	for name, usage := range map[string]string{
		"an empty usage object":  `"usage":{}`,
		"input_tokens moved out": `"usage":{"input":{"tokens":0},"output_tokens":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				answer(w, `{"id":"m","model":"claude-sonnet-4-6","stop_reason":"refusal",`+
					`"content":[],`+usage+`}`)
			})
			a := newAgent(t, srv)

			resp, err := a.Invoke(t.Context(), aCase("q"))
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if !resp.GetUsageEstimated() {
				t.Error("a zero read out of an absent field was settled as a measurement")
			}
			if resp.GetCostUsdMicros() == 0 {
				t.Error("settled at zero on evidence that is not there")
			}
		})
	}
}

// TestARateLimitStillIdentifiesItselfAsAnActionable.
//
// core.codeOf reaches the Actionable with errors.As, not errors.Is. Embedding
// *errs.Actionable promotes its Unwrap, which returns the Actionable's own
// cause and jumps past the Actionable itself — so Is answers true, As answers
// false, and the persisted Outcome, the event, and --json all record
// "AGENT_ERROR" for every retry-exhausted rate-limited Case.
func TestARateLimitStillIdentifiesItselfAsAnActionable(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	})
	a := newAgent(t, srv)

	_, err := a.Invoke(t.Context(), aCase("q"))

	var act *errs.Actionable
	if !errors.As(err, &act) {
		t.Fatal("errors.As found no Actionable, so the code recorded on the Outcome " +
			"falls back to AGENT_ERROR for a Case that was rate limited")
	}
	if act.Code != errs.ErrRateLimited.Code {
		t.Errorf("code = %q, want %q", act.Code, errs.ErrRateLimited.Code)
	}
	// The extra fact the type exists to carry must survive alongside it.
	if _, ok := retryAfterOf(err); !ok {
		t.Error("the Retry-After was lost")
	}
}

// TestAProviderMessageAlwaysProducesAMarshalableResponse.
//
// Response.error is a proto3 string and protobuf-go REFUSES to marshal one
// carrying invalid UTF-8. Truncating at a byte offset splits a multi-byte rune
// and produces exactly that, so a provider message with a non-ASCII character
// near the boundary becomes a hard abort of a run whose money is already spent
// — the moment core carries the errored Response, which is the stated purpose
// of returning it.
//
// Padding is swept one byte at a time across the boundary rather than guessed,
// because the failure is an alignment coincidence and a single length misses it.
func TestAProviderMessageAlwaysProducesAMarshalableResponse(t *testing.T) {
	t.Parallel()

	for pad := 290; pad <= 310; pad++ {
		srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error",` +
				`"message":"` + strings.Repeat("a", pad) + `é and more text past the boundary"}}`))
		})
		a := newAgent(t, srv)

		resp, err := a.Invoke(t.Context(), aCase("q"))
		if err == nil {
			t.Fatal("a 400 produced no error")
		}
		if resp == nil {
			t.Fatal("no Response for a call the provider answered")
		}
		if _, mErr := proto.Marshal(resp); mErr != nil {
			t.Fatalf("pad=%d: the Response cannot be persisted or put on the wire: %v", pad, mErr)
		}
	}
}

// TestInvalidUTF8FromAProviderNeverReachesTheResponse.
func TestInvalidUTF8FromAProviderNeverReachesTheResponse(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// A lone continuation byte, written raw rather than through a JSON
		// escape, which is how a mis-encoded intermediary emits one.
		body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad `)
		body = append(body, 0x80, 0x80)
		body = append(body, []byte(`"}}`)...)
		_, _ = w.Write(body)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err == nil {
		t.Fatal("a 400 produced no error")
	}
	if resp == nil {
		t.Fatal("no Response for a call the provider answered")
	}
	if _, mErr := proto.Marshal(resp); mErr != nil {
		t.Fatalf("invalid UTF-8 reached Response.error: %v", mErr)
	}
}

// TestAChangedDetailsShapeDoesNotDiscardTheWholeErrorBody.
//
// One strict decode over a nested object throws away `type` and `message` too
// the moment `details` arrives as something else — silently disabling the
// spend-cap classification and degrading Response.error to a bare status. What
// the adapter branches on is decoded before the hint it merely reads.
func TestAChangedDetailsShapeDoesNotDiscardTheWholeErrorBody(t *testing.T) {
	t.Parallel()

	for name, details := range map[string]string{
		"details as a string": `,"details":"enforced_spend_limit_reached"`,
		"details as an array": `,"details":["a","b"]`,
		"details absent":      ``,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error",` +
					`"message":"prompt is too long: 250000 tokens > 200000 maximum"` + details + `}}`))
			})
			a := newAgent(t, srv)

			resp, err := a.Invoke(t.Context(), aCase("q"))
			if err == nil {
				t.Fatal("a 400 produced no error")
			}
			// The fix line depends on the message surviving the decode.
			if !strings.Contains(err.Error(), "context window") {
				t.Errorf("the message was discarded along with details, so the fix is gone: %v", err)
			}
			if !strings.Contains(resp.GetError(), "invalid_request_error") {
				t.Errorf("Response.error lost the provider's type: %q", resp.GetError())
			}
		})
	}
}

// TestASpendCapCodeIsStillReadWhenDetailsCarriesOtherKeys.
func TestASpendCapCodeIsStillReadWhenDetailsCarriesOtherKeys(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error",` +
			`"message":"monthly threshold crossed",` +
			`"details":{"resets_at":"2026-09-01","error_code":"enforced_spend_limit_reached"}}}`))
	})
	a := newAgent(t, srv)

	_, err := a.Invoke(t.Context(), aCase("q"))
	if errors.Is(err, errs.ErrRateLimited) {
		t.Error("a spend-cap 429 was classified as retryable; it never clears within the run")
	}
}

// TestAToolCallSurvivesAnUnreadableInput, so a shape change in a block this
// adapter only records cannot empty the record.
func TestAToolCallSurvivesAnUnreadableInput(t *testing.T) {
	t.Parallel()

	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, `{"id":"m","model":"claude-sonnet-4-6","stop_reason":"tool_use",
		            "content":[{"type":"tool_use","name":"search","input":"a bare string"}],
		            "usage":{"input_tokens":5,"output_tokens":3}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), aCase("q"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(resp.GetToolCalls()) != 1 {
		t.Fatalf("tool calls = %v", resp.GetToolCalls())
	}
	if resp.GetStopReason() != knov1.StopReason_STOP_REASON_TOOL_CALL {
		t.Errorf("stop_reason = %v, want TOOL_CALL", resp.GetStopReason())
	}
}

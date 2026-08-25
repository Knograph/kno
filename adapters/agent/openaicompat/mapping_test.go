package openaicompat_test

import (
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/adapters/agent/internal/transport"
	"github.com/knograph/kno/adapters/agent/openaicompat"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestToolCallsSurviveIntoTheResponse.
//
// BEHAVIOR Assets are the only Kind that faces the fine-tuning bridge, and tool
// use is one of the two things they exist to capture. Dropping the calls would
// leave the score as the only evidence of a Case the model answered by calling
// something, and a tool-call stop with an empty output would read as a blank
// answer rather than as a different kind of answer.
func TestToolCallsSurviveIntoTheResponse(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, http.StatusOK, `{"model":"m","choices":[{"index":0,"message":{
			"role":"assistant","content":"",
			"tool_calls":[
			  {"id":"call_1","type":"function","function":{"name":"lookup",
			   "arguments":"{\"q\":\"paris\"}"}},
			  {"id":"call_2","type":"function","function":{"name":"weather",
			   "arguments":"{}"}}
			]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":30,"completion_tokens":12}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), newCase("c", "hi"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got, want := resp.GetStopReason(), knov1.StopReason_STOP_REASON_TOOL_CALL; got != want {
		t.Errorf("stop_reason = %v, want %v", got, want)
	}
	calls := resp.GetToolCalls()
	if len(calls) != 2 {
		t.Fatalf("%d tool calls recorded, want 2", len(calls))
	}
	if got := calls[0].GetName(); got != "lookup" {
		t.Errorf("tool_calls[0].name = %q, want lookup", got)
	}
	if got := calls[0].GetArguments(); !strings.Contains(got, "paris") {
		t.Errorf("tool_calls[0].arguments = %q; the arguments are what a BEHAVIOR "+
			"Asset is measured against", got)
	}
}

// TestEveryHistoryRoleReachesTheWireAsItself.
//
// A history turn rewritten to another role is a different conversation than the
// one being scored, and the estimate still charged for the tokens either way —
// so the reservation and the request would describe different prompts.
func TestEveryHistoryRoleReachesTheWireAsItself(t *testing.T) {
	t.Parallel()

	var body string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 8192)
		n, _ := r.Body.Read(b)
		body = string(b[:n])
		jsonReply(w, http.StatusOK, answeredBody)
	})
	a := newAgent(t, srv)

	c := newCase("c", "the question")
	c.History = []*knov1.Turn{
		{Role: knov1.Role_ROLE_SYSTEM, Content: "sys-turn"},
		{Role: knov1.Role_ROLE_USER, Content: "user-turn"},
		{Role: knov1.Role_ROLE_ASSISTANT, Content: "asst-turn"},
		{Role: knov1.Role_ROLE_TOOL, Content: "tool-turn"},
		// An unset role is a defect in the Evals adapter. Dropped, it would
		// change the conversation; carried as "user" it is at worst mislabelled
		// and at best exactly right, which is the recoverable direction.
		{Role: knov1.Role_ROLE_UNSPECIFIED, Content: "unset-turn"},
	}
	if _, err := a.Invoke(t.Context(), c); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	for _, want := range []string{
		`"role":"system","content":"sys-turn"`,
		`"role":"user","content":"user-turn"`,
		`"role":"assistant","content":"asst-turn"`,
		`"role":"tool","content":"tool-turn"`,
		`"role":"user","content":"unset-turn"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the request does not carry %s\ngot: %s", want, body)
		}
	}
}

// TestEveryFinishReasonTheProvidersUseIsRecognized.
//
// An unrecognized finish reason becomes STOP_REASON_UNSPECIFIED, which reads
// downstream as "the provider did not say" — indistinguishable from a provider
// that genuinely did not. The Anthropic spellings are here because several
// OpenAI-compatible gateways proxy Anthropic models and pass the upstream
// reason through unchanged.
func TestEveryFinishReasonTheProvidersUseIsRecognized(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		finish string
		want   knov1.StopReason
	}{
		{"stop", knov1.StopReason_STOP_REASON_STOP},
		{"end_turn", knov1.StopReason_STOP_REASON_STOP},
		{"length", knov1.StopReason_STOP_REASON_LENGTH},
		{"max_tokens", knov1.StopReason_STOP_REASON_LENGTH},
		{"tool_calls", knov1.StopReason_STOP_REASON_TOOL_CALL},
		{"function_call", knov1.StopReason_STOP_REASON_TOOL_CALL},
		{"content_filter", knov1.StopReason_STOP_REASON_CONTENT_FILTER},
		{"something_new", knov1.StopReason_STOP_REASON_UNSPECIFIED},
	} {
		t.Run(tc.finish, func(t *testing.T) {
			t.Parallel()

			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				jsonReply(w, http.StatusOK, `{"model":"m","choices":[{"index":0,`+
					`"message":{"role":"assistant","content":"x"},"finish_reason":"`+
					tc.finish+`"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
			})
			a := newAgent(t, srv)

			resp, err := a.Invoke(t.Context(), newCase("c", "hi"))
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if got := resp.GetStopReason(); got != tc.want {
				t.Errorf("stop_reason = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAbsurdTokenCountsSaturateRatherThanWrap.
//
// A provider is free to report any number it likes, and int64 multiplication
// wraps. A wrapped product lands SMALL AND POSITIVE, which reads as a cheap
// call rather than as nonsense — nothing downstream would catch it, and
// budget.validate rejects negatives, not wrapped ones. Saturating is the only
// answer that stays in the safe direction.
func TestAbsurdTokenCountsSaturateRatherThanWrap(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, http.StatusOK, `{"model":"m","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":9223372036854775807,`+
			`"completion_tokens":9223372036854775807}}`)
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), newCase("c", "hi"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := resp.GetCostUsdMicros(); got != math.MaxInt64 {
		t.Errorf("cost_usd_micros = %d, want saturation at %d. A wrapped product "+
			"reads as a cheap call and the guard would authorize the next one",
			got, int64(math.MaxInt64))
	}
}

// TestNewRefusesADestinationItWillNotSendTo.
//
// The refusals live in the transport, and this asserts the adapter routes to
// them rather than reimplementing or bypassing them. A second copy of the
// address rules here would be a second thing to keep correct, and the one that
// drifts is the one that ships.
func TestNewRefusesADestinationItWillNotSendTo(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, ref string
		insecure  bool
		private   bool
	}{
		{
			"plain HTTP without the opt-in",
			"openai:m@http://example.com/v1",
			false, false,
		},
		{
			"a private address without the opt-in",
			"openai:m@https://10.0.0.1/v1",
			false, false,
		},
		{
			// Refused with no override at all: 169.254.169.254 is where cloud
			// instance metadata lives, and this tool persists response bodies.
			"link-local, which has no opt-in",
			"openai:m@https://169.254.169.254/v1",
			true, true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ref, err := agentref.Parse(tc.ref)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.ref, err)
			}
			if _, err := openaicompat.New(openaicompat.Options{
				Ref:                  ref,
				AllowInsecureBaseURL: tc.insecure,
				AllowPrivateAddress:  tc.private,
			}); err == nil {
				t.Fatalf("New accepted %s", tc.ref)
			} else if !errors.Is(err, transport.ErrRefusedDestination) {
				t.Errorf("err = %v, want ErrRefusedDestination; the adapter is "+
					"deciding this rather than delegating it", err)
			}
		})
	}
}

// TestAnAgentRefWithNoModelIsRefused.
//
// `fake:` is the one scheme whose target is optional, and it does not reach
// here. An empty model would be sent to the provider verbatim and come back as
// a 400 the user pays for, once per Case.
func TestAnAgentRefWithNoModelIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := openaicompat.New(openaicompat.Options{
		Ref: &core.AgentRef{Scheme: agentref.SchemeOpenAI},
	}); !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("New error = %v, want ErrInvalidInput", err)
	}
}

// TestAnEmptyReplyBodyIsAnErrorNotAnEmptyAnswer.
//
// A zero-length 200 is what a misconfigured proxy returns. Scored, it is a
// wrong answer attributed to the model.
func TestAnEmptyReplyBodyIsAnErrorNotAnEmptyAnswer(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	})
	a := newAgent(t, srv)

	if _, err := a.Invoke(t.Context(), newCase("c", "hi")); err == nil {
		t.Error("an empty body was accepted as an answer")
	}
}

// TestANilCaseIsRefusedRatherThanPanicking.
//
// The executor recovers a panic in one item so it does not kill the run, which
// means a panic here becomes one failed Case and a stack trace in a log — a
// bug that is expensive to find. A refusal names itself.
func TestANilCaseIsRefusedRatherThanPanicking(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {})
	a := newAgent(t, srv)

	if _, err := a.Invoke(t.Context(), nil); !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("Invoke(nil) error = %v, want ErrInvalidInput", err)
	}
	if _, err := a.Estimate(t.Context(), nil); !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("Estimate(nil) error = %v, want ErrInvalidInput", err)
	}
}

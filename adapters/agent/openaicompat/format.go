package openaicompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// This is the ONE file in the package that speaks the provider's JSON.
//
// encoding/json is banned repo-wide (see .golangci.yml) because kno.v1 types
// must be marshaled with protojson, and the exemption is scoped to
// adapters/**/format.go. Keeping every wire struct and every Marshal call here
// is what makes that exemption true rather than merely configured: nothing
// else in this package can decode a provider payload without going through a
// named type a reviewer can read against the provider's schema.

// chatRequest is the Chat Completions request body.
//
// Every optional field is a pointer with omitempty, because "not sent" and
// "sent as zero" are different requests. temperature=0 is a real, meaningful
// value; a non-pointer float64 would send it on every call, and OpenAI's
// reasoning models answer any non-default temperature with a 400 — every Case
// errored, the error-rate threshold tripped, and the user told "too many cases
// errored for this to be a usable baseline", which names nothing about the
// cause.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`

	// MaxCompletionTokens is the current spelling and the one OpenAI's
	// reasoning models require.
	MaxCompletionTokens *int64 `json:"max_completion_tokens,omitempty"`

	// MaxTokens is the legacy spelling. llama.cpp, LM Studio, and older
	// self-hosted servers know only this one, and OpenAI rejects it on
	// reasoning models — so which is sent is a per-endpoint choice
	// (Options.UseLegacyMaxTokens), never both. An output ceiling that is
	// silently dropped by the server is not a cosmetic problem: it is the
	// output term of every reservation, so the estimate would bound something
	// the request no longer bounds.
	MaxTokens *int64 `json:"max_tokens,omitempty"`

	Temperature *float64 `json:"temperature,omitempty"`
	Seed        *int64   `json:"seed,omitempty"`

	// Stream is sent explicitly as false rather than omitted. Streaming is
	// unimplemented (docs/debt.md#35) and a compatible server that defaults to
	// streaming would hand this adapter an SSE body it would report as
	// malformed JSON — a confusing failure for a setting we can simply state.
	Stream bool `json:"stream"`
}

// chatMessage is one message in either direction.
//
// Content is a string, which is what the assistant role returns. A provider
// answering with the array-of-parts form fails to decode and is reported as a
// malformed body — visibly, rather than by silently scoring an empty answer.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`

	// Refusal is OpenAI's structured decline. Present and non-empty means the
	// model declined on policy grounds; it is NOT an error, it is a scored
	// Case with Response.refused set. See mapResponse.
	Refusal string `json:"refusal,omitempty"`

	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
}

// wireToolCall is one tool invocation as the provider reports it.
type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// chatUsage is the provider's own token accounting.
//
// The block itself is a pointer on chatResponse: a missing usage block and a
// usage block of zeros settle differently. Missing means "settle at the
// reservation and mark it estimated"; a claim of zero is the shape that made a
// dollar cap unenforceable in M1, and usableUsage refuses it.
type chatUsage struct {
	PromptTokens        int64                `json:"prompt_tokens"`
	CompletionTokens    int64                `json:"completion_tokens"`
	TotalTokens         int64                `json:"total_tokens"`
	PromptTokensDetails *promptTokensDetails `json:"prompt_tokens_details"`
}

type promptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type chatResponse struct {
	ID                string       `json:"id"`
	Model             string       `json:"model"`
	SystemFingerprint string       `json:"system_fingerprint"`
	Choices           []chatChoice `json:"choices"`
	Usage             *chatUsage   `json:"usage"`

	// Error is how several compatible providers report a failure inside a 200.
	// Reading it is not optional: a 200 whose body is an error object would
	// otherwise be scored as an empty answer, and an account misconfigured that
	// way would produce a complete baseline of 0.000 with a clean error rate.
	Error *wireError `json:"error"`
}

// wireError is a provider error object.
//
// Code is deliberately json.RawMessage: OpenAI sends a string, Azure and
// several gateways send a number, and a `string` field turns the second into a
// whole-body decode failure — losing the message that says what went wrong.
type wireError struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Param   string          `json:"param"`
	Code    json.RawMessage `json:"code"`
}

// code renders the error code as text, whatever shape it arrived in.
func (e *wireError) code() string {
	if e == nil || len(e.Code) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(e.Code, &s); err == nil {
		return s
	}
	return strings.Trim(string(e.Code), `"`)
}

// encodeRequest marshals a request body.
func encodeRequest(r *chatRequest) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: encoding the request: %w", err)
	}
	return b, nil
}

// decodeResponse parses a provider reply.
//
// Strict about trailing content — json.Decoder plus More() — because a body
// with a second JSON document appended is a provider doing something this
// adapter must not silently average over. json.Unmarshal accepts only the
// first value and ignores nothing after it, which would score half a reply.
func decodeResponse(body []byte) (*chatResponse, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var out chatResponse
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: %s", errMalformedBody, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: the body carries more than one JSON document",
			errMalformedBody)
	}
	return &out, nil
}

// decodeError parses an error body on a best-effort basis.
//
// Best-effort on purpose: the HTTP status already classifies the failure, and
// a provider whose error body does not parse must still produce the status's
// error rather than a second, less useful one about JSON.
func decodeError(body []byte) *wireError {
	var env struct {
		Error *wireError `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error != nil {
		return env.Error
	}
	return nil
}

// roleOf maps a Turn's role onto the wire.
//
// ROLE_UNSPECIFIED becomes "user" rather than being dropped: a history turn
// with no role is a defect in the Evals adapter, and dropping it would change
// the conversation the model sees while the estimate still charged for it —
// so the reservation and the request would describe different prompts.
func roleOf(r knov1.Role) string {
	switch r {
	case knov1.Role_ROLE_SYSTEM:
		return "system"
	case knov1.Role_ROLE_ASSISTANT:
		return "assistant"
	case knov1.Role_ROLE_TOOL:
		return "tool"
	case knov1.Role_ROLE_USER, knov1.Role_ROLE_UNSPECIFIED:
		return "user"
	default:
		return "user"
	}
}

// stopReasonOf maps finish_reason onto the schema's enum.
//
// "length" is the one a naive adapter drops. It is a well-formed 200 with
// valid JSON and a truncated answer: scored as a wrong answer, it means Kno's
// own max_output_tokens silently depresses the baseline with nothing in the
// output saying so.
//
// "end_turn" and "max_tokens" are Anthropic's spellings, accepted here because
// several OpenAI-compatible gateways proxy Anthropic models and pass the
// upstream finish reason through unchanged.
func stopReasonOf(finish string) knov1.StopReason {
	switch finish {
	case "stop", "end_turn":
		return knov1.StopReason_STOP_REASON_STOP
	case "length", "max_tokens":
		return knov1.StopReason_STOP_REASON_LENGTH
	case "tool_calls", "function_call":
		return knov1.StopReason_STOP_REASON_TOOL_CALL
	case "content_filter":
		return knov1.StopReason_STOP_REASON_CONTENT_FILTER
	default:
		return knov1.StopReason_STOP_REASON_UNSPECIFIED
	}
}

// toolCallsOf converts the wire's tool calls.
func toolCallsOf(in []wireToolCall) []*knov1.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]*knov1.ToolCall, 0, len(in))
	for _, tc := range in {
		out = append(out, &knov1.ToolCall{
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return out
}

// decodeUsage pulls a usage block out of any body, on a best-effort basis.
//
// It exists for the FAILURE paths. A 200 carrying an error object, and a 200
// carrying no choices, both arrive with the provider's own usage block already
// in the payload — the provider generated something, billed for it, and then
// told us it went wrong. Discarding that block records a paid call as free.
//
// Best-effort like decodeError: the status and the error object already
// classify the failure, so a body that will not parse must still produce the
// original error rather than a second one about JSON.
func decodeUsage(body []byte) *chatUsage {
	var env struct {
		Usage *chatUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err == nil {
		return env.Usage
	}
	return nil
}

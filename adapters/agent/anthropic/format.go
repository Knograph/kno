// This file holds the Anthropic Messages API wire format, and it is the only
// file in this package that touches encoding/json.
//
// ADR-0001 bans encoding/json because proto3 JSON encodes int64 as quoted
// strings and enums as names, so using it on a kno.v1 type silently diverges
// from the generated OpenAPI spec. That reasoning is about kno.v1 types. These
// are a PROVIDER's wire shapes decoded into plain Go structs — no proto message
// is involved, and protojson would force Anthropic's field names to mirror
// ours, which they do not: `input_tokens` is not `prompt_tokens`, and the
// difference between the two is most of the reason this adapter exists.
//
// The exemption is scoped to files named format.go under adapters/, so it
// cannot quietly spread to code that touches kno.v1 types.

package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Roles the Messages API accepts inside `messages`.
//
// There is no "system" role here on purpose. Anthropic takes the system prompt
// as a TOP-LEVEL field; sending it as a message is the likeliest way to mistake
// this API for an OpenAI-compatible one, and it fails as a 400 on every Case
// rather than as one loud error.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// stop_reason values the Messages API returns.
//
// Enumerated rather than compared inline so the mapping to StopReason and the
// refusal handling read against the same constants.
const (
	stopEndTurn      = "end_turn"
	stopMaxTokens    = "max_tokens"
	stopStopSequence = "stop_sequence"
	stopToolUse      = "tool_use"
	stopRefusal      = "refusal"
	stopPauseTurn    = "pause_turn"
	stopContextLimit = "model_context_window_exceeded"
)

// messagesRequest is the POST /v1/messages body.
//
// Note what is absent: no `cache_control`, anywhere, ever. docs/debt.md#41
// records that Price carries ONE cache-write rate while Anthropic publishes two
// (5-minute at 1.25x base input, 1-hour at 2x) and the table records the
// 5-minute one. That entry is inert only while no adapter writes to the cache.
// Setting cache_control here would settle 1-hour writes at the 5-minute rate —
// an UNDER-charge, which prime directive 4 calls a P0.
type messagesRequest struct {
	Model     string `json:"model"`
	MaxTokens int64  `json:"max_tokens"`

	// System is the top-level system prompt. Omitted when empty: `"system": ""`
	// is not the same as sending no system prompt, and it perturbs the cache
	// prefix for nothing.
	System string `json:"system,omitempty"`

	Messages []message `json:"messages"`

	// Temperature is a pointer because 0 is a meaningful value and the field
	// must be ABSENT — not zero — on models that reject sampling parameters
	// with a 400. See samplingRemoved.
	Temperature *float64 `json:"temperature,omitempty"`
}

// message is one entry in the Messages API's alternating message list.
type message struct {
	Role string `json:"role"`

	// Content is the plain-string form, which the API accepts for text-only
	// turns. M2 sends no images, documents, or tool blocks, so the block-array
	// form would be ceremony around a string.
	Content string `json:"content"`
}

// messagesResponse is a 200 from POST /v1/messages.
type messagesResponse struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Role  string `json:"role"`
	Model string `json:"model"`

	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`

	// Usage is a pointer so an absent block is distinguishable from a block of
	// zeros. They settle differently: absent means "estimate and mark it", and
	// all-zero on a refusal means the provider genuinely billed nothing.
	Usage *usage `json:"usage"`
}

// contentBlock is one block of a response's content array.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// Name and Input carry a tool_use block. M2 sends no tools, so these are
	// only populated by a provider doing something we did not ask for —
	// recorded rather than dropped, because a silently discarded tool call
	// reads downstream as an empty answer.
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// errorEnvelope is the decoded body of a non-2xx response.
//
// A flat struct rather than a mirror of the wire shape, because the wire shape
// is decoded in stages. See decodeError.
type errorEnvelope struct {
	// Type is the envelope's own type, always "error" today.
	Type string

	// ErrorType is error.type — "rate_limit_error", "overloaded_error".
	ErrorType string

	// Message is error.message, unsanitized. Every reader of it sanitizes.
	Message string

	// ErrorCode is error.details.error_code, empty when absent or unreadable.
	// It distinguishes a spend-cap 429 — which never succeeds on retry — from
	// an ordinary one, which does.
	ErrorCode string
}

// encodeRequest marshals a request body.
func encodeRequest(r *messagesRequest) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("anthropic: encoding the request: %w", err)
	}
	return b, nil
}

// decodeResponse parses a 200 body.
//
// Unknown fields are IGNORED, unlike the eval file formats, which reject them.
// The reasoning inverts with who owns the schema: a user's file with a
// misspelled key is a mistake worth naming, while a provider adding a field is
// routine and monthly. Rejecting one would turn every Anthropic release into a
// run that errors every Case.
func decodeResponse(body []byte) (*messagesResponse, error) {
	var m messagesResponse
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("anthropic: decoding the response body: %w", err)
	}
	return &m, nil
}

// decodeError parses an error body, best effort.
//
// Best effort because an error body is exactly where a gateway substitutes an
// HTML page for the provider's JSON. A nil return means "no structured error",
// which the caller reports as the status alone rather than failing twice.
func decodeError(body []byte) *errorEnvelope {
	var outer struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &outer); err != nil || len(outer.Error) == 0 {
		return nil
	}

	// Staged on purpose. One strict Unmarshal over a nested details object
	// discards `type` and `message` too the moment `details` arrives as a
	// string or an array, which would silently disable the spend-cap
	// classification, the context-window fix line, and the self-set-spend-limit
	// fix line, and degrade Response.error to a bare "HTTP 429". What this
	// adapter BRANCHES on is decoded first; the hint it merely reads is decoded
	// last, and its failure costs the hint alone.
	var inner struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(outer.Error, &inner); err != nil {
		return nil
	}
	if inner.Type == "" && inner.Message == "" {
		return nil
	}

	e := &errorEnvelope{Type: outer.Type, ErrorType: inner.Type, Message: inner.Message}
	if len(inner.Details) > 0 {
		var d struct {
			ErrorCode string `json:"error_code"`
		}
		if err := json.Unmarshal(inner.Details, &d); err == nil {
			e.ErrorCode = d.ErrorCode
		}
	}
	return e
}

// text concatenates the response's text blocks.
//
// Every text block, not the first. A response legitimately splits into several
// — thinking summaries, citation-bearing segments — and taking content[0].text
// silently truncates the answer, which then scores as a wrong one.
func (m *messagesResponse) text() string {
	var b strings.Builder
	for i := range m.Content {
		if m.Content[i].Type == "text" {
			b.WriteString(m.Content[i].Text)
		}
	}
	return b.String()
}

// toolCalls reports any tool_use blocks the provider returned.
func (m *messagesResponse) toolCalls() []*knov1.ToolCall {
	var out []*knov1.ToolCall
	for i := range m.Content {
		if m.Content[i].Type != "tool_use" {
			continue
		}
		out = append(out, &knov1.ToolCall{
			Name:      m.Content[i].Name,
			Arguments: string(m.Content[i].Input),
		})
	}
	return out
}

// stopReasonOf maps Anthropic's stop_reason onto the schema's enum.
//
// max_tokens is the one that costs money to get wrong. It is a well-formed 200
// carrying a truncated answer, so an adapter that drops it lets Kno's own
// --max-output-tokens depress the baseline invisibly: the answer scores as
// wrong and nothing in the record says it was cut off.
//
// refusal maps to CONTENT_FILTER and is NEVER an error. An account whose safety
// settings decline every Case would otherwise produce 100% scored Cases, an
// aggregate of 0.000, and a clean error rate — a confident reference number for
// a run in which the agent was never measured. Response.refused is the
// authoritative flag; this enum is the secondary record.
func stopReasonOf(s string) knov1.StopReason {
	switch s {
	case stopEndTurn, stopStopSequence, stopPauseTurn:
		return knov1.StopReason_STOP_REASON_STOP
	case stopMaxTokens, stopContextLimit:
		return knov1.StopReason_STOP_REASON_LENGTH
	case stopToolUse:
		return knov1.StopReason_STOP_REASON_TOOL_CALL
	case stopRefusal:
		return knov1.StopReason_STOP_REASON_CONTENT_FILTER
	default:
		// Including "": a provider that reports nothing is not the same as one
		// reporting the model finishing on its own, and claiming STOP would
		// hide a truncation behind a plausible-looking record.
		return knov1.StopReason_STOP_REASON_UNSPECIFIED
	}
}

// maxProviderMessage bounds how much of a provider's error text is quoted.
const maxProviderMessage = 300

// sanitize makes a provider-supplied string safe to put in an error, a log
// line, and a persisted Outcome.
//
// Two hazards, both real. A provider's 400 message quotes parts of the request
// back, and a Case is customer data; and any string rendered with %s into a log
// line is a log-injection primitive the moment it carries a newline. So control
// characters collapse to spaces and the whole thing is truncated.
func sanitize(s string) string {
	// Control characters first, so a newline cannot inject a line into a log
	// this string is rendered into with %s.
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)

	// A plain byte cut, repaired immediately below. Deliberately NOT a
	// rune-boundary search: two mechanisms that each independently guarantee
	// valid UTF-8 mean neither one can be removed by a test, and the one left
	// standing would be whichever a future edit did not touch.
	if len(s) > maxProviderMessage {
		s = s[:maxProviderMessage] + "..."
	}

	// The single guarantee, unconditional and last. Response.error is a proto3
	// string and protobuf-go REFUSES to marshal one carrying invalid UTF-8, so
	// a mis-encoded byte from a provider — or a byte cut through a multi-byte
	// rune, one byte of padding away in the line above — is a hard abort of a
	// run that has already spent its money. Latent today only because core
	// discards the errored Response; it stops being latent the moment
	// docs/debt.md#43 is repaid, which is the stated purpose of returning it.
	//
	// strings.Map above also happens to fold invalid bytes into U+FFFD, but
	// that is incidental behaviour of ranging over a string rather than a
	// documented property of Map, and it cannot repair a cut that happens
	// after it runs. This line is what the guarantee rests on.
	return strings.ToValidUTF8(s, "")
}

// This file holds the AWS Bedrock Converse wire format, and it is the only
// file in this package that touches encoding/json.
//
// Same exemption as the anthropic adapter's format.go: these are a PROVIDER's
// wire shapes decoded into plain Go structs, no kno.v1 type is involved, and
// protojson would force Bedrock's camelCase field names to mirror ours, which
// they do not — `inputTokens` is not `input_tokens`, and the difference
// between the two APIs is most of the reason this adapter exists.
//
// The exemption is scoped to files named format.go under adapters/, so it
// cannot quietly spread to code that touches kno.v1 types.

package bedrock

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Roles Converse accepts inside `messages`.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// stop reasons Converse returns.
//
// Enumerated rather than compared inline so the mapping to StopReason and the
// refusal handling read against the same constants. The family differs from
// the Messages API's: there is no "refusal" stop reason on Converse — the
// model's safety classifiers report `content_filtered` instead, and Bedrock
// guardrails report `guardrail_intervened`. Both suppress what the model would
// otherwise have produced, so both score as a refused Case.
const (
	stopEndTurn           = "end_turn"
	stopMaxTokens         = "max_tokens"
	stopStopSequence      = "stop_sequence"
	stopToolUse           = "tool_use"
	stopContentFiltered   = "content_filtered"
	stopGuardrail         = "guardrail_intervened"
	stopModelContextLimit = "model_context_window_exceeded"
)

// converseRequest is the POST /model/{modelId}/converse body.
//
// The model id is NOT here — it lives in the URL path, percent-encoded. The
// inference settings nest under `inferenceConfig`, and messages are
// content-block arrays rather than plain strings.
//
// Deliberately absent: `seed`. It does not exist in inferenceConfig
// (docs/plans/2026-08-29-bedrock-vertex-agents.md P0-3), and inventing a
// parameter the endpoint rejects is how a run 400s every Case.
type converseRequest struct {
	Messages []converseMessage `json:"messages"`

	// System is an ARRAY of text blocks on Converse, not a top-level string.
	// Omitted when empty for the same reason the Messages API's is: a present
	// empty array is not the same as no system prompt.
	System []textBlock `json:"system,omitempty"`

	// InferenceConfig nests maxTokens, temperature, topP, and stopSequences.
	// maxTokens is always sent — Converse requires it, and the estimate's
	// output term is unbounded without it.
	InferenceConfig inferenceConfig `json:"inferenceConfig"`
}

// converseMessage is one entry in the message list.
type converseMessage struct {
	Role string `json:"role"`

	// Content is the block-array form. Converse rejects a plain string here,
	// which is the difference from the Messages API that costs the most when
	// overlooked: the wire shape is close enough that a mapping written for
	// one API ships for the other and 400s every Case.
	Content []textBlock `json:"content"`
}

// textBlock is a Converse text content block.
type textBlock struct {
	Text string `json:"text"`
}

// inferenceConfig is the settings object Converse nests.
//
// Temperature is a pointer because 0 is a meaningful value and the field must
// be ABSENT on models that reject sampling parameters with a 400 — the same
// contract as the Messages API, and the same reason.
type inferenceConfig struct {
	MaxTokens     int64     `json:"maxTokens"`
	Temperature   *float64  `json:"temperature,omitempty"`
	TopP          *float64  `json:"topP,omitempty"`
	StopSequences *[]string `json:"stopSequences,omitempty"`
}

// converseResponse is a 200 from Converse.
type converseResponse struct {
	Output *converseOutput `json:"output"`

	// StopReason also appears INSIDE output.message on Converse; the top-level
	// one is the record this adapter reads, and the nested one is ignored.
	StopReason string `json:"stopReason"`

	// Usage is a pointer so an absent block is distinguishable from a block of
	// zeros.
	Usage *usage `json:"usage"`
}

// converseOutput wraps the response message.
type converseOutput struct {
	Message *converseResponseMessage `json:"message"`
}

// converseResponseMessage is a response message, whose content blocks carry a
// type tag so tool_use blocks are distinguishable from text.
type converseResponseMessage struct {
	Role    string                 `json:"role"`
	Content []converseContentBlock `json:"content"`
}

// converseContentBlock is one block of a response message's content array.
//
// Converse nests toolUse's fields under a "toolUse" object rather than beside
// the type tag the way the Messages API does. M2 sends no tools, so the block
// is recorded rather than used — but recorded correctly, because a silently
// discarded tool call reads downstream as an empty answer.
type converseContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`

	ToolUse *struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"toolUse"`
}

// escapeModelID percent-encodes a model id for the URL path.
//
// Colons become %3A — an ARN model id carries several, and AWS's request
// routing does not accept them literally. Slashes stay literal: the ARN's
// "foundation-model/" is path structure, not a character of the id. This is
// the exact string the signer's canonical URI uses, which is what makes the
// signature valid: the wire URL and the canonical URI must agree byte for
// byte, and the pinned ARN golden test holds them to it.
func escapeModelID(id string) string {
	var b strings.Builder
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c == ':':
			b.WriteString("%3A")
		case c == '/':
			b.WriteByte('/')
		case isUnreserved(c):
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// isUnreserved is RFC 3986's unreserved set, which needs no encoding.
func isUnreserved(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '_', c == '.', c == '~':
		return true
	}
	return false
}

// errorEnvelope is the decoded body of a non-2xx response.
//
// Bedrock's errors are a flat object: `{"message": "...", "type": "..."}` —
// occasionally with a `code`. Unlike the Anthropic adapter there is no nested
// error object to stage, so the decode is one pass.
type errorEnvelope struct {
	// Type is the exception class — "ValidationException",
	// "AccessDeniedException", "ModelNotAccessibleException".
	Type string

	// Message is the provider's own words, unsanitized. Every reader
	// sanitizes.
	Message string
}

// encodeRequest marshals a request body.
func encodeRequest(r *converseRequest) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("bedrock: encoding the request: %w", err)
	}
	return b, nil
}

// decodeResponse parses a 200 body.
//
// Unknown fields are IGNORED, for the same reason as the Messages API: a
// provider adding a field is routine and monthly, and rejecting one would turn
// every Bedrock release into a run that errors every Case.
func decodeResponse(body []byte) (*converseResponse, error) {
	var m converseResponse
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("bedrock: decoding the response body: %w", err)
	}
	return &m, nil
}

// decodeError parses an error body, best effort.
//
// Best effort because an error body is exactly where a gateway substitutes an
// HTML page for the provider's JSON. A nil return means "no structured
// error", which the caller reports as the status alone rather than failing
// twice.
func decodeError(body []byte) *errorEnvelope {
	var e errorEnvelope
	if err := json.Unmarshal(body, &e); err != nil {
		return nil
	}
	if e.Type == "" && e.Message == "" {
		return nil
	}
	return &e
}

// text concatenates the response message's text blocks.
func (m *converseResponse) text() string {
	if m.Output == nil || m.Output.Message == nil {
		return ""
	}
	var b strings.Builder
	for i := range m.Output.Message.Content {
		if m.Output.Message.Content[i].Type == "text" {
			b.WriteString(m.Output.Message.Content[i].Text)
		}
	}
	return b.String()
}

// toolCalls reports any toolUse blocks the provider returned.
//
// M2 sends no tools, so these are only populated by a provider doing something
// we did not ask for — recorded rather than dropped, because a silently
// discarded tool call reads downstream as an empty answer.
func (m *converseResponse) toolCalls() []*knov1.ToolCall {
	if m.Output == nil || m.Output.Message == nil {
		return nil
	}
	var out []*knov1.ToolCall
	for i := range m.Output.Message.Content {
		blk := m.Output.Message.Content[i]
		if blk.Type != "toolUse" || blk.ToolUse == nil {
			continue
		}
		out = append(out, &knov1.ToolCall{
			Name:      blk.ToolUse.Name,
			Arguments: string(blk.ToolUse.Input),
		})
	}
	return out
}

// stopReasonOf maps Converse's stop reasons onto the schema's enum.
//
// The two refusal values — content_filtered and guardrail_intervened — map to
// CONTENT_FILTER and are NEVER an error: an account whose safety settings
// decline every Case would otherwise produce 100% scored Cases, an aggregate
// of 0.000, and a clean error rate — a confident reference number for a run
// in which the agent was never measured.
func stopReasonOf(s string) knov1.StopReason {
	switch s {
	case stopEndTurn, stopStopSequence:
		return knov1.StopReason_STOP_REASON_STOP
	case stopMaxTokens, stopModelContextLimit:
		return knov1.StopReason_STOP_REASON_LENGTH
	case stopToolUse:
		return knov1.StopReason_STOP_REASON_TOOL_CALL
	case stopContentFiltered, stopGuardrail:
		return knov1.StopReason_STOP_REASON_CONTENT_FILTER
	default:
		return knov1.StopReason_STOP_REASON_UNSPECIFIED
	}
}

// refused reports whether a stop reason means the model's answer was
// suppressed rather than produced.
func refused(stopReason string) bool {
	return stopReason == stopContentFiltered || stopReason == stopGuardrail
}

// maxProviderMessage bounds how much of a provider's error text is quoted.
const maxProviderMessage = 300

// sanitize makes a provider-supplied string safe to put in an error, a log
// line, and a persisted Outcome.
//
// Identical contract to the anthropic adapter's: a provider's error message
// can quote parts of the request back (a Case is customer data), and any
// string rendered with %s into a log line is a log-injection primitive the
// moment it carries a newline. Control characters collapse to spaces and the
// whole thing is truncated.
func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)

	if len(s) > maxProviderMessage {
		s = s[:maxProviderMessage] + "..."
	}
	return strings.ToValidUTF8(s, "")
}

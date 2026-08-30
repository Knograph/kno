// This file holds the Vertex :rawPredict wire format, and it is the only file
// in this package that touches encoding/json.
//
// Same exemption as the sibling adapters' format.go: these are a PROVIDER's
// wire shapes decoded into plain Go structs, no kno.v1 type is involved.
//
// The wire is the Anthropic Messages format with two named divergences
// (docs/plans/2026-08-29-bedrock-vertex-agents.md P0-1), spelled here and
// nowhere else:
//
//   - `anthropic_version` travels in the BODY, not the `anthropic-version`
//     header. Sending the header instead is the kind of near-miss that 400s
//     every Case with an error message that does not name the cause.
//   - the model id is in the URL path, percent-encoded (the `@` pin of a
//     dated id must reach the router as %40).

package vertex

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// anthropicVersion is the protocol version the :rawPredict surface expects.
const anthropicVersion = "vertex-2023-10-16"

// roles the wire accepts.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// stop reasons Vertex returns — the Messages API's own vocabulary.
const (
	stopEndTurn      = "end_turn"
	stopMaxTokens    = "max_tokens"
	stopStopSequence = "stop_sequence"
	stopToolUse      = "tool_use"
	stopRefusal      = "refusal"
)

// rawPredictRequest is the POST :rawPredict body.
//
// The model id is NOT here — it lives in the URL path, percent-encoded.
// anthropic_version is here, in the body, per P0-1.
//
// Deliberately absent: `seed`. The Messages API has no seed parameter, and
// inventing one is how a run 400s every Case
// (docs/plans/2026-08-29-bedrock-vertex-agents.md P0-3).
type rawPredictRequest struct {
	AnthropicVersion string `json:"anthropic_version"`
	MaxTokens        int64  `json:"max_tokens"`

	// System is the top-level system prompt. Omitted when empty: `"system": ""`
	// is not the same as sending no system prompt, and it perturbs the cache
	// prefix for nothing.
	System string `json:"system,omitempty"`

	Messages []rawMessage `json:"messages"`

	// Temperature is a pointer because 0 is a meaningful value and the field
	// must be ABSENT — not zero — on models that reject sampling parameters
	// with a 400. See samplingRemoved.
	Temperature *float64 `json:"temperature,omitempty"`
}

// rawMessage is one entry in the message list.
type rawMessage struct {
	Role string `json:"role"`

	// Content is the plain-string form, which the API accepts for text-only
	// turns. M2 sends no images, documents, or tool blocks, so the block-array
	// form would be ceremony around a string.
	Content string `json:"content"`
}

// rawResponse is a 200 from :rawPredict.
type rawResponse struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Role  string `json:"role"`
	Model string `json:"model"`

	Content    []rawContentBlock `json:"content"`
	StopReason string            `json:"stop_reason"`

	// Usage is a pointer so an absent block is distinguishable from a block of
	// zeros. They settle differently: absent means "estimate and mark it", and
	// all-zero on a refusal means the provider genuinely billed nothing.
	Usage *usage `json:"usage"`
}

// rawContentBlock is one block of a response's content array.
type rawContentBlock struct {
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
// Vertex AI's errors are nested: `{"error": {"code": 404, "message": "...",
// "status": "NOT_FOUND"}}`. The code and status are the provider's own words
// — every reader sanitizes.
type errorEnvelope struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// escapeModelID percent-encodes a model id for the URL path.
//
// The `@` pin of a dated id ("claude-3-5-sonnet@20240620") becomes %40 — the
// AI Platform router does not accept it literally. Unreserved characters
// stay, everything else is percent-encoded byte-wise. Slashes stay literal:
// a model id is one path segment, never a path.
func escapeModelID(id string) string {
	var b strings.Builder
	for i := 0; i < len(id); i++ {
		c := id[i]
		if isUnreserved(c) {
			b.WriteByte(c)
		} else {
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

// encodeRequest marshals a request body.
func encodeRequest(r *rawPredictRequest) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("vertex: encoding the request: %w", err)
	}
	return b, nil
}

// decodeResponse parses a 200 body.
//
// Unknown fields are IGNORED, for the same reason as the Messages API: a
// provider adding a field is routine and monthly, and rejecting one would
// turn every Vertex release into a run that errors every Case.
func decodeResponse(body []byte) (*rawResponse, error) {
	var m rawResponse
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("vertex: decoding the response body: %w", err)
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
	if e.Error == nil || (e.Error.Message == "" && e.Error.Status == "") {
		return nil
	}
	return &e
}

// text concatenates the response's text blocks.
func (m *rawResponse) text() string {
	if m.Content == nil {
		return ""
	}
	var b strings.Builder
	for i := range m.Content {
		if m.Content[i].Type == "text" {
			b.WriteString(m.Content[i].Text)
		}
	}
	return b.String()
}

// toolCalls reports any tool_use blocks the provider returned.
//
// M2 sends no tools, so these are only populated by a provider doing
// something we did not ask for — recorded rather than dropped, because a
// silently discarded tool call reads downstream as an empty answer.
func (m *rawResponse) toolCalls() []*knov1.ToolCall {
	var out []*knov1.ToolCall
	for i := range m.Content {
		blk := m.Content[i]
		if blk.Type != "tool_use" || blk.Input == nil {
			continue
		}
		out = append(out, &knov1.ToolCall{
			Name:      blk.Name,
			Arguments: string(blk.Input),
		})
	}
	return out
}

// stopReasonOf maps the Messages API's stop reasons onto the schema's enum.
//
// `refusal` is scored as CONTENT_FILTER and is NEVER an error: an account
// whose safety settings decline every Case would otherwise produce 100%
// scored Cases, an aggregate of 0.000, and a clean error rate — a confident
// reference number for a run in which the agent was never measured.
func stopReasonOf(s string) knov1.StopReason {
	switch s {
	case stopEndTurn, stopStopSequence:
		return knov1.StopReason_STOP_REASON_STOP
	case stopMaxTokens:
		return knov1.StopReason_STOP_REASON_LENGTH
	case stopToolUse:
		return knov1.StopReason_STOP_REASON_TOOL_CALL
	case stopRefusal:
		return knov1.StopReason_STOP_REASON_CONTENT_FILTER
	default:
		return knov1.StopReason_STOP_REASON_UNSPECIFIED
	}
}

// refused reports whether a stop reason means the model's answer was
// suppressed rather than produced.
func refused(stopReason string) bool {
	return stopReason == stopRefusal
}

// maxProviderMessage bounds how much of a provider's error text is quoted.
const maxProviderMessage = 300

// sanitize makes a provider-supplied string safe to put in an error, a log
// line, and a persisted Outcome.
//
// Identical contract to the sibling adapters': a provider's error message
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

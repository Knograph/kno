package vertex

import (
	"strings"
	"testing"
	"unicode/utf8"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestEncodeRequest asserts the exact request body: anthropic_version IN the
// body, model NOT in it (the model lives in the URL path), snake_case field
// names, system omitted when empty, temperature omitted when nil.
func TestEncodeRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  rawPredictRequest
		want string // exact compact body
	}{
		{
			name: "minimal",
			req: rawPredictRequest{
				AnthropicVersion: anthropicVersion,
				MaxTokens:        1024,
				Messages:         []rawMessage{{Role: roleUser, Content: "hello"}},
			},
			want: `{"anthropic_version":"vertex-2023-10-16","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`,
		},
		{
			name: "system and temperature",
			req: rawPredictRequest{
				AnthropicVersion: anthropicVersion,
				MaxTokens:        2048,
				System:           "you are a grader",
				Messages:         []rawMessage{{Role: roleUser, Content: "hi"}},
				Temperature:      p64(0.2),
			},
			want: `{"anthropic_version":"vertex-2023-10-16","max_tokens":2048,"system":"you are a grader","messages":[{"role":"user","content":"hi"}],"temperature":0.2}`,
		},
		{
			name: "temperature zero is present, not omitted",
			req: rawPredictRequest{
				AnthropicVersion: anthropicVersion,
				MaxTokens:        100,
				Messages:         []rawMessage{{Role: roleUser, Content: "x"}},
				Temperature:      p64(0),
			},
			want: `{"anthropic_version":"vertex-2023-10-16","max_tokens":100,"messages":[{"role":"user","content":"x"}],"temperature":0}`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := encodeRequest(&tt.req)
			if err != nil {
				t.Fatalf("encodeRequest: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("body:\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

// TestDecodeResponse asserts the Messages response shape decodes, including a
// tool_use block and an absent usage block.
func TestDecodeResponse(t *testing.T) {
	t.Parallel()

	m, err := decodeResponse([]byte(rawPredictOK))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if m.StopReason != stopEndTurn {
		t.Errorf("stop_reason = %q", m.StopReason)
	}
	if m.text() != "hello from vertex" {
		t.Errorf("text = %q", m.text())
	}
	if m.Usage == nil {
		t.Fatal("usage is nil")
	}

	withTool := `{"content":[{"type":"tool_use","name":"lookup","input":{"q":"x"}},{"type":"text","text":"done"}],"stop_reason":"tool_use"}`
	m, err = decodeResponse([]byte(withTool))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	calls := m.toolCalls()
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	if calls[0].Name != "lookup" || calls[0].Arguments != `{"q":"x"}` {
		t.Errorf("call = %+v", calls[0])
	}
	if m.text() != "done" {
		t.Errorf("text = %q", m.text())
	}

	noUsage := `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`
	m, err = decodeResponse([]byte(noUsage))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if m.Usage != nil {
		t.Error("absent usage decoded as present")
	}
}

// TestDecodeError asserts the nested Google error envelope and the
// best-effort behavior on non-JSON bodies.
func TestDecodeError(t *testing.T) {
	t.Parallel()

	env := decodeError([]byte(`{"error":{"code":404,"message":"models/foo not found","status":"NOT_FOUND"}}`))
	if env == nil || env.Error == nil {
		t.Fatal("env is nil")
	}
	if env.Error.Status != "NOT_FOUND" {
		t.Errorf("status = %q", env.Error.Status)
	}
	if env.Error.Code != 404 {
		t.Errorf("code = %d", env.Error.Code)
	}

	for _, bad := range []string{"<html>gateway</html>", "", `{"error":{}}`} {
		if env := decodeError([]byte(bad)); env != nil {
			t.Errorf("decodeError(%q) = %+v, want nil", bad, env)
		}
	}
}

// TestEscapeModelID asserts the @ pin becomes %40 and slashes stay literal.
func TestEscapeModelID(t *testing.T) {
	t.Parallel()

	if got := escapeModelID("claude-3-5-sonnet@20240620"); got != "claude-3-5-sonnet%4020240620" {
		t.Errorf("escape = %q", got)
	}
	if got := escapeModelID("claude-sonnet-4-5"); got != "claude-sonnet-4-5" {
		t.Errorf("escape = %q", got)
	}
}

// TestRawPredictPath asserts the URL shape the router must receive.
func TestRawPredictPath(t *testing.T) {
	t.Parallel()

	got := rawPredictPath("proj-1", "us-central1", "claude-3-5-sonnet@20240620")
	want := "/v1/projects/proj-1/locations/us-central1/publishers/anthropic/models/claude-3-5-sonnet%4020240620:rawPredict"
	if got != want {
		t.Errorf("path:\n got %s\nwant %s", got, want)
	}
}

// TestStopReasonOf maps the Messages vocabulary onto the schema enum.
func TestStopReasonOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want knov1.StopReason
	}{
		{stopEndTurn, knov1.StopReason_STOP_REASON_STOP},
		{stopStopSequence, knov1.StopReason_STOP_REASON_STOP},
		{stopMaxTokens, knov1.StopReason_STOP_REASON_LENGTH},
		{stopToolUse, knov1.StopReason_STOP_REASON_TOOL_CALL},
		{stopRefusal, knov1.StopReason_STOP_REASON_CONTENT_FILTER},
		{"something-new", knov1.StopReason_STOP_REASON_UNSPECIFIED},
	}
	for _, tt := range tests {
		if got := stopReasonOf(tt.in); got != tt.want {
			t.Errorf("stopReasonOf(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// TestRefused asserts only the refusal stop reason flags a refused Case.
func TestRefused(t *testing.T) {
	t.Parallel()

	if !refused(stopRefusal) {
		t.Error("refusal stop reason is not refused")
	}
	for _, s := range []string{stopEndTurn, stopMaxTokens, stopToolUse, ""} {
		if refused(s) {
			t.Errorf("%q is refused", s)
		}
	}
}

// TestSanitize asserts provider text is bounded, control characters collapse,
// and invalid UTF-8 cannot ride along.
func TestSanitize(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 400)
	if got := sanitize(long); len(got) != maxProviderMessage+3 {
		t.Errorf("sanitize length = %d, want %d", len(got), maxProviderMessage+3)
	}
	if got := sanitize("a\nb\tc\x01"); got != "a b c" {
		t.Errorf("sanitize = %q", got)
	}
	if got := sanitize(string([]byte{0xff, 'a'})); !utf8.ValidString(got) {
		t.Errorf("sanitize is not valid UTF-8: %q", got)
	}
	if got := sanitize("  "); got != "" {
		t.Errorf("sanitize = %q", got)
	}
}

// TestErrorTextOf asserts the provider's words, sanitized, with the status as
// the fallback when there is no body.
func TestErrorTextOf(t *testing.T) {
	t.Parallel()

	if got := errorTextOf(500, nil); got != "HTTP 500" {
		t.Errorf("errorTextOf = %q", got)
	}
	env := decodeError([]byte(`{"error":{"message":"bad\nrequest","status":"INVALID_ARGUMENT"}}`))
	if got := errorTextOf(400, env); got != "INVALID_ARGUMENT: bad request" {
		t.Errorf("errorTextOf = %q", got)
	}
}

func p64(v float64) *float64 { return &v }

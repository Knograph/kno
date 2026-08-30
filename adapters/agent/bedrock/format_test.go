package bedrock

import (
	"strings"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestEncodeRequestPinsTheWireShape pins the request body Converse receives:
// camelCase, content-block arrays, system as an array, and NO seed.
func TestEncodeRequestPinsTheWireShape(t *testing.T) {
	t.Parallel()

	temp := 0.7
	body, err := encodeRequest(&converseRequest{
		Messages: []converseMessage{
			{Role: roleUser, Content: []textBlock{{Text: "hi"}}},
		},
		System: []textBlock{{Text: "S"}},
		InferenceConfig: inferenceConfig{
			MaxTokens:   1024,
			Temperature: &temp,
		},
	})
	if err != nil {
		t.Fatalf("encodeRequest: %v", err)
	}
	s := string(body)

	for _, want := range []string{
		`"messages":[{"role":"user","content":[{"text":"hi"}]}]`,
		`"system":[{"text":"S"}]`,
		`"inferenceConfig":{"maxTokens":1024,"temperature":0.7}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("body does not contain %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "seed") {
		t.Error("the request carries a seed parameter Converse rejects (plan P0-3)")
	}
}

// TestEncodeRequestOmissions pins the deliberate absences: no system key at
// all when there is no system prompt, no temperature when unset.
func TestEncodeRequestOmissions(t *testing.T) {
	t.Parallel()

	body, err := encodeRequest(&converseRequest{
		Messages: []converseMessage{{Role: roleUser, Content: []textBlock{{Text: "hi"}}}},
		InferenceConfig: inferenceConfig{
			MaxTokens: 1024,
		},
	})
	if err != nil {
		t.Fatalf("encodeRequest: %v", err)
	}
	s := string(body)
	for _, absent := range []string{`"system"`, `"temperature"`} {
		if strings.Contains(s, absent) {
			t.Errorf("body contains %s, want it omitted:\n%s", absent, s)
		}
	}
}

// TestDecodeResponseIgnoresUnknownFields pins the tolerance policy: a provider
// adding a field is routine, and rejecting it breaks every Case.
func TestDecodeResponseIgnoresUnknownFields(t *testing.T) {
	t.Parallel()

	m, err := decodeResponse([]byte(`{
		"someFutureField": {"nested": [1, 2]},
		"output": {"message": {"role": "assistant", "content": [
			{"type": "text", "text": "a"},
			{"type": "anotherFutureBlock", "stuff": true},
			{"type": "text", "text": "b"}
		]}},
		"stopReason": "end_turn",
		"usage": {"inputTokens": 1, "outputTokens": 1, "totalTokens": 2}
	}`))
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if got := m.text(); got != "ab" {
		t.Errorf("text = %q, want \"ab\"", got)
	}
	if m.StopReason != stopEndTurn {
		t.Errorf("StopReason = %q", m.StopReason)
	}
	if m.Usage == nil || tokens(m.Usage.InputTokens) != 1 {
		t.Errorf("usage not decoded: %+v", m.Usage)
	}
}

// TestDecodeResponseMalformed pins the error path: garbage is an error, and
// truncated JSON is an error, not a silent partial read.
func TestDecodeResponseMalformed(t *testing.T) {
	t.Parallel()

	for _, body := range []string{`{`, `not json`, `{"output":`} {
		if _, err := decodeResponse([]byte(body)); err == nil {
			t.Errorf("decodeResponse(%q) succeeded, want an error", body)
		}
	}
}

// TestStopReasonOf pins the stop-reason mapping, including the refusal pair
// that MUST score as CONTENT_FILTER rather than error.
func TestStopReasonOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		reason string
		want   knov1.StopReason
	}{
		{"end_turn", knov1.StopReason_STOP_REASON_STOP},
		{"stop_sequence", knov1.StopReason_STOP_REASON_STOP},
		{"max_tokens", knov1.StopReason_STOP_REASON_LENGTH},
		{"model_context_window_exceeded", knov1.StopReason_STOP_REASON_LENGTH},
		{"tool_use", knov1.StopReason_STOP_REASON_TOOL_CALL},
		{"content_filtered", knov1.StopReason_STOP_REASON_CONTENT_FILTER},
		{"guardrail_intervened", knov1.StopReason_STOP_REASON_CONTENT_FILTER},
		{"end_turn_extra", knov1.StopReason_STOP_REASON_UNSPECIFIED},
		{"", knov1.StopReason_STOP_REASON_UNSPECIFIED},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.reason, func(t *testing.T) {
			t.Parallel()
			if got := stopReasonOf(tc.reason); got != tc.want {
				t.Errorf("stopReasonOf(%q) = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

// TestSanitize pins the provider-message scrub: control characters collapse,
// the string is trimmed and truncated, and invalid UTF-8 does not survive.
func TestSanitize(t *testing.T) {
	t.Parallel()

	if got := Sanitize("  hello\n\nworld  "); got != "hello  world" {
		t.Errorf("Sanitize = %q", got)
	}

	long := strings.Repeat("x", 400)
	if got := Sanitize(long); len(got) != maxProviderMessage+3 {
		t.Errorf("Sanitize(long) length = %d, want %d", len(got), maxProviderMessage+3)
	}

	// A multi-byte rune split by the byte truncation must not leave invalid
	// UTF-8 behind.
	wide := strings.Repeat("é", maxProviderMessage)
	if got := Sanitize(wide); strings.ToValidUTF8(got, "\ufffd") != got {
		t.Error("Sanitize left invalid UTF-8 behind")
	}

	if got := Sanitize("\x01\x02\x03"); got != "" {
		t.Errorf("Sanitize(controls) = %q, want empty", got)
	}
}

// TestDecodeErrorPinsTheEnvelope pins the best-effort error decode: valid
// JSON that is not an error shape yields nil, as does garbage.
func TestDecodeErrorPinsTheEnvelope(t *testing.T) {
	t.Parallel()

	env := decodeError([]byte(`{"message":"the model is not accessible","type":"ModelNotAccessibleException"}`))
	if env == nil {
		t.Fatal("decodeError = nil for a valid envelope")
	}
	if env.Message != "the model is not accessible" || env.Type != "ModelNotAccessibleException" {
		t.Errorf("envelope = %+v", env)
	}

	for _, body := range []string{`{}`, `{"message":""}`, `not json`, `{"message":123}`} {
		if got := decodeError([]byte(body)); got != nil {
			t.Errorf("decodeError(%q) = %+v, want nil", body, got)
		}
	}
}

// TestRefusedPinsTheRefusalPair pins the two values that score as refused.
func TestRefusedPinsTheRefusalPair(t *testing.T) {
	t.Parallel()

	if !refused(stopContentFiltered) || !refused(stopGuardrail) {
		t.Error("refused misses a refusal stop reason")
	}
	for _, s := range []string{stopEndTurn, stopMaxTokens, stopToolUse, stopModelContextLimit, "anything"} {
		if refused(s) {
			t.Errorf("refused(%q) = true", s)
		}
	}
}

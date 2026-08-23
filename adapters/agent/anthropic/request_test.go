package anthropic_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/anthropic"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// okBody is the shortest well-formed Messages API answer.
const okBody = `{
  "id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6-20260101",
  "content":[{"type":"text","text":"4"}],
  "stop_reason":"end_turn",
  "usage":{"input_tokens":10,"output_tokens":2}
}`

// TestSystemPromptTravelsAsATopLevelFieldNotAMessage.
//
// This is the difference between the Messages API and an OpenAI-compatible one
// that produces a run of failures rather than a single readable error: the API
// rejects role "system" inside `messages` with a 400, so an adapter that sends
// it there fails EVERY Case and reports "too many cases errored for this to be
// a usable baseline", naming nothing about the cause.
func TestSystemPromptTravelsAsATopLevelFieldNotAMessage(t *testing.T) {
	t.Parallel()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv, func(o *anthropic.Options) { o.System = "you are terse" })

	if _, err := a.Invoke(t.Context(), aCase("2+2?")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	body := rec.body(t, 0)
	if !strings.Contains(body, `"system":"you are terse"`) {
		t.Errorf("the system prompt is not a top-level field; body was %s", body)
	}
	if strings.Contains(body, `"role":"system"`) {
		t.Errorf("the system prompt was sent as a message, which the Messages API "+
			"rejects with a 400 on every Case; body was %s", body)
	}
}

// TestMaxTokensIsAlwaysSent: the Messages API REQUIRES max_tokens. An adapter
// that treats it as optional 400s on every Case.
func TestMaxTokensIsAlwaysSent(t *testing.T) {
	t.Parallel()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv, func(o *anthropic.Options) { o.MaxOutputTokens = 777 })

	if _, err := a.Invoke(t.Context(), aCase("hi")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(rec.body(t, 0), `"max_tokens":777`) {
		t.Errorf("max_tokens is missing or wrong; body was %s", rec.body(t, 0))
	}
}

// TestNoCacheControlIsEverSent.
//
// docs/debt.md#41 is inert only while this holds: Price carries ONE cache-write
// rate and Anthropic publishes two, so a cache write would settle a 1-hour
// write at the 5-minute rate — an UNDER-charge, which prime directive 4 calls a
// P0. The entry assumes this; the assumption is asserted rather than trusted.
func TestNoCacheControlIsEverSent(t *testing.T) {
	t.Parallel()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv, func(o *anthropic.Options) { o.System = "long standing instructions" })

	c := &knov1.Case{Id: "c", Input: "q", History: []*knov1.Turn{
		{Role: knov1.Role_ROLE_USER, Content: "earlier"},
		{Role: knov1.Role_ROLE_ASSISTANT, Content: "answer"},
	}}
	if _, err := a.Invoke(t.Context(), c); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if strings.Contains(rec.body(t, 0), "cache_control") {
		t.Errorf("a cache_control breakpoint was sent, which makes docs/debt.md#41 "+
			"an under-charge rather than inert; body was %s", rec.body(t, 0))
	}
}

// TestHistoryBecomesAlternatingMessagesWithTheInputLast.
func TestHistoryBecomesAlternatingMessagesWithTheInputLast(t *testing.T) {
	t.Parallel()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv)

	c := &knov1.Case{Id: "c", Input: "and then?", History: []*knov1.Turn{
		{Role: knov1.Role_ROLE_SYSTEM, Content: "be brief"},
		{Role: knov1.Role_ROLE_USER, Content: "first"},
		{Role: knov1.Role_ROLE_ASSISTANT, Content: "second"},
	}}
	if _, err := a.Invoke(t.Context(), c); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	body := rec.body(t, 0)
	want := `"messages":[{"role":"user","content":"first"},` +
		`{"role":"assistant","content":"second"},` +
		`{"role":"user","content":"and then?"}]`
	if !strings.Contains(body, want) {
		t.Errorf("messages are not the alternating list the API requires;\n got %s\nwant %s", body, want)
	}
	if !strings.Contains(body, `"system":"be brief"`) {
		t.Errorf("a ROLE_SYSTEM history turn did not reach the top-level system field; body was %s", body)
	}
}

// TestConsecutiveSameRoleTurnsAreJoinedRatherThanRefused.
//
// The API rejects two user messages in a row with a 400. A multi-turn Case
// carrying them is ordinary, and refusing it would drop a Case from the
// denominator behind every later delta.
func TestConsecutiveSameRoleTurnsAreJoinedRatherThanRefused(t *testing.T) {
	t.Parallel()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv)

	c := &knov1.Case{Id: "c", Input: "third", History: []*knov1.Turn{
		{Role: knov1.Role_ROLE_USER, Content: "first"},
		{Role: knov1.Role_ROLE_USER, Content: "second"},
	}}
	if _, err := a.Invoke(t.Context(), c); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	body := rec.body(t, 0)
	if strings.Count(body, `"role":"user"`) != 1 {
		t.Errorf("consecutive user turns were sent as separate messages, which the "+
			"API rejects with a 400; body was %s", body)
	}
	for _, frag := range []string{"first", "second", "third"} {
		if !strings.Contains(body, frag) {
			t.Errorf("joining the turns dropped %q; body was %s", frag, body)
		}
	}
}

// TestAssistantFirstHistoryIsRefusedBeforeAnythingIsSpent.
//
// The API requires the first message to be a user turn. Refused locally, and
// the assertion that matters is the CALL COUNT: letting the provider refuse it
// pays for a 400 per attempt, per retry, for every Case with the same shape.
func TestAssistantFirstHistoryIsRefusedBeforeAnythingIsSpent(t *testing.T) {
	t.Parallel()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv)

	c := &knov1.Case{Id: "c", Input: "next", History: []*knov1.Turn{
		{Role: knov1.Role_ROLE_ASSISTANT, Content: "I went first"},
	}}
	_, err := a.Invoke(t.Context(), c)
	if err == nil {
		t.Fatal("an assistant-first history was accepted")
	}
	if rec.calls() != 0 {
		t.Errorf("the server saw %d requests; a Case Kno will not send must cost nothing", rec.calls())
	}
}

// TestACaseWithNoInputIsRefusedBeforeAnythingIsSpent.
func TestACaseWithNoInputIsRefusedBeforeAnythingIsSpent(t *testing.T) {
	t.Parallel()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv)

	if _, err := a.Invoke(t.Context(), aCase("")); err == nil {
		t.Fatal("a Case with no input was accepted")
	}
	if rec.calls() != 0 {
		t.Errorf("the server saw %d requests for a Case with no input", rec.calls())
	}
}

// TestAToolTurnReachesTheModelAsAUserTurn: the Messages API carries a tool
// result inside a user turn. Dropping it would change the prompt the Case
// describes.
func TestAToolTurnReachesTheModelAsAUserTurn(t *testing.T) {
	t.Parallel()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv)

	c := &knov1.Case{Id: "c", Input: "so?", History: []*knov1.Turn{
		{Role: knov1.Role_ROLE_USER, Content: "look it up"},
		{Role: knov1.Role_ROLE_ASSISTANT, Content: "calling search"},
		{Role: knov1.Role_ROLE_TOOL, Content: "search said 42"},
	}}
	if _, err := a.Invoke(t.Context(), c); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(rec.body(t, 0), "search said 42") {
		t.Errorf("the tool turn was dropped; body was %s", rec.body(t, 0))
	}
}

// TestAnthropicVersionHeaderIsPinned.
//
// The version header is how Anthropic keeps a response shape stable. A run
// whose parsing changes underneath it is a run whose numbers changed for no
// recorded reason.
func TestAnthropicVersionHeaderIsPinned(t *testing.T) {
	t.Parallel()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv)

	if _, err := a.Invoke(t.Context(), aCase("hi")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := rec.header(t, 0).Get("Anthropic-Version"); got != anthropic.APIVersion {
		t.Errorf("anthropic-version = %q, want %q", got, anthropic.APIVersion)
	}
	if got := rec.header(t, 0).Get("Anthropic-Beta"); got != "" {
		t.Errorf("anthropic-beta = %q; this adapter opts into no betas, and the "+
			"header is one of the ones fixtures must never carry", got)
	}
}

// TestTemperatureIsOmittedWhenUnsetAndSentWhenSet.
func TestTemperatureIsOmittedWhenUnsetAndSentWhenSet(t *testing.T) {
	t.Parallel()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })

	plain := newAgent(t, srv)
	if _, err := plain.Invoke(t.Context(), aCase("hi")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if strings.Contains(rec.body(t, 0), "temperature") {
		t.Errorf("temperature was sent with no temperature configured; body was %s", rec.body(t, 0))
	}

	zero := 0.0
	set := newAgent(t, srv, func(o *anthropic.Options) { o.Temperature = &zero })
	if _, err := set.Invoke(t.Context(), aCase("hi")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// Zero is the value a measurement run wants, and it is exactly the value a
	// non-pointer field would have made indistinguishable from "unset".
	if !strings.Contains(rec.body(t, 1), `"temperature":0`) {
		t.Errorf("temperature 0 was not sent; body was %s", rec.body(t, 1))
	}
}

// TestExactlyOneRequestPerInvoke, counted AT THE SERVER.
//
// core settles every Response as exactly one call and takes one reservation per
// attempt. An adapter retrying underneath would make N provider calls inside
// one reservation, turning --max-calls 1000 into up to 3000 real calls.
func TestExactlyOneRequestPerInvoke(t *testing.T) {
	t.Parallel()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		// Close the connection so the next call reuses nothing, which is the
		// state where net/http would replay a request on its own.
		w.Header().Set("Connection", "close")
		answer(w, okBody)
	})
	a := newAgent(t, srv)

	const calls = 4
	for range calls {
		if _, err := a.Invoke(t.Context(), aCase("hi")); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
	}
	if rec.calls() != calls {
		t.Errorf("the server saw %d requests for %d Invokes", rec.calls(), calls)
	}
	if got := a.RoundTrips(); got != calls {
		t.Errorf("RoundTrips = %d, want %d", got, calls)
	}
}

// TestNilCaseIsAnError, because a nil Case is programmer error reaching a
// network call.
func TestNilCaseIsAnError(t *testing.T) {
	t.Parallel()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) { answer(w, okBody) })
	a := newAgent(t, srv)

	var c *core.Case
	if _, err := a.Invoke(t.Context(), c); err == nil {
		t.Fatal("a nil Case was accepted")
	}
	if rec.calls() != 0 {
		t.Errorf("a nil Case reached the network")
	}
}

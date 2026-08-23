package anthropic

import (
	"context"
	"fmt"
	"math"
	"math/bits"
	"net/http"
	"time"

	"github.com/knograph/kno/adapters/agent/internal/transport"
	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Invoke runs one Case against the Messages API.
//
// Exactly one provider request per call, no retry. core owns retry, because
// each attempt takes its own budget reservation and settles its own call; an
// adapter retrying underneath would make N provider calls inside one
// reservation and settle them as one, turning --max-calls 1000 into up to 3000
// real calls.
func (a *Agent) Invoke(ctx context.Context, c *core.Case) (*core.Response, error) {
	if c == nil {
		return nil, fmt.Errorf("anthropic: nil case")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	body, err := a.encode(c)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	raw, err := a.client.Do(ctx, http.MethodPost, messagesPath, body)
	elapsed := time.Since(start)
	if err != nil {
		// No Response: nothing reached a handler, or nothing came back that
		// describes one. There is no provider-side fact to record.
		return nil, fromTransport(err)
	}

	// The limiter's wait is not the provider's latency. A run held back 60
	// seconds by a Retry-After would otherwise report a 60-second latency for a
	// call the provider answered in one, and every latency Goal downstream
	// would be measuring Kno's backoff.
	latency := elapsed - raw.WaitedFor
	if latency < 0 {
		latency = 0
	}

	if raw.StatusCode != http.StatusOK {
		return a.failure(c, raw, latency)
	}
	return a.success(c, raw, latency)
}

// encode turns a Case into a request body.
func (a *Agent) encode(c *core.Case) ([]byte, error) {
	system, msgs, err := a.compose(c)
	if err != nil {
		return nil, err
	}
	return encodeRequest(&messagesRequest{
		Model:       a.opts.Model,
		MaxTokens:   a.opts.MaxOutputTokens,
		System:      system,
		Messages:    msgs,
		Temperature: a.opts.Temperature,
	})
}

// compose splits a Case into the two halves the Messages API takes.
//
// The system prompt is a TOP-LEVEL field. A history turn with ROLE_SYSTEM joins
// it rather than becoming a message, because the API rejects role "system"
// inside `messages` with a 400 — which, applied to every Case, reports as "too
// many cases errored for this to be a usable baseline" and names nothing about
// the cause.
//
// The message list is normalized to what the API accepts: it must begin with a
// user turn and must not repeat a role, both of which are 400s. Consecutive
// same-role turns are joined rather than refused, because a multi-turn Case
// carrying two user messages in a row is ordinary and refusing it would drop a
// Case from the denominator. A history that BEGINS with an assistant turn is
// refused, because inventing a user turn to put in front of it would measure a
// prompt the Case does not describe.
func (a *Agent) compose(c *core.Case) (string, []message, error) {
	system := a.opts.System
	var msgs []message

	for _, t := range c.GetHistory() {
		content := t.GetContent()
		switch t.GetRole() {
		case knov1.Role_ROLE_SYSTEM:
			system = join(system, content)
		case knov1.Role_ROLE_USER:
			msgs = appendTurn(msgs, roleUser, content)
		case knov1.Role_ROLE_ASSISTANT:
			msgs = appendTurn(msgs, roleAssistant, content)
		case knov1.Role_ROLE_TOOL:
			// A tool result reaches the model inside a USER turn in this API.
			// M2 sends no tools, so this is only reached by an eval set that
			// recorded one; carrying the text as a user turn preserves what the
			// model saw better than dropping it.
			msgs = appendTurn(msgs, roleUser, content)
		default:
			return "", nil, malformedCase(c,
				fmt.Errorf("a history turn has no role, so its place in the conversation is undefined"))
		}
	}

	if c.GetInput() == "" {
		return "", nil, malformedCase(c,
			fmt.Errorf("the Case has no input, and the Messages API rejects an empty turn"))
	}
	msgs = appendTurn(msgs, roleUser, c.GetInput())

	if msgs[0].Role != roleUser {
		return "", nil, malformedCase(c,
			fmt.Errorf("the history begins with an assistant turn, and the Messages API requires the first message to be a user turn"))
	}
	return system, msgs, nil
}

// malformedCase is a Case this adapter refuses to send.
//
// Terminal and unretryable, and refused BEFORE the request goes out, so it
// costs nothing. Letting the provider refuse it instead would pay for a 400 per
// attempt, per retry, for every Case with the same shape.
func malformedCase(c *core.Case, cause error) error {
	return errs.ErrInvalidInput.
		WithFix("fix the Case in the evals file; Kno will not invent a turn to make it well formed").
		Wrap(fmt.Errorf("anthropic: case %s: %w", c.GetId(), cause))
}

// appendTurn adds content under role, joining it to the previous message when
// the role repeats.
//
// Empty content is dropped rather than sent: the API rejects an empty text
// block, and a blank history turn is not worth failing a Case over.
func appendTurn(msgs []message, role, content string) []message {
	if content == "" {
		return msgs
	}
	if n := len(msgs); n > 0 && msgs[n-1].Role == role {
		msgs[n-1].Content = join(msgs[n-1].Content, content)
		return msgs
	}
	return append(msgs, message{Role: role, Content: content})
}

// join concatenates two prompt fragments with a blank line, skipping empties.
func join(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n\n" + b
	}
}

// success builds the Response for a 200.
func (a *Agent) success(c *core.Case, raw *transport.Response, latency time.Duration) (*core.Response, error) {
	m, err := decodeResponse(raw.Body)
	if err != nil {
		// TERMINAL. The provider answered, so it billed; retrying pays twice for
		// one answer. See ErrMalformedResponse.
		return nil, ErrMalformedResponse.Wrap(err)
	}

	text := m.text()
	out := &core.Response{
		CaseId:        c.GetId(),
		Output:        text,
		ToolCalls:     m.toolCalls(),
		LatencyMs:     latency.Milliseconds(),
		ResolvedModel: m.Model,
		StopReason:    stopReasonOf(m.StopReason),

		// The authoritative refusal flag, set from stop_reason and from nothing
		// else. A refusal is a SCORED Case, never an error: scoring it without
		// recording it is how an account whose safety settings decline every
		// Case produces 100% scored Cases, an aggregate of 0.000, and a clean
		// error rate — a confident reference number for a run in which the agent
		// was never measured.
		Refused: m.StopReason == stopRefusal,

		// Anthropic reports no backend build identifier. Deliberately left
		// empty rather than filled with the message id: the Run records the SET
		// of build identifiers observed, and a per-request id would make that
		// set one entry per Case — a field that says nothing, at the size of the
		// run.
		ProviderBuildId: "",
	}

	a.settle(out, c, m, text)
	return out, nil
}

// settle fills in what the call cost.
//
// The ADAPTER stamps cost_usd_micros, including when it is inferred, because
// core derives spend from the Response alone and the store persists that same
// derivation. Charging the guard a number the Response does not carry would
// make Guard.Restore under-restore on resume by the full inferred cost of every
// Case — a resumed run spending its cap twice.
func (a *Agent) settle(out *core.Response, c *core.Case, m *messagesResponse, text string) {
	u := m.Usage

	// A refusal that fired before any output is documented as not billed at
	// all. Settling it at the pessimistic estimate would burn a run's whole
	// cost cap on an account that declines every Case, having spent nothing —
	// so this reports a MEASURED zero rather than an inferred number, and
	// usage_estimated stays false because that is what it means.
	if m.unbilledRefusal(text) {
		return
	}

	if u == nil || !u.usable(text) {
		a.settleFromEstimate(out, c)
		return
	}

	out.PromptTokens = u.billedInput()
	out.CompletionTokens = tokens(u.OutputTokens)
	out.CachedTokens = tokens(u.CacheReadInputTokens)

	p, ok := price(m.Model, a.opts.Model)
	if !ok {
		// The tokens are MEASURED; only the rate is missing. So this is not an
		// estimate and must not claim to be one: usage_estimated says
		// "cost_usd_micros is the engine's own prediction", and there is no
		// prediction here — case.proto forbids pairing that flag with a zero,
		// and pairing it with one anyway is a false claim in exactly the
		// direction the flag exists to prevent.
		//
		// Unreachable under a cost cap, and that is a guarantee rather than a
		// hope: Estimate reads the same table through the same fallback to the
		// requested model, so a model this cannot price is a model Estimate
		// could not price either, and core.estimate refuses such a Case before
		// Authorize whenever MaxCostUSDMicros is set. Without a cap the user
		// was told spend is unbounded and unpredicted, and Run's pricing-table
		// provenance is what explains the empty money column.
		return
	}
	out.CostUsdMicros = costOf(p, u)
}

// settleFromEstimate charges the reservation rather than nothing.
//
// Never zero. A zero settlement is what makes a dollar cap unenforceable, and
// it has already caused one real overshoot. Under a pessimistic estimate this
// over-charges the budget, which is the recoverable direction: the guard
// refuses early rather than late.
func (a *Agent) settleFromEstimate(out *core.Response, c *core.Case) {
	out.UsageEstimated = true
	est, err := a.estimate(c)
	if err != nil {
		// Unpriced. Left at zero, which core only reaches with no dollar cap
		// set — with one, estimate() refuses the Case before Authorize.
		return
	}
	out.CostUsdMicros = est.CostUSDMicros
}

// price returns the table row for a model, preferring the one the provider says
// answered.
//
// The resolved model is what was billed: `claude-sonnet-4-6` is an alias, and
// the dated ID behind it is what appears on the invoice. Falling back to the
// requested name matters for the reverse case, where the provider resolves to
// something the table has never heard of and the alias is priced.
func price(resolved, requested string) (*knov1.Price, bool) {
	if resolved != "" {
		if p, ok := pricing.Lookup(Scheme, resolved); ok {
			return p, true
		}
	}
	return pricing.Lookup(Scheme, requested)
}

// costOf prices a usage block.
//
// Four rates, not two. Anthropic bills fresh input, cache writes, cache reads,
// and output at four different numbers, and settling cache reads at the fresh
// input rate overstates every cached Case by 10x on that term — a systematic
// divergence from the user's invoice, in the direction that looks like Kno
// working correctly.
//
// Rounded UP per dimension, matching pricing.perMTok. At most four micro-dollars
// per Case, against a Case that costs thousands, and it is the direction that
// cannot make a cap silently loose.
func costOf(p *knov1.Price, u *usage) int64 {
	return add(
		micros(p.GetInputPerMtokUsdMicros(), tokens(u.InputTokens)),
		micros(p.GetCacheWritePerMtokUsdMicros(), tokens(u.CacheCreationInputTokens)),
		micros(p.GetCachedInputPerMtokUsdMicros(), tokens(u.CacheReadInputTokens)),
		micros(p.GetOutputPerMtokUsdMicros(), tokens(u.OutputTokens)),
	)
}

// add sums the cost terms without wrapping, for any inputs.
//
// TOTAL, not merely correct for the inputs it happens to get. The previous
// version guarded with `sum > math.MaxInt64-t`, which itself overflows when t
// is negative and then returns MaxInt64 for a total of 98 — a guard that
// misfires into the very value it exists to avoid, and one that quietly made
// an overflow test pass for the wrong reason.
//
// Four saturated terms overflow into a NEGATIVE total, and core settles that
// straight into Guard.Settle, which adds unchecked: a negative spend makes the
// guard's remaining headroom LARGER the worse the provider's numbers get, and
// Guard.Spent() — the number a report shows and the number Guard.Restore
// re-reads on resume — goes negative with it.
//
// Saturation is defence in depth only. usable already refuses any usage block
// whose dimensions exceed maxPlausibleTokens, which puts every term at most
// 5e8 micro-USD and makes overflow unreachable by construction.
func add(terms ...int64) int64 {
	var sum int64
	for _, t := range terms {
		switch {
		case t > 0 && sum > math.MaxInt64-t:
			return math.MaxInt64
		case t < 0 && sum < math.MinInt64-t:
			return math.MinInt64
		}
		sum += t
	}
	return sum
}

// micros converts a per-million-token rate and a token count to micro-USD,
// rounding up.
//
// Overflow-checked. A wrapped product lands small and positive, which reads as
// a cheap call rather than as an error, and nothing downstream would catch it.
//
// Local rather than shared with pricing.perMTok because this is its first
// occurrence in a settlement path; the second — openaicompat — is where an
// exported pricing.Settle earns its place. See the report accompanying this PR.
func micros(ratePerMTok, tokens int64) int64 {
	if tokens <= 0 || ratePerMTok <= 0 {
		return 0
	}
	const mtok = 1_000_000
	hi, lo := bits.Mul64(uint64(ratePerMTok), uint64(tokens))
	if hi != 0 || lo > math.MaxInt64-mtok {
		return math.MaxInt64
	}
	return (int64(lo) + mtok - 1) / mtok
}

// failure builds the Response and the error for a non-2xx.
//
// It returns BOTH. The Response describes a provider-side fact — a status, a
// latency, a cost — and core currently discards it on the error path, settling
// a flat one call with zero cost. That is docs/debt.md#43: a provider that bills
// a request which then errors is invisible to the guard and the store. This is
// the half an adapter can supply; the other half is core carrying the Response
// across an error, which this PR does not touch.
func (a *Agent) failure(c *core.Case, raw *transport.Response, latency time.Duration) (*core.Response, error) {
	env := decodeError(raw.Body)
	err := fromStatus(raw.StatusCode, raw.RetryAfter, env)

	out := &core.Response{
		CaseId:    c.GetId(),
		LatencyMs: latency.Milliseconds(),

		// The status and the provider's own words, sanitized: this string is
		// logged, persisted on the Outcome, and rendered in --json, and a
		// provider's 400 quotes parts of the request back.
		Error: errorTextOf(raw.StatusCode, env),

		// Zero, and measured rather than inferred. An error body carries no
		// usage block, so there is no reported cost — see docs/debt.md#43 for
		// what is NOT known here.
		CostUsdMicros: 0,
	}
	return out, err
}

// errorTextOf renders Response.error: one line, bounded, redacted.
func errorTextOf(status int, env *errorEnvelope) string {
	if env == nil {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d %s: %s",
		status, sanitize(env.ErrorType), sanitize(env.Message))
}

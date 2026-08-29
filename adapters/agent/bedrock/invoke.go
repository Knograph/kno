package bedrock

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

// Invoke runs one Case against Converse.
//
// Exactly one provider request per call, no retry — with ONE exception, the
// clock-skew retry below, which is bounded to a single extra request for the
// whole Agent. core owns retry, because each attempt takes its own budget
// reservation and settles its own call; an adapter retrying underneath would
// make N provider calls inside one reservation and settle them as one.
func (a *Agent) Invoke(ctx context.Context, c *core.Case) (*core.Response, error) {
	if c == nil {
		return nil, fmt.Errorf("bedrock: nil case")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	body, err := a.encode(c)
	if err != nil {
		return nil, err
	}

	return a.invoke(ctx, c, body)
}

// invoke sends the request, once normally and once more if the single allowed
// skew retry applies.
func (a *Agent) invoke(ctx context.Context, c *core.Case, body []byte) (*core.Response, error) {
	start := time.Now()
	raw, err := a.client.Do(ctx, http.MethodPost, conversePath(a.opts.Model), body)
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
		if a.retrySkew(raw) {
			// A fresh transport.Do rebuilds the request, which re-invokes the
			// Authorize hook and re-stamps x-amz-date from the clock — one
			// retry, one fresh signature. Bounded below, never a storm.
			return a.invoke(ctx, c, body)
		}
		return a.failure(c, raw, latency)
	}
	return a.success(c, raw, latency)
}

// retrySkew reports whether a 403 is the clock, and whether this Agent still
// has its one allowed retry.
//
// x-amz-date skew beyond AWS's 15-minute window answers 403 — the SAME status
// as a rejected credential, and the fix for the two is different. The plan's
// rule is "retry once with a fresh stamp, never a storm". A skewing clock
// skews every call, so the retry is per-Agent, atomic, once — the second Case
// to hit the same skew gets the honest terminal 403 rather than a second
// wasted request for every Case in the run.
func (a *Agent) retrySkew(raw *transport.Response) bool {
	if raw.StatusCode != http.StatusForbidden {
		return false
	}
	env := decodeError(raw.Body)
	if env == nil || !skew(env.Message) {
		return false
	}
	return a.retried.CompareAndSwap(false, true)
}

// encode turns a Case into a request body.
func (a *Agent) encode(c *core.Case) ([]byte, error) {
	system, msgs, err := a.compose(c)
	if err != nil {
		return nil, err
	}
	return encodeRequest(&converseRequest{
		Messages: msgs,
		System:   system,
		InferenceConfig: inferenceConfig{
			MaxTokens:   a.opts.MaxOutputTokens,
			Temperature: a.opts.Temperature,
		},
	})
}

// compose splits a Case into the two halves Converse takes.
//
// Same normalization as the anthropic adapter's compose, with Converse's
// shapes: the system prompt is an array of text blocks, and messages are
// content-block arrays. A history that BEGINS with an assistant turn is
// refused, because inventing a user turn to put in front of it would measure a
// prompt the Case does not describe.
func (a *Agent) compose(c *core.Case) ([]textBlock, []converseMessage, error) {
	// Omitted when empty: a present system array with an empty text block is
	// not the same as no system prompt, and it perturbs the cache prefix for
	// nothing.
	var system []textBlock
	if prefix := a.systemPrefix(); prefix != "" {
		system = []textBlock{{Text: prefix}}
	}
	var msgs []converseMessage

	for _, t := range c.GetHistory() {
		content := t.GetContent()
		switch t.GetRole() {
		case knov1.Role_ROLE_SYSTEM:
			system = append(system, textBlock{Text: content})
		case knov1.Role_ROLE_USER:
			msgs = appendTurn(msgs, roleUser, content)
		case knov1.Role_ROLE_ASSISTANT:
			msgs = appendTurn(msgs, roleAssistant, content)
		case knov1.Role_ROLE_TOOL:
			// A tool result reaches the model inside a USER turn. M2 sends no
			// tools, so this is only reached by an eval set that recorded one;
			// carrying the text as a user turn preserves what the model saw
			// better than dropping it.
			msgs = appendTurn(msgs, roleUser, content)
		default:
			return nil, nil, malformedCase(c,
				fmt.Errorf("a history turn has no role, so its place in the conversation is undefined"))
		}
	}

	if c.GetInput() == "" {
		return nil, nil, malformedCase(c,
			fmt.Errorf("the Case has no input, and Converse rejects an empty turn"))
	}
	msgs = appendTurn(msgs, roleUser, c.GetInput())

	if msgs[0].Role != roleUser {
		return nil, nil, malformedCase(c,
			fmt.Errorf("the history begins with an assistant turn, and Converse requires the first message to be a user turn"))
	}
	return system, msgs, nil
}

// malformedCase is a Case this adapter refuses to send.
//
// Terminal and unretryable, and refused BEFORE the request goes out, so it
// costs nothing.
func malformedCase(c *core.Case, cause error) error {
	return errs.ErrInvalidInput.
		WithFix("fix the Case in the evals file; Kno will not invent a turn to make it well formed").
		Wrap(fmt.Errorf("bedrock: case %s: %w", c.GetId(), cause))
}

// appendTurn adds content under role, joining it to the previous message when
// the role repeats. Empty content is dropped rather than sent.
func appendTurn(msgs []converseMessage, role, content string) []converseMessage {
	if content == "" {
		return msgs
	}
	if n := len(msgs); n > 0 && msgs[n-1].Role == role {
		last := len(msgs[n-1].Content) - 1
		msgs[n-1].Content[last].Text = join(msgs[n-1].Content[last].Text, content)
		return msgs
	}
	return append(msgs, converseMessage{Role: role, Content: []textBlock{{Text: content}}})
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
		// TERMINAL. The provider answered, so it billed; retrying pays twice
		// for one answer. See ErrMalformedResponse.
		return nil, ErrMalformedResponse.Wrap(err)
	}

	text := m.text()
	out := &core.Response{
		CaseId:     c.GetId(),
		Output:     text,
		ToolCalls:  m.toolCalls(),
		LatencyMs:  latency.Milliseconds(),
		StopReason: stopReasonOf(m.StopReason),

		// The authoritative refusal flag, set from stopReason and from nothing
		// else. A refusal is a SCORED Case, never an error: scoring it without
		// recording it is how an account whose safety settings decline every
		// Case produces 100% scored Cases, an aggregate of 0.000, and a clean
		// error rate — a confident reference number for a run in which the
		// agent was never measured.
		Refused: refused(m.StopReason),

		// Converse reports no backend build identifier. Deliberately left
		// empty rather than filled with a response id: the Run records the SET
		// of build identifiers observed, and a per-request id would make that
		// set one entry per Case.
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
// make Guard.Restore under-restore on resume by the full inferred cost of
// every Case — a resumed run spending its cap twice.
func (a *Agent) settle(out *core.Response, c *core.Case, m *converseResponse, text string) {
	u := m.Usage

	if u == nil || !u.usable(text) {
		a.settleFromEstimate(out, c)
		return
	}

	out.PromptTokens = u.billedInput()
	out.CompletionTokens = tokens(u.OutputTokens)
	out.CachedTokens = tokens(u.CacheReadInputTokens)

	p, ok := price(a.opts.Model)
	if !ok {
		// The tokens are MEASURED; only the rate is missing. So this is not an
		// estimate and must not claim to be one: usage_estimated says
		// "cost_usd_micros is the engine's own prediction", and there is no
		// prediction here — case.proto forbids pairing that flag with a zero,
		// and pairing it with one anyway is a false claim in exactly the
		// direction the flag exists to prevent.
		//
		// Unreachable under a cost cap: Estimate reads the same table, so a
		// model this cannot price is a model Estimate could not price either,
		// and core.estimate refuses such a Case before Authorize whenever
		// MaxCostUSDMicros is set.
		return
	}
	out.CostUsdMicros = a.regional(costOf(p, u))
}

// settleFromEstimate charges the reservation rather than nothing.
//
// Never zero. A zero settlement is what makes a dollar cap unenforceable.
// Under a pessimistic estimate this over-charges the budget, which is the
// recoverable direction: the guard refuses early rather than late. The
// estimate already carries the regional multiplier, so the settlement does
// too.
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

// price returns the table row for the requested model.
//
// Unlike the Messages API, Converse does not echo a resolved model id, so
// there is no resolved name to prefer — the requested id is the only candidate,
// and the version-suffix rule in pricing.Lookup resolves its dated pins.
func price(requested string) (*knov1.Price, bool) {
	return pricing.Lookup(Scheme, requested)
}

// costOf prices a usage block.
//
// Four rates, not two: fresh input, cache writes, cache reads, and output —
// the same four the Messages API bills, at the table's four rates. Rounded UP
// per dimension, matching pricing.perMTok.
func costOf(p *knov1.Price, u *usage) int64 {
	// Absent is not free. The fast rows publish no cached rate, and a cache
	// read can still happen unrequested — so a nil cached rate settles reads
	// at the FRESH input rate. The alternative silently discounts tokens the
	// invoice charges in full.
	cachedRate := p.GetCachedInputPerMtokUsdMicros()
	if p.CachedInputPerMtokUsdMicros == nil {
		cachedRate = p.GetInputPerMtokUsdMicros()
	}
	return add(
		micros(p.GetInputPerMtokUsdMicros(), tokens(u.InputTokens)),
		micros(p.GetCacheWritePerMtokUsdMicros(), tokens(u.CacheCreationInputTokens)),
		micros(cachedRate, tokens(u.CacheReadInputTokens)),
		micros(p.GetOutputPerMtokUsdMicros(), tokens(u.OutputTokens)),
	)
}

// add sums the cost terms without wrapping, for any inputs. See the anthropic
// adapter's add for the full reasoning — four saturated terms would otherwise
// wrap into a negative total, which Guard.Settle adds unchecked and turns into
// MORE headroom the worse the provider's numbers get.
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
// rounding up. Overflow-checked: a wrapped product lands small and positive,
// which reads as a cheap call rather than as an error.
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
// latency — and core currently discards it on the error path, settling a flat
// one call with zero cost. That is docs/debt.md#43: a provider that bills a
// request which then errors is invisible to the guard and the store. This is
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
		status, sanitize(env.Type), sanitize(env.Message))
}

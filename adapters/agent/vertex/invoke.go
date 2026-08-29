package vertex

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

// Invoke runs one Case against :rawPredict.
//
// Exactly one provider request per call, no retry: core owns retry, because
// each attempt takes its own budget reservation and settles its own call; an
// adapter retrying underneath would make N provider calls inside one
// reservation and settle them as one. The token cache's mid-run exchange is
// NOT a retry of the Case — it is the credential refresh the next Case needs
// anyway, and it fails as the Case's error, never as a storm.
func (a *Agent) Invoke(ctx context.Context, c *core.Case) (*core.Response, error) {
	if c == nil {
		return nil, fmt.Errorf("vertex: nil case")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	body, err := a.encode(c)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	raw, err := a.client.Do(ctx, http.MethodPost, rawPredictPath(a.creds.Project, a.creds.Region, a.opts.Model), body)
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
	return encodeRequest(&rawPredictRequest{
		AnthropicVersion: anthropicVersion,
		Messages:         msgs,
		System:           system,
		MaxTokens:        a.opts.MaxOutputTokens,
		Temperature:      a.opts.Temperature,
	})
}

// compose splits a Case into the two halves :rawPredict takes.
//
// Same normalization as the sibling adapters' compose: the system prompt is a
// top-level string, and messages are plain strings. A history that BEGINS
// with an assistant turn is refused, because inventing a user turn to put in
// front of it would measure a prompt the Case does not describe.
func (a *Agent) compose(c *core.Case) (string, []rawMessage, error) {
	// Omitted when empty: a present empty system string is not the same as no
	// system prompt, and it perturbs the cache prefix for nothing.
	system := a.systemPrefix()
	var msgs []rawMessage

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
			// A tool result reaches the model inside a USER turn. M2 sends no
			// tools, so this is only reached by an eval set that recorded one;
			// carrying the text as a user turn preserves what the model saw
			// better than dropping it.
			msgs = appendTurn(msgs, roleUser, content)
		default:
			return "", nil, malformedCase(c,
				fmt.Errorf("a history turn has no role, so its place in the conversation is undefined"))
		}
	}

	if c.GetInput() == "" {
		return "", nil, malformedCase(c,
			fmt.Errorf("the Case has no input, and :rawPredict rejects an empty turn"))
	}
	msgs = appendTurn(msgs, roleUser, c.GetInput())

	if msgs[0].Role != roleUser {
		return "", nil, malformedCase(c,
			fmt.Errorf("the history begins with an assistant turn, and :rawPredict requires the first message to be a user turn"))
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
		Wrap(fmt.Errorf("vertex: case %s: %w", c.GetId(), cause))
}

// appendTurn adds content under role, joining it to the previous message when
// the role repeats. Empty content is dropped rather than sent.
func appendTurn(msgs []rawMessage, role, content string) []rawMessage {
	if content == "" {
		return msgs
	}
	if n := len(msgs); n > 0 && msgs[n-1].Role == role {
		last := len(msgs) - 1
		msgs[last].Content = join(msgs[last].Content, content)
		return msgs
	}
	return append(msgs, rawMessage{Role: role, Content: content})
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

		// :rawPredict reports no backend build identifier. Deliberately left
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
func (a *Agent) settle(out *core.Response, c *core.Case, m *rawResponse, text string) {
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
// :rawPredict echoes the model id back in the response, but it echoes what
// was requested — there is no resolved alias to prefer — so the requested id
// is the only candidate, and the version-suffix rule in pricing.Lookup
// resolves its dated pins.
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

// errorTextOf renders what the provider said for the Response's Error field.
func errorTextOf(status int, env *errorEnvelope) string {
	if env == nil || env.Error == nil {
		return fmt.Sprintf("HTTP %d", status)
	}
	return sanitize(env.Error.Status) + ": " + sanitize(env.Error.Message)
}

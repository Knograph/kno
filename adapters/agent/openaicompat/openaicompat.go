// Package openaicompat is the adapter for the OpenAI Chat Completions shape.
//
// ONE adapter, base_url-configurable. `openai:gpt-5.6-sol` reaches OpenAI;
// `openai:llama-3.3-70b@https://api.groq.com/openai/v1` reaches Groq through
// the same code. There is deliberately no second "openai-compat" scheme:
// two user-visible names for one adapter is vocabulary drift, and the base URL
// is the only thing that actually differs.
//
// What lives here is the request and response SHAPE. Where a request may go,
// which credential may travel with it, how a rate limit is honored, and what an
// error may say all belong to adapters/agent/internal/transport, which every
// adapter shares. Reimplementing any of it here would give the security
// boundary two definitions and one of them would drift.
//
// The adapter does not retry. core owns retry, because each attempt takes its
// own budget reservation and settles its own call — a retry underneath the
// guard would make N provider calls inside one reservation and settle them as
// one.
package openaicompat

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/knograph/kno/adapters/agent/internal/transport"
	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Agent invokes a Chat Completions endpoint.
//
// Safe for concurrent use: every field is read-only after New, and the
// transport is built for concurrency.
type Agent struct {
	client *transport.Client

	model  string
	host   string
	scheme string

	// keyEnv is the NAME of the environment variable the credential for this
	// host came from, empty when none is bound. Kept so a 401 can name the
	// variable that was actually consulted rather than offering generic advice
	// about "your API key" — which is least useful exactly when the per-host
	// binding is the thing that went wrong.
	keyEnv string

	system string

	// asset is the injected Asset's content, empty on an Agent that carries
	// none. Set only by WithContext, and only on a COPY — the un-injected
	// receiver is the control arm of the measurement and must keep sending the
	// prompt it sent before.
	asset string

	maxOutput   int64
	maxPrompt   int
	temperature *float64
	seed        *int64
	legacyMax   bool

	// genParams is the RESOLVED answer to "does this endpoint accept sampling
	// parameters", after any override. Stored rather than recomputed, because
	// Capabilities recomputing it from the model name would report the static
	// matrix's answer while the requests carry the override's — two answers to
	// one question, and the capability matrix is the one users read.
	genParams bool

	// price is nil when the model has no row. Nil is NOT zero: Estimate
	// reports the absence as an error so core can refuse under a cost cap,
	// which is the one thing a zero would make impossible.
	price *knov1.Price

	// now reads the clock. Options.Now supplies it; nil means time.Now.
	//
	// The seam is on Options rather than only here, and that is the point: with
	// an unexported field and every test in an external package, nothing could
	// reach it — so "latency is measured" was an untested claim, and replacing
	// the measurement with a constant zero broke no test. A seam a test cannot
	// reach is not a seam.
	now func() time.Time
}

// Capabilities reports what this adapter supports.
//
// Answered from a static matrix, never by probing. Every claim here is
// something the adapter actually implements: declaring a capability it does not
// would let a valuation run report a measurement mode it never used.
//
// ContextInject is true for the ADAPTER, not for the individual Agent: it says
// this adapter implements core.ContextInjector, which is what V-4c checks
// before it routes an Asset and before it spends anything. An adapter that
// answered per-Agent would report false on the un-injected control arm, and the
// stage would refuse the Asset it was about to measure. KnowledgeWrite is false
// because a Chat Completions endpoint has no index to write. Stream is false
// per docs/debt.md#35.
func (a *Agent) Capabilities() *core.Capabilities {
	return &knov1.Capabilities{
		ContextInject:  true,
		KnowledgeWrite: false,
		Stream:         false,

		// The provider reports usage, so cost is MEASURED rather than
		// predicted — on the calls where it does. A reply with no usage block
		// settles at the reservation and sets Response.usage_estimated, which
		// is how a consumer tells the two apart per Case.
		TokenCounts: true,

		GenerationParams: a.genParams,
	}
}

// OutputCeiling reports the max_output_tokens this Agent sends.
//
// Exported because the number does not stay inside the adapter. It is the
// output term of every reservation, it is the point at which an answer is
// truncated — so it depresses a baseline through STOP_REASON_LENGTH rather than
// through anything the agent did — and §6 of the M2 plan requires it recorded
// on the Run and folded into InputFingerprint, so a resume with a different
// ceiling is refused rather than blended. None of that is reachable if the
// value is private to this package.
func (a *Agent) OutputCeiling() int64 { return a.maxOutput }

// Invoke runs one Case.
//
// A refusal, a truncation, and an answer are all RESPONSES. Only a failure to
// obtain one is an error. Getting that backwards is the failure §6 of the M2
// plan is built around: an account whose safety settings refuse every Case
// would otherwise produce 100% scored Cases, an aggregate of 0.000, and a clean
// error rate — a confident-looking reference number from a run in which the
// agent was never measured.
func (a *Agent) Invoke(ctx context.Context, c *core.Case) (*core.Response, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: openaicompat: nil case", errs.ErrInvalidInput)
	}

	prompt := a.promptOf(c)
	// The same arithmetic Estimate ran before the guard authorized this call.
	// Recomputed rather than passed in because Agent.Invoke takes only a Case —
	// and it must agree with Estimate, or a reply with no usage block would
	// settle at a number the guard never reserved.
	est, estErr := a.estimate(prompt)

	body, err := a.requestBody(c, prompt)
	if err != nil {
		return nil, err
	}

	start := a.now()
	resp, err := a.client.Do(ctx, http.MethodPost, "/chat/completions", body)
	elapsed := a.now().Sub(start)
	if err != nil {
		return nil, classify(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, a.errorFor(resp)
	}

	// The limiter's hold is NOT the model's latency. Client.Do waits inside
	// itself when a host is closed by an earlier 429, and returns WaitedFor
	// precisely so a caller can subtract it. Measured without this: 1002ms
	// recorded for a call the server answered instantly, because one earlier
	// 429 carried Retry-After: 1. Left in, latency_ms stops describing the
	// provider and starts describing our own pacing — and the number is
	// compared across adapters and across runs.
	latency := elapsed - resp.WaitedFor
	if latency < 0 {
		// Cannot happen from the arithmetic, but the clock is injectable and a
		// negative duration would become a negative latency_ms that no consumer
		// checks for.
		latency = 0
	}

	parsed, err := decodeResponse(resp.Body)
	if err != nil {
		return nil, errs.ErrInvalidInput.
			WithFix(fmt.Sprintf("check that %s speaks the OpenAI Chat Completions "+
				"shape at this base URL", a.host)).
			Wrap(err)
	}
	return a.mapResponse(c, parsed, latency, est, estErr)
}

// requestBody assembles the wire request for one Case.
func (a *Agent) requestBody(c *core.Case, prompt pricing.Prompt) ([]byte, error) {
	if err := a.checkPromptSize(prompt); err != nil {
		return nil, err
	}

	msgs := make([]chatMessage, 0, len(c.GetHistory())+3)
	if a.system != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: a.system})
	}
	// Immediately after the system prompt and ahead of everything the Case
	// varies. [system][asset] is then a byte-identical PREFIX across an Asset's
	// whole sample, which is the shape a provider's prompt cache keys on — and
	// costOf prices a cache read far below fresh input. Placed after the
	// history instead, the Asset sits behind bytes that change every Case and
	// is billed fresh every time.
	if a.asset != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: a.asset})
	}
	for _, t := range c.GetHistory() {
		msgs = append(msgs, chatMessage{Role: roleOf(t.GetRole()), Content: t.GetContent()})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: c.GetInput()})

	req := &chatRequest{
		Model:       a.model,
		Messages:    msgs,
		Temperature: a.temperature,
		Seed:        a.seed,
		Stream:      false,
	}
	// Exactly one spelling of the output ceiling. Sending both makes the
	// request's meaning depend on which one the server reads last.
	ceiling := a.maxOutput
	if a.legacyMax {
		req.MaxTokens = &ceiling
	} else {
		req.MaxCompletionTokens = &ceiling
	}

	return encodeRequest(req)
}

// mapResponse turns a parsed reply into the Response the engine records.
//
// Every field the schema defines for a provider call is populated here, and
// which ones are populated is not a matter of effort: prompt_tokens and
// cost_usd_micros feed the budget, refused and stop_reason decide whether a
// score means anything, and resolved_model is what a resume compares so a
// re-pointed alias cannot blend two models into one aggregate.
func (a *Agent) mapResponse(
	c *core.Case,
	p *chatResponse,
	latency time.Duration,
	est budgetEstimate,
	estErr error,
) (*core.Response, error) {
	// A 200 carrying an error object is a provider failing inside a success
	// status. Several compatible gateways do this. Scoring it as an empty
	// answer would produce a complete baseline of 0.000 with nothing errored.
	//
	// Both refusals below carry p.Usage forward through a.billed. The provider
	// generated tokens, billed them, and only then said the call went wrong —
	// the usage block is already parsed and sitting in hand. Dropping it makes a
	// paid call settle at zero, which under --max-cost-usd is spend the cap
	// cannot see. See docs/debt.md#43.
	if p.Error != nil && p.Error.Message != "" {
		return nil, a.billed(errs.ErrInvalidInput.
			WithFix("check the model name and the request parameters for this endpoint").
			Wrap(fmt.Errorf("%s returned an error inside a 200 for %s: %s",
				a.host, a.model, describe(http.StatusOK, p.Error))), p.Usage)
	}
	if len(p.Choices) == 0 {
		return nil, a.billed(errs.ErrInvalidInput.
			WithFix(fmt.Sprintf("check that %s speaks the OpenAI Chat Completions "+
				"shape at this base URL", a.host)).
			Wrap(fmt.Errorf("%w: the reply carries no choices", errMalformedBody)), p.Usage)
	}
	choice := p.Choices[0]

	out := &knov1.Response{
		CaseId:          c.GetId(),
		Output:          choice.Message.Content,
		LatencyMs:       latency.Milliseconds(),
		StopReason:      stopReasonOf(choice.FinishReason),
		ResolvedModel:   p.Model,
		ProviderBuildId: p.SystemFingerprint,
		ToolCalls:       toolCallsOf(choice.Message.ToolCalls),
	}

	// Response.refused is AUTHORITATIVE for the run-level refusal count, and
	// the two providers express a decline differently — OpenAI reports
	// finish_reason "content_filter" on a filtered generation and a non-empty
	// message.refusal on a model-side decline. Deriving the count from
	// stop_reason alone would make it depend on which of the two happened.
	if choice.Message.Refusal != "" {
		out.Refused = true
		// The decline IS the answer for scoring purposes, and an empty output
		// with refused set would score as a missing answer rather than a
		// declined one.
		if out.Output == "" {
			out.Output = choice.Message.Refusal
		}
	}
	if out.GetStopReason() == knov1.StopReason_STOP_REASON_CONTENT_FILTER {
		out.Refused = true
	}

	a.settleUsage(out, p.Usage, est, estErr)
	return out, nil
}

// settleUsage fills the cost and token fields.
//
// The adapter stamps its OWN estimate when the provider reports no usage, and
// that placement is the point. core holds the reservation but the store
// persists spendOf(Response); charging the guard a reservation the store knows
// nothing about means Guard.Restore under-restores by the full inferred cost of
// every usage-less Case, and a resumed run spends that much of its cap twice.
// One derivation, on the Response, and guard and store agree.
//
// Never zero. A zero settlement is what made a dollar cap unenforceable in M1.
func (a *Agent) settleUsage(
	out *knov1.Response,
	u *chatUsage,
	est budgetEstimate,
	estErr error,
) {
	if usable := usableUsage(u); usable != nil {
		out.PromptTokens = usable.PromptTokens
		out.CompletionTokens = usable.CompletionTokens
		out.CachedTokens = cachedOf(usable)
		if a.price != nil {
			out.CostUsdMicros = costOf(a.price, out)
			return
		}
		// Tokens are measured, the price is not known. The cost is an absence,
		// not a zero — and marking it estimated is what tells a report that the
		// figure it is adding up is not derived from a price at all.
		out.UsageEstimated = true
		return
	}

	// No usable usage block. Settle at the reservation, which under pessimistic
	// estimation is a true ceiling: it over-charges the budget, and that is the
	// safe direction. estErr means the model is unpriced, and there is nothing
	// honest to put here — the flag says the number is not measured, and core
	// refuses an unpriced Case outright when a dollar cap is set.
	out.UsageEstimated = true
	if estErr == nil {
		out.CostUsdMicros = est.CostUSDMicros
		out.PromptTokens = est.PromptTokens
	}
}

// usableUsage reports the usage block only when it can be believed.
//
// A negative count is nonsense and an all-zero block is a claim that the call
// was free — the exact shape that makes a dollar cap unenforceable. Both are
// treated as ABSENT, which routes them to the pessimistic reservation rather
// than to a cheap-looking number. Measured wrong in the safe direction beats
// measured wrong in the direction prime directive 4 calls a P0.
// cachedOf reports the cache-read share of the prompt.
//
// Clamped to the prompt count: a provider claiming more cached tokens than it
// charged prompt tokens for would otherwise make costOf's fresh term negative,
// and the cheaper cached rate would be applied to tokens that were never billed
// at it. Extracted so the success path and the billed-failure path price an
// identical usage block identically — two copies of this arithmetic would let a
// failed call and a successful one disagree about the same numbers.
func cachedOf(u *chatUsage) int64 {
	d := u.PromptTokensDetails
	if d == nil || d.CachedTokens <= 0 {
		return 0
	}
	return min(d.CachedTokens, u.PromptTokens)
}

func usableUsage(u *chatUsage) *chatUsage {
	if u == nil {
		return nil
	}
	if u.PromptTokens < 0 || u.CompletionTokens < 0 {
		return nil
	}
	if u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return nil
	}
	return u
}

var (
	_ core.Agent     = (*Agent)(nil)
	_ core.Capable   = (*Agent)(nil)
	_ core.Estimator = (*Agent)(nil)
)

package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"net/url"
	"strings"

	"github.com/knograph/kno/adapters/agent/internal/agenterr"
	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// budgetEstimate is the adapter's own arithmetic.
//
// Richer than budget.Estimate by one field, and the field earns its place: when
// a provider returns no usage block the Response settles at the reservation,
// and Response.prompt_tokens has to carry the same input count the reservation
// was built from. budget.Estimate.Tokens is the SUM of input and output, so
// splitting it back apart afterwards would be guesswork.
type budgetEstimate struct {
	budget.Estimate

	// PromptTokens is the input term alone.
	PromptTokens int64
}

// Estimate reports the most one Invoke of c could cost.
//
// Local arithmetic over the pricing table, with no network call: this runs
// BEFORE the budget guard authorizes anything, so a request here would spend
// money outside the guard entirely.
//
// Pessimistic on every term — fresh input rates rather than cached, the full
// output ceiling rather than a typical answer — because it bounds a
// reservation, and a bound that can be too low is not a bound. Over-estimating
// only makes the guard refuse early, which is recoverable; under-estimating is
// how a run walks past its cap.
//
// An error means the cost is UNKNOWN, which is not the same as zero. core
// refuses the Case when a dollar cap is set and falls back to its own scalar
// when one is not. Two things produce one: a model with no row in the table,
// and a Case whose prompt exceeds MaxPromptBytes — the ceiling that makes
// WorstCase a bound rather than a guess.
func (a *Agent) Estimate(_ context.Context, c *core.Case) (budget.Estimate, error) {
	if c == nil {
		return budget.Estimate{}, fmt.Errorf("%w: openaicompat: nil case", errs.ErrInvalidInput)
	}
	est, err := a.estimate(a.promptOf(c))
	return est.Estimate, err
}

// WorstCase reports the most any single Case could cost, before any Case is
// seen.
//
// It can be answered because both terms are bounded up front: the output
// ceiling is MaxOutputTokens by construction, and the input is bounded by
// MaxPromptBytes — which this adapter ENFORCES rather than assumes, in Estimate
// and in Invoke alike. That enforcement is what makes this an upper bound and
// not a guess: there is no Case this Agent will send whose Estimate exceeds
// what is returned here.
//
// core plans concurrency against it (a run must not hold more than a quarter of
// its cap as un-spendable headroom) and quotes it to the human before the first
// call. Planning against a number the run will not reserve answers a different
// question — measured on an adapter pricing at $0.20 against a scalar of
// $0.001, the consent prompt quoted $0.06 for a run whose real exposure was
// $12.00.
//
// An UNPRICED model returns the zero Estimate, which core reads as "this
// adapter cannot plan" and falls back to its own scalar. That is the honest
// answer: inventing a cheap number here would make a cost cap look enforceable
// when it is not, and the per-Case refusal in Estimate is where an unpriced
// model under a cap is actually caught.
func (a *Agent) WorstCase() budget.Estimate {
	if a.price == nil {
		return budget.Estimate{}
	}
	// The ceiling is a TOTAL, and checkPromptSize enforces it against the same
	// four fields — so an injected Asset does not raise this number, it spends
	// part of it. Charging the Asset ON TOP would still be an upper bound, but a
	// loose one, and core plans concurrency against this: a WorstCase inflated
	// by an Asset the ceiling already covers makes the feasibility check hold
	// headroom for spend that cannot happen and run fewer Cases at once.
	//
	// Context carries the Asset rather than folding it into Input because the
	// term has to be visible at the call site. The price is identical either
	// way — the estimate reads Prompt only through its total size — which is
	// exactly why an omitted term here would be invisible in the number.
	worst := pricing.Prompt{
		Context: a.asset,
		Input:   strings.Repeat("x", a.maxPrompt-len(a.asset)),
	}
	est, err := pricing.EstimateWithPrice(a.price, a.model, worst, a.maxOutput)
	if err != nil {
		return budget.Estimate{}
	}
	return est
}

// estimate is the shared arithmetic behind Estimate, WorstCase, and the
// usage-less settlement path.
//
// One function on purpose. Invoke stamps this number onto
// Response.cost_usd_micros when the provider reports no usage, so if it
// disagreed with what Estimate returned, the guard would settle at a figure it
// never reserved — and the difference would show up as an unexplained overshoot
// rather than as the bug it is.
func (a *Agent) estimate(prompt pricing.Prompt) (budgetEstimate, error) {
	if err := a.checkPromptSize(prompt); err != nil {
		return budgetEstimate{}, err
	}
	if a.price == nil {
		// Run-fatal: a property of the MODEL, which does not change mid-run.
		// Under a dollar cap core refuses every Case it cannot price, so
		// without this a run made one refusal per Case and ended as "too many
		// cases errored" — naming nothing about pricing. See docs/debt.md#46.
		return budgetEstimate{}, agenterr.AsRunFatal(
			fmt.Errorf("%w: %s:%s has no row in the %s price table",
				pricing.ErrUnpriced, a.scheme, a.model, pricing.Version),
		)
	}
	est, err := pricing.EstimateWithPrice(a.price, a.model, prompt, a.maxOutput)
	if errors.Is(err, pricing.ErrUnpriced) {
		return budgetEstimate{}, agenterr.AsRunFatal(err)
	}
	if err != nil {
		return budgetEstimate{}, err
	}
	// Tokens is input plus the output ceiling, so the input term is what is
	// left after removing the ceiling. Derived rather than recounted: counting
	// a second time is a second implementation that can drift from the one the
	// reservation used.
	return budgetEstimate{Estimate: est, PromptTokens: est.Tokens - a.maxOutput}, nil
}

// checkCeilings refuses an output or prompt ceiling the estimate cannot work
// with.
//
// Refused here rather than discovered per Case, because of what the failure
// looks like otherwise. pricing.EstimateWithPrice rejects an output ceiling
// above its overflow bound, so an out-of-range MaxOutputTokens makes EVERY
// Estimate return an error and makes WorstCase return zero — and core reads a
// zero WorstCase as "this adapter cannot plan" and silently falls back to its
// own scalar. That is exactly the failure WorstCase's godoc exists to prevent,
// reached through a mistyped flag, and nothing in the run would say so: under a
// cost cap every Case is refused as unpriceable, and the message blames the
// pricing table rather than the ceiling.
//
// The prompt ceiling is checked for the same reason one exists at all: a
// negative or absurd value would make WorstCase meaningless while every
// per-Case Estimate still looked fine.
func (a *Agent) checkCeilings() error {
	if a.maxOutput > maxOutputCeiling {
		return errs.ErrInvalidInput.WithFix(fmt.Sprintf(
			"lower --max-output-tokens below %d", maxOutputCeiling,
		)).
			Wrap(fmt.Errorf("an output ceiling of %d is beyond any real model's "+
				"context window, and the cost arithmetic cannot bound it — every "+
				"Case would be refused as unpriceable", a.maxOutput))
	}
	if a.maxPrompt > maxPromptCeiling {
		return errs.ErrInvalidInput.WithFix(fmt.Sprintf(
			"lower --max-prompt-bytes below %d", maxPromptCeiling,
		)).
			Wrap(fmt.Errorf("a prompt ceiling of %d bytes is past anything a "+
				"provider will accept, so it would bound nothing while still "+
				"inflating the planned cost of every Case", a.maxPrompt))
	}
	return nil
}

// maxOutputCeiling mirrors pricing's own overflow bound.
//
// Duplicated rather than imported because pricing keeps it unexported. The two
// must agree, and TestAnOutOfRangeCeilingIsRefusedAtConstruction asserts they
// do by driving a value one past this through pricing itself.
const maxOutputCeiling = 10_000_000

// maxPromptCeiling bounds MaxPromptBytes.
//
// 64 MiB: two orders of magnitude above any published context window, and well
// below where the byte-count arithmetic could overflow.
const maxPromptCeiling = 64 << 20

// checkPromptSize enforces the ceiling WorstCase depends on.
//
// Refused locally rather than sent and refused remotely. A prompt past this
// ceiling would come back as a context-length 400 from the provider — and
// docs/debt.md#43 records that whether a provider bills a request it then
// rejects is not something we can observe, so the free refusal is strictly
// better than the paid one.
func (a *Agent) checkPromptSize(prompt pricing.Prompt) error {
	size := promptSize(prompt)
	if size <= a.maxPrompt {
		return nil
	}
	return errs.ErrInvalidInput.WithFix(fmt.Sprintf(
		"shorten the Case, or raise --max-prompt-bytes above %d — note that "+
			"raising it also raises the planned cost of every Case, so fewer run "+
			"concurrently under a cost cap", size,
	)).
		Wrap(fmt.Errorf("the assembled prompt is %d bytes and this adapter sends "+
			"at most %d; the ceiling is what makes the run's planned cost a bound "+
			"rather than a guess", size, a.maxPrompt))
}

// promptOf assembles everything the provider will bill as input.
//
// Named parts rather than one string, because the billed prompt is not
// Case.input: it is the system prompt, plus the injected Asset, plus the Case's
// history, plus its input. A signature taking a single "input" makes the
// forgotten term invisible in the number rather than visible at the call site.
//
// Context carries the injected Asset, and it is the term that makes the cost
// cap bind on the thing being measured: it is typically the LARGEST part of the
// prompt, so an estimate that omitted it would reserve against a Case-sized
// prompt while sending an Asset-sized one. Empty on an Agent that carries no
// Asset, which is every Agent WithContext has not copied.
func (a *Agent) promptOf(c *core.Case) pricing.Prompt {
	var history strings.Builder
	for _, t := range c.GetHistory() {
		// Role names are billed too — they are tokens in the request — so they
		// are counted rather than only the content.
		history.WriteString(roleOf(t.GetRole()))
		history.WriteString(t.GetContent())
	}
	return pricing.Prompt{
		System:  a.system,
		Context: a.asset,
		History: history.String(),
		Input:   c.GetInput(),
	}
}

// promptSize reports the assembled prompt's size in bytes.
//
// pricing.Prompt keeps its own size unexported, so this recomputes it. The two
// must agree, and a test asserts the ceiling is applied to the same bytes the
// estimate prices.
func promptSize(p pricing.Prompt) int {
	return len(p.System) + len(p.Context) + len(p.History) + len(p.Input)
}

// costOf prices a reply from the tokens the provider actually reported.
//
// Three terms, because the price is a vector and not a pair: cached input is
// billed far below fresh input, and pricing every reported prompt token at the
// fresh rate overstates spend systematically — in exactly the direction a user
// notices as divergence from their invoice.
//
// A provider that reports cached tokens for a model whose row publishes no
// cached rate has them billed at the FRESH rate. Absent is not free: the
// alternative would silently discount tokens the invoice charges in full.
//
// Rounds UP, matching the reservation arithmetic. A settlement that truncated
// would shave a fraction off every Case, and a total that is systematically a
// little low is a total that eventually disagrees with the bill.
func costOf(p *knov1.Price, r *knov1.Response) int64 {
	cached := r.GetCachedTokens()
	fresh := r.GetPromptTokens() - cached
	if fresh < 0 {
		fresh = 0
	}

	cachedRate := p.GetCachedInputPerMtokUsdMicros()
	if p.CachedInputPerMtokUsdMicros == nil {
		cachedRate = p.GetInputPerMtokUsdMicros()
	}

	return saturatingAdd(
		perMTok(p.GetInputPerMtokUsdMicros(), fresh),
		saturatingAdd(
			perMTok(cachedRate, cached),
			perMTok(p.GetOutputPerMtokUsdMicros(), r.GetCompletionTokens()),
		),
	)
}

// perMTok converts a per-million-token rate and a token count to micro-USD,
// rounding up.
//
// Overflow-checked. A provider is free to report any token count it likes, and
// a wrapped product lands small and POSITIVE — which reads as a cheap call
// rather than as the nonsense it is, and nothing downstream would catch it.
func perMTok(ratePerMTok, tokens int64) int64 {
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

// saturatingAdd clamps rather than wrapping, for the same reason perMTok does.
func saturatingAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// parseBase parses a base URL.
func parseBase(raw string) (*url.URL, error) { return url.Parse(raw) }

// mustHost extracts the host from a constant base URL at package init.
//
// Panicking is right here and only here: the argument is DefaultBaseURL, a
// compile-time constant in this package, so a failure is a programmer error
// that every test would hit immediately rather than a user's input.
func mustHost(raw string) string {
	h, err := hostOf(raw)
	if err != nil {
		panic("openaicompat: DefaultBaseURL is not a valid URL: " + err.Error())
	}
	return h
}

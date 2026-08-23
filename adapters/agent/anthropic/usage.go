package anthropic

// This file is everything the adapter believes about a reported usage block:
// what the fields mean, when they may be trusted, and when a zero is a
// measurement rather than an absence.
//
// Separate from format.go because the two answer different questions. format.go
// answers "what shape did the provider send"; this answers "may that shape be
// turned into money". The second is where every settlement bug in this adapter
// has lived, and it deserves to be readable on its own.

// usage is Anthropic's usage block.
//
// Shaped differently from OpenAI's in the way that matters most: InputTokens
// counts only the tokens AFTER the last cache breakpoint, so billed input is
// the SUM of all three input fields. Reading InputTokens as "prompt tokens"
// under-reports a cached request by the whole cached prefix — 200,050 real
// input tokens reported as 50, in Anthropic's own documented example.
type usage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
}

// tokens reads a usage field, treating absent as zero for ARITHMETIC only.
//
// Presence is never inferred from the value it returns. Everything that decides
// what a zero MEANS — usable, unbilledRefusal — reads the pointer directly,
// because "the field was zero" and "the field was not there" settle
// differently and a helper that collapsed them would make that impossible to
// get right at the call site.
func tokens(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// maxPlausibleTokens bounds any single usage dimension.
//
// Mirrors pricing.maxOutputCeiling, and for the same reason: a count beyond any
// real context window is not describing a served request, and letting it
// through is how integer arithmetic stops being a safety property.
//
// The failure it closes is concrete and reachable through a supported
// configuration — a broken or hostile intermediary at --base-url. Three fields
// at MaxInt64 sum to 2^63-3, which is POSITIVE and passes every non-negativity
// check; the cost terms then saturate; and Guard.Settle adds the saturated
// value UNCHECKED, so two such Cases against a $1.00 cap leave spent negative,
// Remaining reporting more than the cap, and the guard authorizing again.
// Guard.Spent() — the number a report shows and the number Guard.Restore
// re-reads on resume — goes negative with it.
//
// Refusing here removes the saturation path entirely rather than relying on
// stats/budget being hardened later.
const maxPlausibleTokens = 10_000_000

// billedInput is the total input the provider charges for. The sum, not
// InputTokens — see the usage godoc.
//
// Safe to add only because usable has already bounded every dimension by
// maxPlausibleTokens. Called on an unvalidated block this would wrap: three
// fields at MaxInt64 sum to 2^63-3, which is positive and passes every
// non-negativity check downstream.
func (u *usage) billedInput() int64 {
	return tokens(u.InputTokens) + tokens(u.CacheCreationInputTokens) + tokens(u.CacheReadInputTokens)
}

// usable reports whether a usage block describes the response it arrived with.
//
// A block that disagrees with the body is worse than an absent one: absent is
// handled — estimate it and mark usage_estimated — while a block reporting zero
// output tokens for a full answer settles the Case at input-only cost, and a
// dollar cap then under-counts every Case in the run with nothing saying so.
//
// Deliberately only the clear-cut cases. A ratio test against our own token
// approximation would reject legitimate responses whenever the approximation
// was wrong, which is the whole reason it is called an approximation.
func (u *usage) usable(output string) bool {
	// Positive evidence, not the absence of contradiction. input_tokens and
	// output_tokens must be PRESENT: a block that omits them — `"usage": {}`,
	// or a future schema that moves them under a wrapper the way
	// cache_creation already nests alongside its scalar — decodes to zeros that
	// are indistinguishable from a provider reporting zero. Requiring presence
	// makes a schema move fall through to the estimate instead of settling a
	// silent zero for every Case in the run.
	if u.InputTokens == nil || u.OutputTokens == nil {
		return false
	}
	for _, f := range []*int64{
		u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens,
	} {
		if f == nil {
			continue
		}
		if *f < 0 || *f > maxPlausibleTokens {
			return false
		}
	}
	if u.billedInput() <= 0 {
		// Every request carries a prompt. Zero billed input means this block is
		// not describing a request that was served.
		return false
	}
	if output != "" && *u.OutputTokens <= 0 {
		return false
	}
	return true
}

// unbilledRefusal reports a refusal that fired before any output.
//
// Anthropic documents this case as not billed at all — no input tokens, no
// output tokens, no rate-limit consumption. Settling it at the pessimistic
// estimate instead would burn a run's whole cost cap on an account whose safety
// classifiers decline every Case, having spent nothing.
//
// All four conditions, because the mid-stream form of the same refusal DOES
// bill the output it already produced, and the two are one stop_reason apart.
func (m *messagesResponse) unbilledRefusal(text string) bool {
	if m.StopReason != stopRefusal {
		return false
	}
	// "Pre-output" is the load-bearing half. The MID-output form of the same
	// refusal bills what it already produced, and the two differ only in
	// whether there is text — so dropping this condition would settle a real,
	// billed refusal at zero.
	if text != "" {
		return false
	}
	u := m.Usage
	if u == nil {
		return false
	}
	// input_tokens must be PRESENT and explicitly zero. Inferring "not billed"
	// from a field that simply was not there means any schema move settles
	// every refused Case at $0 without even marking it inferred.
	if u.InputTokens == nil || *u.InputTokens != 0 {
		return false
	}
	return tokens(u.OutputTokens) == 0 &&
		tokens(u.CacheCreationInputTokens) == 0 &&
		tokens(u.CacheReadInputTokens) == 0
}

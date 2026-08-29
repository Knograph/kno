package bedrock

// This file is everything the adapter believes about a reported usage block:
// what the fields mean, when they may be trusted, and when a zero is a
// measurement rather than an absence.
//
// Same questions as the anthropic adapter's usage.go — Converse's billing
// semantics are the same family's — with the Converse spelling: camelCase,
// and a totalTokens that sums the three input fields.

// usage is Converse's usage block.
//
// InputTokens counts only the tokens AFTER the last cache breakpoint, exactly
// as on the Messages API, so billed input is the SUM of all three input
// fields. Reading InputTokens as "prompt tokens" under-reports a cached
// request by the whole cached prefix.
type usage struct {
	InputTokens              *int64 `json:"inputTokens"`
	OutputTokens             *int64 `json:"outputTokens"`
	CacheCreationInputTokens *int64 `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     *int64 `json:"cacheReadInputTokens"`

	// TotalTokens is the sum of the four above. Not read: it is the provider's
	// own addition of the same fields, and reading it would be a second,
	// unverifiable path to a number the three fields already make.
	TotalTokens *int64 `json:"totalTokens"`
}

// tokens reads a usage field, treating absent as zero for ARITHMETIC only.
//
// Presence is never inferred from the value it returns. Everything that
// decides what a zero MEANS reads the pointer directly, because "the field
// was zero" and "the field was not there" settle differently.
func tokens(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// maxPlausibleTokens bounds any single usage dimension.
//
// Mirrors the anthropic adapter's same constant, for the same reason: a count
// beyond any real context window is not describing a served request, and
// letting it through is how integer arithmetic stops being a safety property.
// The failure it closes is concrete — a broken or hostile intermediary at the
// endpoint — and it is documented in full in that godoc.
const maxPlausibleTokens = 10_000_000

// billedInput is the total input the provider charges for. The sum, not
// InputTokens — see the usage godoc.
//
// Safe to add only because usable has already bounded every dimension by
// maxPlausibleTokens.
func (u *usage) billedInput() int64 {
	return tokens(u.InputTokens) + tokens(u.CacheCreationInputTokens) + tokens(u.CacheReadInputTokens)
}

// usable reports whether a usage block describes the response it arrived
// with.
//
// A block that disagrees with the body is worse than an absent one: absent is
// handled — estimate it and mark usage_estimated — while a block reporting
// zero output tokens for a full answer settles the Case at input-only cost,
// and a dollar cap then under-counts every Case in the run with nothing
// saying so.
func (u *usage) usable(output string) bool {
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

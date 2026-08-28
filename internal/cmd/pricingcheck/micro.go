package main

import (
	"fmt"
	"math/big"
	"strings"
)

// microsPerMTok is a price in micro-USD per million tokens.
//
// Everything the detector reads is normalized to this unit on the way in, so
// the checks compare like with like: OpenRouter publishes USD per TOKEN as a
// decimal string, the two pages publish USD per MTok, and the schema's own
// Price fields are micro-USD per MTok. big.Rat keeps every conversion exact —
// a float in the middle is how a ratio check silently stops being a ratio.
type microsPerMTok = big.Rat

// microsPerMTokFromUSDPerToken converts an OpenRouter price — USD per token as
// a decimal string — to micro-USD per MTok.
//
// One USD per token is a million micro-USD per token, and one token is a
// millionth of an MTok, so the factor is 10^12. Exact in big.Rat arithmetic.
func microsPerMTokFromUSDPerToken(s string) (*microsPerMTok, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return nil, fmt.Errorf("not a decimal price %q", s)
	}
	return new(big.Rat).Mul(r, big.NewRat(1_000_000_000_000, 1)), nil
}

// microsPerMTokFromUSDPerMTok converts a page price — USD per million tokens,
// as printed on the provider's page — to micro-USD per MTok.
//
// A cell reads "$10 / MTok", "$4.00", or "5". The part before any slash is
// the number; currency and grouping punctuation are stripped. A cell that
// names no number ("—", "N/A", empty) is an error — on the tables this
// detector reads, every price is present, and a missing one is a changed
// layout, not an absent rate.
func microsPerMTokFromUSDPerMTok(s string) (*microsPerMTok, error) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	s = strings.ReplaceAll(s, "$", "")
	s = strings.ReplaceAll(s, ",", "")
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty price cell")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("not a decimal price %q", s)
	}
	return new(big.Rat).Mul(r, big.NewRat(1_000_000, 1)), nil
}

// formatUSDPerMTok renders a micro-USD-per-MTok price the way a price list
// prints it, for report lines. Display-only; the checks never compare through
// this.
func formatUSDPerMTok(r *microsPerMTok) string {
	f, _ := new(big.Rat).Quo(r, big.NewRat(1_000_000, 1)).Float64()
	return fmt.Sprintf("$%.2f", f)
}

// ratMul scales a rate by an exact rational multiplier.
func ratMul(r, by *big.Rat) *big.Rat {
	return new(big.Rat).Mul(r, by)
}

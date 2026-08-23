package anthropic

// Exported for tests in the same package tree.
//
// The cost arithmetic is unexported because nothing outside this package should
// call it, but it is also the only code here whose failure mode is a NUMBER
// rather than a behaviour — and a number reached through an httptest server is
// a number whose bad cases cannot all be provoked. The saturating add in
// particular has a precondition ("terms are non-negative") that its callers
// satisfy today; testing it only through Invoke is what let a guard that
// misfires on negative terms pass for a year.
var (
	// Add is the saturating sum of the cost terms.
	Add = add

	// Micros converts a per-million-token rate and a token count to micro-USD.
	Micros = micros

	// Sanitize bounds and cleans a provider-supplied string.
	Sanitize = sanitize

	// MaxProviderMessage is the byte ceiling Sanitize applies.
	MaxProviderMessage = maxProviderMessage

	// MaxPlausibleTokens is the per-dimension bound on a reported usage block.
	MaxPlausibleTokens = maxPlausibleTokens
)

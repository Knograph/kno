package bedrock

// Exported for tests in the same package tree.
//
// The SigV4 machinery is unexported because nothing outside this package
// should call it, but it is also the only code here whose failure mode is a
// SIGNATURE rather than a behaviour — and a signature is verified by AWS's
// published test vectors or not at all. The vector tests need the canonical
// request, the string-to-sign, and the signing key separately, because that is
// what the suite pins.
var (
	// CanonicalRequest builds the string a signature is computed over.
	CanonicalRequest = (*signer).canonicalRequest

	// CanonicalQuery encodes and sorts a URL's query parameters.
	CanonicalQuery = canonicalQuery

	// UriEncode applies AWS's URI encoding rules.
	UriEncode = uriEncode

	// EscapeModelID percent-encodes a model id for the URL path.
	EscapeModelID = escapeModelID

	// PayloadHash is sha256 over the exact request bytes.
	PayloadHash = payloadHash

	// EmptyPayloadHash is sha256(""), the payload hash of a bodyless request.
	EmptyPayloadHash = sigV4PayloadEmptyHash

	// NewSigner builds a signer; the test sets its clock explicitly.
	NewSigner = newSigner

	// CredentialScope is the date/region/service/aws4_request scope.
	CredentialScope = (*signer).credentialScope

	// SigningKey derives the per-request key from the secret.
	SigningKey = (*signer).signingKey

	// SignedHeaderNames joins a sorted header set into the canonical form.
	SignedHeaderNames = (*signer).signedHeaderNames

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

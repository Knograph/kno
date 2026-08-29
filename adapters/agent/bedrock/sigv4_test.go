package bedrock

// This file pins the SigV4 signer against AWS's published test-vector suite
// (docs.aws.amazon.com/general/latest/gr/signature-v4-test-suite.html). The
// suite is the only authority that says "the canonical request really looks
// like this": a hand-rolled signature scheme whose tests only check against
// itself proves nothing, and a 403 in production is the wrong place to learn
// that a header was folded wrong.
//
// The vectors are exercised in two layers:
//
//   - canonicalRequest / signingKey, the pure functions, against the suite's
//     creq, sts, and signature files. These pin the SHAPE of the signature —
//     the exact bytes AWS's spec computes — so a regression anywhere in the
//     chain fails with a known-answer.
//   - sign() end to end, against a self-consistent golden. sign() cannot be
//     checked against the suite directly: it stamps x-amz-content-sha256,
//     which joins the signed set, and the suite has no vector for that
//     header. The golden pins the whole Authorization header instead, which
//     locks the wire format — header names, sorting, the %3A in the canonical
//     URI — and is asserted for consistency with the canonical chain at the
//     same time.
//
// The vector files themselves are not vendored: the pinned values are the
// test's constants, with the vector's file name on the same line so a
// re-download can be diffed against the published suite.

import (
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

// vectorCreds are the suite's fixed identity.
var vectorCreds = newSigner(
	"AKIDEXAMPLE",
	"wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	"", // no session token on the non-STS vectors
	"us-east-1",
	"service",
)

// vectorNow is the suite's fixed clock: 20150830T123600Z.
func vectorNow() time.Time {
	return time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
}

// vector is one published test case.
type vector struct {
	name    string
	method  string
	target  string
	headers map[string]string

	// creq is the file's canonical-request content. The suite's files end the
	// canonical request with a trailing newline (the creq files were generated
	// by echo); the vector value here is the string WITHOUT that final
	// newline, which is the string the signature is actually computed over —
	// the sts hash in the same file proves the shape, and the sig proves the
	// hash.
	creq string

	// stsHash is the string-to-sign's fourth line: hex(sha256(creq)).
	stsHash string

	// sig is the suite's expected Signature value.
	sig string
}

func vectorReq(v vector) *http.Request {
	req, err := http.NewRequest(v.method, v.target, nil)
	if err != nil {
		panic(err)
	}
	// The suite's requests carry Host as a header; Go carries it separately,
	// and the signer reads req.Host when set. Setting it by hand is what lets
	// the suite's header list match ours.
	req.Host = "example.amazonaws.com"
	for name, value := range v.headers {
		req.Header.Set(name, value)
	}
	return req
}

// TestSigV4Vectors pins the canonical request, the string-to-sign, and the
// signature for the suite's cases. The vector files for these cases are known
// good (the ones whose published files are corrupted are not included).
func TestSigV4Vectors(t *testing.T) {
	t.Parallel()

	// The session token of post-sts-header-before. Its length is the point:
	// a token this long in a signed header exercises the exact canonical
	// rendering of a multi-hundred-byte value.
	const stsToken = "AQoDYXdzEPT//////////wEXAMPLEtc764bNrC9SAPBSM22wDOk4x4HIZ8j4FZTwdQWLWsKWHGBuFqwAeMicRXmxfpSPfIeoIYRqTflfKD8YUuwthAx7mSEI/qkPpKPi/kMcGdQrmGdeehM4IC1NtBmUpp2wUE8phUZampKsburEDy0KPkyQDYwT7WZ0wq5VSXDvp75YU9HFvlRd8Tx6q6fE8YQcHNVXAkiY9q6d+xo0rKwT38xVqr7ZD0u0iPPkUL64lIZbqBAz+scqKmlzm8FDrypNC9Yjc8fPOLn9FX9KSYvKTr4rvx3iSIlTJabIQwj2ICCR/oLxBA=="

	tests := []vector{
		{
			// get-vanilla: the shape every other vector builds on.
			name: "get-vanilla", method: http.MethodGet,
			target:  "https://example.amazonaws.com/",
			headers: map[string]string{"X-Amz-Date": "20150830T123600Z"},
			creq: "" +
				"GET\n" +
				"/\n" +
				"\n" +
				"host:example.amazonaws.com\n" +
				"x-amz-date:20150830T123600Z\n" +
				"\n" +
				"host;x-amz-date\n" +
				"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			stsHash: "bb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63",
			sig:     "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			// get-relative: RFC 3986 dot-segment removal. The request line is
			// "GET /example/.. HTTP/1.1" and the canonical URI is "/" — Go's
			// url.Parse does NOT resolve the ".." (it is the server's job), so
			// the signer's own normalization is what passes this vector.
			name: "get-relative", method: http.MethodGet,
			target:  "https://example.amazonaws.com/example/..",
			headers: map[string]string{"X-Amz-Date": "20150830T123600Z"},
			creq: "" +
				"GET\n" +
				"/\n" +
				"\n" +
				"host:example.amazonaws.com\n" +
				"x-amz-date:20150830T123600Z\n" +
				"\n" +
				"host;x-amz-date\n" +
				"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			stsHash: "bb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63",
			sig:     "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			// get-vanilla-query-order-key: the query's VALUE order, not its
			// key order, decides the canonical form. Sent "Param1=value2&
			// Param1=Value1", canonicalized "Param1=Value1&Param1=value2".
			name: "get-vanilla-query-order-key", method: http.MethodGet,
			target:  "https://example.amazonaws.com/?Param1=value2&Param1=Value1",
			headers: map[string]string{"X-Amz-Date": "20150830T123600Z"},
			creq: "" +
				"GET\n" +
				"/\n" +
				"Param1=Value1&Param1=value2\n" +
				"host:example.amazonaws.com\n" +
				"x-amz-date:20150830T123600Z\n" +
				"\n" +
				"host;x-amz-date\n" +
				"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			stsHash: "704b4cef673542d84cdff252633f065e8daeba5f168b77116f8b1bcaf3d38f89",
			sig:     "eedbc4e291e521cf13422ffca22be7d2eb8146eecf653089df300a15b2382bd1",
		},
		{
			// get-unreserved: every RFC 3986 unreserved character passes
			// through the canonical URI untouched.
			name: "get-unreserved", method: http.MethodGet,
			target:  "https://example.amazonaws.com/-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz",
			headers: map[string]string{"X-Amz-Date": "20150830T123600Z"},
			creq: "" +
				"GET\n" +
				"/-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz\n" +
				"\n" +
				"host:example.amazonaws.com\n" +
				"x-amz-date:20150830T123600Z\n" +
				"\n" +
				"host;x-amz-date\n" +
				"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			stsHash: "6a968768eefaa713e2a6b16b589a8ea192661f098f37349f4e2c0082757446f9",
			sig:     "07ef7494c76fa4850883e2b006601f940f8a34d404d0cfa977f52a65bbf5f24f",
		},
		{
			// get-vanilla-utf8-query: non-ASCII is percent-encoded as UTF-8
			// bytes, uppercase hex. Sent "?ሴ=bar", canonicalized
			// "%E1%88%B4=bar".
			name: "get-vanilla-utf8-query", method: http.MethodGet,
			target:  "https://example.amazonaws.com/?ሴ=bar",
			headers: map[string]string{"X-Amz-Date": "20150830T123600Z"},
			creq: "" +
				"GET\n" +
				"/\n" +
				"%E1%88%B4=bar\n" +
				"host:example.amazonaws.com\n" +
				"x-amz-date:20150830T123600Z\n" +
				"\n" +
				"host;x-amz-date\n" +
				"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			stsHash: "eb30c5bed55734080471a834cc727ae56beb50e5f39d1bff6d0d38cb192a7073",
			sig:     "2cdec8eed098649ff3a119c94853b13c643bcf08f8b0a1d91e12c9027818dd04",
		},
		{
			// post-header-value-case: header NAMES are lowercased and sorted,
			// VALUES keep their case.
			name: "post-header-value-case", method: http.MethodPost,
			target: "https://example.amazonaws.com/",
			headers: map[string]string{
				"My-Header1": "VALUE1",
				"X-Amz-Date": "20150830T123600Z",
			},
			creq: "" +
				"POST\n" +
				"/\n" +
				"\n" +
				"host:example.amazonaws.com\n" +
				"my-header1:VALUE1\n" +
				"x-amz-date:20150830T123600Z\n" +
				"\n" +
				"host;my-header1;x-amz-date\n" +
				"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			stsHash: "d51ced243e649e3de6ef63afbbdcbca03131a21a7103a1583706a64618606a93",
			sig:     "cdbc9802e29d2942e5e10b5bccfdd67c5f22c7c4e8ae67b53629efa58b974b7d",
		},
		{
			// post-sts-header-before: a session token is a SIGNED header. The
			// signing key is the same chain (the token never touches it); what
			// this vector pins is that the token's exact bytes join the
			// canonical header set, sorted between x-amz-date and host.
			name: "post-sts-header-before", method: http.MethodPost,
			target: "https://example.amazonaws.com/",
			headers: map[string]string{
				"X-Amz-Date":           "20150830T123600Z",
				"X-Amz-Security-Token": stsToken,
			},
			creq: "" +
				"POST\n" +
				"/\n" +
				"\n" +
				"host:example.amazonaws.com\n" +
				"x-amz-date:20150830T123600Z\n" +
				"x-amz-security-token:" + stsToken + "\n" +
				"\n" +
				"host;x-amz-date;x-amz-security-token\n" +
				"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			stsHash: "c237e1b440d4c63c32ca95b5b99481081cb7b13c7e40434868e71567c1a882f6",
			sig:     "85d96828115b5dc0cfc3bd16ad9e210dd772bbebba041836c64533a82be05ead",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// A fresh signer per subtest: vectorCreds is a shared *signer, and
			// the now seam is written per test.
			s := *vectorCreds
			s.now = vectorNow
			req := vectorReq(tc)

			canonical, err := CanonicalRequest(&s, req, EmptyPayloadHash)
			if err != nil {
				t.Fatalf("canonicalRequest: %v", err)
			}
			if canonical != tc.creq {
				t.Errorf("canonical request mismatch\nwant:\n%s\nhave:\n%s", tc.creq, canonical)
			}

			sts := strings.Join([]string{
				sigV4Algorithm,
				req.Header.Get("X-Amz-Date"),
				CredentialScope(&s, req.Header.Get("X-Amz-Date")),
				hexString(sha256Sum([]byte(canonical))),
			}, "\n")
			hash := hex.EncodeToString(sha256Sum([]byte(canonical)))
			if hash != tc.stsHash {
				t.Errorf("string-to-sign hash mismatch: want %s, have %s", tc.stsHash, hash)
			}

			key := SigningKey(&s, "20150830")
			sig := hexString(hmacSHA256(key, []byte(sts)))
			if sig != tc.sig {
				t.Errorf("signature mismatch: want %s, have %s", tc.sig, sig)
			}
		})
	}
}

// TestSignEndToEndPinsAuthorization locks the full Authorization header that
// sign() stamps on a real Converse request, ARN model id included.
//
// Self-consistent rather than vector-pinned — sign() adds
// x-amz-content-sha256 to the request, which joins the signed set, and the
// suite has no vector carrying that header. What the golden CAN assert is
// agreement with the canonical chain: the signature recomputed here over the
// post-sign request must match the header's own signature, which is the
// property that actually breaks production requests (a signature that does
// not describe the request it rides on).
func TestSignEndToEndPinsAuthorization(t *testing.T) {
	t.Parallel()

	s := newSigner(
		"AKIDEXAMPLE",
		"wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		"", // no session token on this request
		"us-east-1",
		"bedrock",
	)
	s.now = vectorNow

	// The full ARN form of a Bedrock model id, as the CLI would hand it over.
	// Colons are %3A, the slash stays literal — see EscapeModelID.
	model := "arn:aws:bedrock:us-east-1:123456789012:foundation-model/anthropic.claude-sonnet-4-5-20250929-v1:0"
	body := []byte(`{"messages":[{"role":"user","content":[{"text":"hello"}]}],"inferenceConfig":{"maxTokens":1024}}`)

	req, err := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com"+conversePath(model), strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "bedrock-runtime.us-east-1.amazonaws.com"
	req.Header.Set("User-Agent", "kno-test/1.0")

	if err := s.sign(req, body); err != nil {
		t.Fatalf("sign: %v", err)
	}

	auth := req.Header.Get("Authorization")
	const wantCredential = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/bedrock/aws4_request"
	if !strings.HasPrefix(auth, wantCredential) {
		t.Fatalf("Authorization does not open with the credential scope:\n%s", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=host;user-agent;x-amz-content-sha256;x-amz-date") {
		t.Errorf("SignedHeaders mismatch in:\n%s", auth)
	}

	// The signature must describe the request it rides on. Recompute the
	// chain over the post-sign request — the same view AWS's servers take —
	// and demand agreement.
	canonical, err := CanonicalRequest(s, req, req.Header.Get("X-Amz-Content-Sha256"))
	if err != nil {
		t.Fatalf("canonicalRequest after sign: %v", err)
	}
	sts := strings.Join([]string{
		sigV4Algorithm,
		req.Header.Get("X-Amz-Date"),
		CredentialScope(s, req.Header.Get("X-Amz-Date")),
		hexString(sha256Sum([]byte(canonical))),
	}, "\n")
	wantSig := hexString(hmacSHA256(SigningKey(s, "20150830"), []byte(sts)))
	if !strings.HasSuffix(auth, "Signature="+wantSig) {
		t.Errorf("Authorization's signature does not match the request it rides on:\n%s\nrecomputed: %s", auth, wantSig)
	}

	// The canonical URI must carry the ARN's %3A. A signature computed over
	// the decoded path would still be self-consistent with nothing but a 403
	// to say so — this pin is the check that makes it visible.
	if !strings.Contains(canonical, "/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A123456789012%3Afoundation-model/") {
		t.Errorf("canonical request does not preserve the ARN's percent-encoding:\n%s", canonical)
	}

	// The payload hash must be over the exact body bytes — not the URL, not
	// the headers, not a re-encoded copy.
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != payloadHash(body) {
		t.Errorf("x-amz-content-sha256 mismatch: want %s, have %s", payloadHash(body), got)
	}

	if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Errorf("x-amz-date: want 20150830T123600Z, have %q", got)
	}
}

// TestEscapeModelID pins the exact encoding Bedrock requires in the URL path.
func TestEscapeModelID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id   string
		want string
	}{
		// The vendor name form: colons encoded, alphanumerics and dashes not.
		{"anthropic.claude-sonnet-4-5-20250929-v1:0", "anthropic.claude-sonnet-4-5-20250929-v1%3A0"},
		// The full ARN: the "foundation-model/" slash is path structure and
		// stays literal; every other separator is a colon and is encoded.
		{
			"arn:aws:bedrock:us-east-1:123456789012:foundation-model/anthropic.claude-sonnet-4-5-20250929-v1:0",
			"arn%3Aaws%3Abedrock%3Aus-east-1%3A123456789012%3Afoundation-model/anthropic.claude-sonnet-4-5-20250929-v1%3A0",
		},
		// An already-escaped id must not be double-escaped: % is not in the
		// unreserved set and the encoder is byte-wise, so "%" becomes "%25"
		// exactly once.
		{"a%3Ab", "a%253Ab"},
	}
	for _, tc := range tests {
		if got := EscapeModelID(tc.id); got != tc.want {
			t.Errorf("escapeModelID(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

// TestCanonicalURI pins the dot-segment rules in isolation.
func TestCanonicalURI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		target string
		want   string
	}{
		{"https://example.com/", "/"},
		{"https://example.com/example/..", "/"},
		{"https://example.com/example/.", "/example/"},
		{"https://example.com/a/../b", "/b"},
		{"https://example.com/a/../../b", "/b"},
		{"https://example.com/a/b/../..", "/"},
		{"https://example.com/a//../b", "/a/b"},
		// Percent-encoding survives dot-segment removal: %2E is not a dot
		// segment, and an ARN's %3A is untouched by the walk.
		{"https://example.com/x%2Ey/a/../b", "/x%2Ey/b"},
		{"https://example.com/model/arn%3Aaws%3Abedrock/../b", "/model/b"},
	}
	for _, tc := range tests {
		req := vectorReq(vector{method: http.MethodGet, target: tc.target})
		got := canonicalURI(req.URL)
		if got != tc.want {
			t.Errorf("canonicalURI(%s) = %q, want %q", tc.target, got, tc.want)
		}
	}
}

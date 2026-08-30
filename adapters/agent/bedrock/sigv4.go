package bedrock

// This file is SigV4 — AWS's request signing scheme — implemented with the
// standard library. It is pinned against AWS's published test-vector suite
// (docs.aws.amazon.com/general/latest/gr/signature-v4-test-suite.html), which
// is the only authority that says "the canonical request really looks like
// this", and against the Converse-specific goldens the suite cannot cover.
//
// Why hand-rolled rather than the AWS SDK: the SDK pulls a dependency tree
// larger than the engine for one HMAC chain, and a signature scheme this small
// is security-relevant exactly because it is readable. The vectors make the
// cost of a regression a failing test with a known-answer, not a 403 in
// production.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// SigV4 constants.
const (
	// sigV4Algorithm is the credential scope's scheme, and the algorithm word
	// the Authorization header opens with.
	sigV4Algorithm = "AWS4-HMAC-SHA256"

	// sigV4Terminator ends every credential scope.
	sigV4Terminator = "aws4_request"

	// sigV4PayloadEmptyHash is sha256(""), the payload hash of a bodyless
	// request. It appears in the AWS vectors and in every signature computed
	// before the payload hash header exists to be read from.
	sigV4PayloadEmptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	// sigV4DateLayout is the x-amz-date format: seconds, always UTC, no
	// fractional part.
	sigV4DateLayout = "20060102T150405Z"

	// sigV4DayLayout is the credential scope's date: the same stamp minus the
	// time.
	sigV4DayLayout = "20060102"
)

// signer signs requests for one credential and region.
//
// Not a global. Two Agents with different credentials must not be able to sign
// for each other's keys, and a signer is what carries one key.
type signer struct {
	accessKey    string
	secretKey    string
	sessionToken string
	region       string
	service      string

	// now supplies the clock, so a test can pin x-amz-date to a vector's value
	// instead of a live one. The wall clock is the only input a pinned test
	// cannot fix, and the whole signature changes with it.
	now func() time.Time
}

// newSigner builds a signer from env-provided credentials.
func newSigner(accessKey, secretKey, sessionToken, region, service string) *signer {
	return &signer{
		accessKey:    accessKey,
		secretKey:    secretKey,
		sessionToken: sessionToken,
		region:       region,
		service:      service,
		now:          time.Now,
	}
}

// sign signs req in place.
//
// Stamps the three x-amz- headers that are absent (the date, the payload hash,
// and the session token when the credential carries one), then the
// Authorization header computed over the request as it will go out.
//
// body is the EXACT bytes the request will send. The payload hash is
// sha256(body), and re-marshaling or re-encoding the body before signing
// produces a signature that AWS rejects with 403 — the hash must be over the
// same bytes the wire carries.
//
// Called once per wire request: the adapter builds a fresh request for every
// transport.Do, which is what makes the clock-skew retry a fresh stamp rather
// than a replay.
func (s *signer) sign(req *http.Request, body []byte) error {
	if req.Header.Get("X-Amz-Date") == "" {
		req.Header.Set("X-Amz-Date", s.now().UTC().Format(sigV4DateLayout))
	}
	if req.Header.Get("X-Amz-Content-Sha256") == "" {
		req.Header.Set("X-Amz-Content-Sha256", payloadHash(body))
	}
	if s.sessionToken != "" && req.Header.Get("X-Amz-Security-Token") == "" {
		// A session token (STS) is a SIGNED header, not an afterthought. AWS
		// computes the signature over it, and signing without it is the classic
		// "signature does not match" 403 that sends everyone hunting in the
		// wrong place.
		req.Header.Set("X-Amz-Security-Token", s.sessionToken)
	}

	canonical, err := s.canonicalRequest(req, req.Header.Get("X-Amz-Content-Sha256"))
	if err != nil {
		return err
	}

	amzDate := req.Header.Get("X-Amz-Date")
	scope := s.credentialScope(amzDate)

	sts := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		scope,
		hexString(sha256Sum([]byte(canonical))),
	}, "\n")

	signingKey := s.signingKey(amzDate[:8])
	sig := hexString(hmacSHA256(signingKey, []byte(sts)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sigV4Algorithm, s.accessKey, scope, s.signedHeaders(req), sig,
	))
	return nil
}

// canonicalRequest builds the string the signature is computed over.
//
// Four parts, exactly as the AWS spec names them: the HTTP method, the
// canonical URI (the path exactly as sent — EscapedPath preserves a caller's
// own percent-encoding, so an ARN model id's %3A survives — with RFC 3986 dot
// segments removed, per the spec's normalization step), the canonical query
// string (parameters URI-encoded with RFC 3986 rules and sorted), and the
// canonical headers with their names lowercased and sorted. The payload hash
// is the last line, taken from the header sign set, so a caller that pre-set
// the header is not re-hashed.
//
// Dot-segment removal happens HERE, not at url.Parse: Go's parser deliberately
// does not resolve ".." in a path (it is the server's job on receipt), so an
// un-normalized signer would sign "/example/.." while the server canonicalizes
// the same request line to "/" — a guaranteed mismatch. get-relative in the
// pinned suite is exactly this case: the canonical request there is "GET\n/"
// for a request line of "GET /example/.. HTTP/1.1".
func (s *signer) canonicalRequest(req *http.Request, payloadHash string) (string, error) {
	query, err := canonicalQuery(req.URL)
	if err != nil {
		return "", err
	}

	host := req.URL.Host
	if req.Host != "" {
		host = req.Host
	}

	// Every header on the request is signed, plus host, which Go carries in
	// Request.Host rather than in Header. Signing everything present is the
	// rule the pinned suite demonstrates — post-sts-header-before signs
	// host;x-amz-date;x-amz-security-token, and post-header-value-case signs
	// every header on the request — and it keeps the signed set exactly equal
	// to what the server can recompute.
	var hdrs []hdr
	for name, values := range req.Header {
		if strings.EqualFold(name, "Authorization") {
			continue
		}
		v := strings.TrimSpace(strings.Join(values, ","))
		// Sequential spaces collapse. "a  b" and "a b" are the same header
		// value after HTTP normalization, and signing them differently would
		// reject a legitimate request.
		v = strings.Join(strings.Fields(v), " ")
		hdrs = append(hdrs, hdr{strings.ToLower(name), v})
	}
	hdrs = append(hdrs, hdr{"host", strings.TrimSpace(host)})
	sort.Slice(hdrs, func(i, j int) bool { return hdrs[i].name < hdrs[j].name })

	// Each header line ends with a newline, and the section ends with an extra
	// one: the canonical form is "name:value\n" per header, then a blank line,
	// then the signed-header list. The pinned suite's canonical requests show
	// the shape exactly — every vector has that blank line.
	var b strings.Builder
	for _, h := range hdrs {
		b.WriteString(h.name)
		b.WriteByte(':')
		b.WriteString(h.value)
		b.WriteByte('\n')
	}

	return strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		query,
		b.String(),
		s.signedHeaderNames(hdrs),
		payloadHash,
	}, "\n"), nil
}

// canonicalURI is the path with RFC 3986 section 5.2.4's remove_dot_segments
// applied.
//
// Operated on the ESCAPED form, because that is the path as sent: a literal
// "." or ".." segment is a dot segment, a "%2E" is not, and percent-encoding
// like an ARN's %3A must survive untouched. The algorithm is the RFC's own —
// ".." removes the previous segment and is ignored past the root, and a
// trailing "." leaves the path's final slash in place — which is the
// normalization the server applies to the request line before it is routed.
func canonicalURI(u *url.URL) string {
	in := u.EscapedPath()
	if in == "" {
		return "/"
	}

	var out []byte
	for len(in) > 0 {
		switch {
		case strings.HasPrefix(in, "../"):
			in = in[3:]
		case strings.HasPrefix(in, "./"):
			in = in[2:]
		case strings.HasPrefix(in, "/./"):
			in = in[2:] // "/./" becomes "/"
		case in == "/.":
			in = "/"
		case strings.HasPrefix(in, "/../"):
			in = in[3:] // "/../" becomes "/", then the last output segment goes
			out = removeLastSegment(out)
		case in == "/..":
			in = "/"
			out = removeLastSegment(out)
		case in == "." || in == "..":
			in = ""
		default:
			// Move the first segment — the leading "/" through the next "/"
			// or the end — onto the output.
			i := 1
			for i < len(in) && in[i] != '/' {
				i++
			}
			out = append(out, in[:i]...)
			in = in[i:]
		}
	}

	if len(out) == 0 {
		return "/"
	}
	return string(out)
}

// removeLastSegment drops the final segment AND the slash in front of it, as
// the RFC's rule C prescribes. "/a/b" becomes "/a" and "/example" becomes the
// empty output — the following rule E then moves the bare "/" onto it, which
// is how "/example/.." resolves to "/".
func removeLastSegment(out []byte) []byte {
	if len(out) == 0 {
		return out
	}
	end := len(out) - 1
	for end > 0 && out[end] != '/' {
		end--
	}
	return out[:end]
}

// signedHeaders builds the SignedHeaders list for the Authorization header.
func (s *signer) signedHeaders(req *http.Request) string {
	names := make([]string, 0, len(req.Header)+1)
	for name := range req.Header {
		if strings.EqualFold(name, "Authorization") {
			continue
		}
		names = append(names, strings.ToLower(name))
	}
	names = append(names, "host")
	sort.Strings(names)
	return strings.Join(names, ";")
}

// signedHeaderNames joins a sorted header set into the canonical form.
func (s *signer) signedHeaderNames(hdrs []hdr) string {
	names := make([]string, 0, len(hdrs))
	for _, h := range hdrs {
		names = append(names, h.name)
	}
	sort.Strings(names)
	return strings.Join(names, ";")
}

// hdr is a canonical header under construction. Both canonicalRequest and
// signedHeaders need the same sorted, lowercased set, so both build it from
// the same type.
type hdr struct{ name, value string }

// canonicalQuery encodes and sorts a URL's query parameters.
//
// Encoded with RFC 3986 rules — unreserved characters pass through, everything
// else is %XX uppercase — NOT url.QueryEscape, which would encode a space as
// "+" and produce a signature AWS rejects. Sorted by name and then value, byte
// order: get-vanilla-query-order-key in the pinned suite sends
// "Param1=value2&Param1=Value1" and the canonical form is
// "Param1=Value1&Param1=value2".
func canonicalQuery(u *url.URL) (string, error) {
	if u.RawQuery == "" {
		return "", nil
	}
	params, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("bedrock: the request query could not be parsed: %w", err)
	}
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)

	// One entry per VALUE, not one per key. A repeated parameter is repeated
	// in the canonical form, sorted by its value — the pinned suite's
	// get-vanilla-query-order-key is exactly this.
	var pairs []string
	for _, name := range names {
		values := params[name]
		sort.Strings(values)
		for _, v := range values {
			pairs = append(pairs, uriEncode(name)+"="+uriEncode(v))
		}
	}
	return strings.Join(pairs, "&"), nil
}

// uriEncode applies AWS's URI encoding: RFC 3986 unreserved characters pass
// through, everything else is percent-encoded with uppercase hex.
//
// Deliberately not url.QueryEscape or url.PathEscape — the first turns spaces
// into "+" and the second leaves ":" and "@" unescaped, and AWS encodes both
// differently than either.
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z',
			c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// credentialScope is the date/region/service/aws4_request scope.
func (s *signer) credentialScope(amzDate string) string {
	return amzDate[:8] + "/" + s.region + "/" + s.service + "/" + sigV4Terminator
}

// signingKey derives the per-request key from the secret.
//
// Four nested HMACs: date, then region, then service, then the terminator.
// Deriving per day is why a stolen signature cannot be replayed tomorrow; the
// chain makes each step depend on the previous.
func (s *signer) signingKey(day string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+s.secretKey), []byte(day))
	kRegion := hmacSHA256(kDate, []byte(s.region))
	kService := hmacSHA256(kRegion, []byte(s.service))
	return hmacSHA256(kService, []byte(sigV4Terminator))
}

// payloadHash is sha256 over the exact request bytes.
func payloadHash(body []byte) string {
	return hexString(sha256Sum(body))
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write(data)
	return m.Sum(nil)
}

func hexString(b []byte) string { return hex.EncodeToString(b) }

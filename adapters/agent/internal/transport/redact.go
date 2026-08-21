package transport

import (
	"net/http"
	"net/url"
	"strings"
)

// redacted is what replaces a secret. Fixed rather than length-preserving: the
// length of a key is information about the key.
const redacted = "[redacted]"

// sensitiveHeaders are never rendered.
//
// A denylist is the wrong shape for scrubbing recorded fixtures, where the
// question is "what may be kept" — but it is the right shape here, where the
// question is "what must never be printed" and the caller controls which
// headers exist. RedactHeaders takes the allowlist approach for the fixture
// case; this covers error rendering.
var sensitiveHeaders = []string{
	"authorization",
	"x-api-key",
	"api-key",
	"cookie",
	"set-cookie",
	"proxy-authorization",
	"openai-organization",
	"openai-project",
	"anthropic-beta",
}

// RedactHeader reports the value to display for a header.
func RedactHeader(name, value string) string {
	n := strings.ToLower(name)
	for _, s := range sensitiveHeaders {
		if n == s {
			return redacted
		}
	}
	return value
}

// RedactHeaders returns a copy safe to render.
func RedactHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		for _, v := range vs {
			out.Add(k, RedactHeader(k, v))
		}
	}
	return out
}

// RedactURL removes userinfo from a URL before it is displayed.
//
// NewDestination refuses a URL carrying userinfo, so this covers the paths a
// URL can reach display without passing through there — a redirect Location, a
// provider echoing a request back in an error body.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User(redacted)
	return u.String()
}

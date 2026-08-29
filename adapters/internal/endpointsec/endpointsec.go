// Package endpointsec validates the hosts a Kno adapter may send a
// credential to, and builds the transport that enforces the same policy at
// dial time.
//
// Every adapter that carries a credential to a user-supplied host — an API
// key, a bearer token — must refuse hosts that are not safe for that
// credential to reach. This code was written a third time here: the agent
// transport (adapters/agent/internal/transport/destination.go), LangSmith,
// and Langfuse each carry their own copy, and the Langfuse copy says it
// verbatim. The copies stay where they are, deliberately — their error
// messages name the provider's own flags and environment variables, which a
// shared package cannot do without learning the vocabulary of every
// provider that imports it.
//
// New adapters must use this package instead of copying a fourth time.
// The cost of the copies is drift; the cost of a shared package that grows
// provider names is that the provider names silently disagree with the
// adapters that do not import it. This package is the third copy made
// explicit: it holds the policy, provider-neutral, and each adapter wraps
// its errors with its own name.
//
// The policy itself is the transport policy, kept verbatim:
//
//   - The endpoint must parse, must be https unless the caller opts into
//     plain HTTP, must carry no userinfo credential, and must have a host
//     that is either a canonical IP literal or a plausible DNS name.
//   - Link-local addresses (the cloud instance-metadata endpoint) are
//     refused with no override.
//   - Loopback and private addresses are refused unless the caller opts in.
//   - The hostname check is a config-time check only; the same policy is
//     re-applied to the RESOLVED address at dial time, because a name can
//     resolve to anywhere.
//   - Redirects are refused outright: Go strips Authorization on a
//     cross-host redirect but not a same-host one, and no policy is
//     stronger than no redirect.
package endpointsec

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Check validates an endpoint URL before any request is made.
//
// The credential travels to this host on every request, so the host itself
// must be one the credential is allowed to reach. Errors deliberately do not
// echo the URL: a malformed URL can still carry a credential — "https://
// user:sk-secret@host:notaport/v1" fails to parse — and quoting it back
// would repeat the credential into a log line.
func Check(raw string, allowInsecureBaseURL, allowPrivateAddress bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("the endpoint could not be parsed")
	}

	// Userinfo never travels. A user who typed https://sk-abc@host has put a
	// credential in a string that is persisted on the Run, emitted on
	// RunStarted, and rendered in --json and logs. Refused rather than
	// stripped: silently rewriting what someone typed hides that they leaked
	// it into their shell history too.
	if u.User != nil {
		return fmt.Errorf("the endpoint carries a credential in its userinfo; " +
			"remove it and set the credential through its environment variable instead")
	}

	switch u.Scheme {
	case "https":
	case "http":
		if !allowInsecureBaseURL {
			return fmt.Errorf("%s is plain HTTP, which sends the request and the "+
				"credential in the clear; use an https URL, or pass the "+
				"--allow-insecure-base-url flag to accept the risk", u.Host)
		}
	default:
		return fmt.Errorf("scheme %q is not http or https", u.Scheme)
	}

	if u.Host == "" {
		return fmt.Errorf("the endpoint has no host")
	}
	return CheckAddress(u.Hostname(), allowPrivateAddress)
}

// CheckAddress applies the private-address rules to a hostname.
//
// A hostname that is not a literal IP is allowed through: resolving it here
// would be a TOCTOU check anyway, since the name can resolve differently by
// the time the request goes out. The literal-IP cases are the ones worth
// refusing, because they are how someone reaches an internal endpoint
// deliberately.
func CheckAddress(hostname string, allowPrivateAddress bool) error {
	ip := net.ParseIP(hostname)
	if ip == nil {
		// A name that is not a canonical IP literal but is also not a
		// plausible DNS name is a spelling of an address chosen to slip past
		// this check. net.ParseIP rejects 127.1, 2130706433, and 0x7f.1; the
		// resolver accepts all three as 127.0.0.1. No provider is spelled
		// that way.
		if !plausibleDNSName(hostname) {
			return fmt.Errorf("%q is neither a hostname nor a canonical IP "+
				"address; non-canonical forms like 127.1 or 2130706433 resolve to "+
				"addresses this refuses to reach", hostname)
		}
		return nil
	}

	// Link-local is refused unconditionally, with no opt-in. 169.254.169.254
	// is the cloud instance-metadata endpoint: reaching it returns
	// credentials, and Kno persists what an eval fetched as a trace. There is
	// no configuration in which an eval dataset lives there.
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("%s is link-local, which is where cloud instance "+
			"metadata lives; this is refused with no override", ip)
	}

	if allowPrivateAddress {
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() {
		return fmt.Errorf("%s is a private address. A self-hosted deployment "+
			"is a real use, so this is opt-in — but the same request shape "+
			"reaches an internal service, and the credential would travel to it", ip)
	}
	return nil
}

// CheckResolved applies the address policy to an address the resolver
// actually produced.
//
// This is the enforcement half of the pair. Check reads a URL the user
// typed; this reads where the connection is about to go, which is the only
// thing that cannot be spelled around. A hostname resolving to
// 169.254.169.254 is refused here even though it looked like an ordinary
// name, and "localhost" is refused here as loopback even though the typed
// URL passed the config-time check.
func CheckResolved(addr string, allowPrivateAddress bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return CheckAddress(host, allowPrivateAddress)
}

// WithResolvedAddressCheck wraps a transport's dialer so the address policy
// is applied to what the resolver produced. Mirror of the langsmith
// adapter's and the agent transport's withResolvedAddressCheck.
//
// A caller-supplied transport that is not an *http.Transport (a test stub, a
// recording round-tripper) is returned untouched: there is no dialer to
// reach, so the configuration-time check is what applies to it.
func WithResolvedAddressCheck(t http.RoundTripper, allowPrivateAddress bool) http.RoundTripper {
	tr, ok := t.(*http.Transport)
	if !ok {
		return t
	}
	// Clone before touching it. Mutating the caller's transport in place
	// would install THIS policy on every other client sharing it.
	tr = tr.Clone()

	// Control, not DialContext. DialContext receives the address as WRITTEN —
	// "localhost:8000" — because Go resolves inside the dialer, so a check
	// there re-reads the hostname it was already given and proves nothing.
	// Control runs once per resolved address, immediately before connect,
	// with the literal IP. That is the only point where "where is this
	// actually going" has an answer.
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			return CheckResolved(address, allowPrivateAddress)
		},
	}
	tr.DialContext = dialer.DialContext
	return tr
}

// RefuseRedirect refuses every redirect, to be installed as the client's
// CheckRedirect.
//
// Go's net/http strips Authorization and Cookie headers on a cross-host
// redirect but NOT a same-host one — and the credential is not a per-host
// header. Refusing all redirects — same host or not — is the only safe
// policy, and the datasets-server endpoints have no legitimate reason to
// redirect.
func RefuseRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("refusing a redirect to %s; the credentials must not follow one", req.URL.Host)
}

// plausibleDNSName reports whether h looks like a hostname rather than an
// address in disguise.
//
// Deliberately strict about the last label: a DNS TLD cannot be all digits
// or start with one, which is exactly what the numeric IP spellings look
// like.
func plausibleDNSName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	labels := strings.Split(h, ".")
	last := labels[len(labels)-1]
	if last == "" && len(labels) > 1 { // trailing dot
		labels = labels[:len(labels)-1]
		last = labels[len(labels)-1]
	}
	if last == "" {
		return false
	}
	// A TLD is alphabetic (or punycode, which starts "xn--").
	for _, r := range last {
		if r >= '0' && r <= '9' {
			return false
		}
	}
	for _, l := range labels {
		if l == "" {
			return false
		}
		for _, r := range l {
			isOK := r == '-' || r == '_' ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !isOK {
				return false
			}
		}
	}
	return true
}

// Package transport is the shared HTTP layer every provider adapter sits on.
//
// It owns the things that must be identical across adapters and are dangerous
// to reimplement: which hosts a request may reach, which credential may travel
// to which host, how rate limits are honored, and what an error is allowed to
// say. Adapters bring the request and response shapes; they do not bring
// policy.
//
// Internal on purpose. Nothing outside adapters/agent should construct one of
// these, and the public surface stays the Ring-0 interfaces in core.
package transport

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Destination is a validated place a request may be sent, with the credential
// that is allowed to travel there.
//
// Constructed once per adapter and reused, so the policy checks below run at
// configuration time — where a mistake is a refusal the user can read — rather
// than per request, where it would be one more failure inside a run.
type Destination struct {
	// base is the endpoint root, already validated.
	base *url.URL

	// host is base's host[:port], the key that credentials bind to.
	host string

	// key is the credential for this host, empty when none is configured.
	key string

	// keyHeader names the header the credential travels in.
	keyHeader string
}

// Policy is what a caller has opted into.
//
// Every field defaults to the safe answer, so a zero Policy is the strictest
// one. Opting in is deliberate and per-risk rather than one blanket "insecure"
// switch, because the two risks below are genuinely different and a user who
// needs local Ollama should not also be waiving TLS to the public internet.
type Policy struct {
	// AllowInsecureHTTP permits a plain-HTTP base URL.
	AllowInsecureHTTP bool

	// AllowPrivateAddress permits loopback and RFC1918 destinations, which is
	// what a local vLLM or Ollama endpoint is.
	//
	// Link-local is NOT covered by this and cannot be opted into: 169.254.169.254
	// is the cloud instance-metadata endpoint, and a tool that fetches a URL and
	// persists the response body has no legitimate reason to reach it.
	AllowPrivateAddress bool
}

// Errors returned when a destination is refused.
var (
	// ErrRefusedDestination means the endpoint is not one Kno will send to.
	ErrRefusedDestination = fmt.Errorf("transport: refused destination")

	// ErrKeyBinding means a credential could not be resolved for a host, or was
	// bound in a way that would let it travel somewhere it should not.
	ErrKeyBinding = fmt.Errorf("transport: key binding")
)

// NewDestination validates rawURL under policy and binds key to its host.
//
// keyHeader is the header the credential travels in — "Authorization" for
// OpenAI-compatible endpoints, "x-api-key" for Anthropic. It matters here
// because Go's redirect handling strips only a fixed set of headers, and
// x-api-key is not among them; see CheckRedirect.
func NewDestination(rawURL, key, keyHeader string, p Policy) (*Destination, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a URL: %w", ErrRefusedDestination, rawURL, err)
	}

	// Userinfo never travels. A user who typed https://sk-abc@host/v1 has put a
	// credential in a string that is persisted on the Run, emitted on
	// RunStarted, and rendered in --json and logs. Refused rather than
	// stripped: silently rewriting what someone typed hides that they leaked it
	// into their shell history too.
	if u.User != nil {
		return nil, fmt.Errorf("%w: the URL carries a credential in its userinfo; "+
			"remove it and pass the key through the environment instead",
			ErrRefusedDestination)
	}

	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowInsecureHTTP {
			return nil, fmt.Errorf("%w: %s is plain HTTP, which sends the request "+
				"and any credential in the clear", ErrRefusedDestination, u.Host)
		}
	default:
		return nil, fmt.Errorf("%w: scheme %q is not http or https",
			ErrRefusedDestination, u.Scheme)
	}

	if u.Host == "" {
		return nil, fmt.Errorf("%w: %q has no host", ErrRefusedDestination, rawURL)
	}
	if err := checkAddress(u.Hostname(), p); err != nil {
		return nil, err
	}

	if key != "" && keyHeader == "" {
		return nil, fmt.Errorf("%w: a key was supplied with no header to carry it",
			ErrKeyBinding)
	}

	return &Destination{base: u, host: u.Host, key: key, keyHeader: keyHeader}, nil
}

// Host reports the host[:port] this destination is bound to.
func (d *Destination) Host() string { return d.host }

// checkAddress applies the private-address rules.
//
// A hostname that is not a literal IP is allowed through: resolving it here
// would be a TOCTOU check anyway, since the name can resolve differently by the
// time the request goes out. The literal-IP cases are the ones worth refusing,
// because they are how someone reaches an internal endpoint deliberately.
func checkAddress(hostname string, p Policy) error {
	ip := net.ParseIP(hostname)
	if ip == nil {
		return nil
	}

	// Link-local is refused unconditionally, with no opt-in. 169.254.169.254 is
	// the cloud instance-metadata endpoint: reaching it returns credentials,
	// and Kno persists response bodies as traces. There is no configuration in
	// which a language model lives there.
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("%w: %s is link-local, which is where cloud instance "+
			"metadata lives; this is refused with no override", ErrRefusedDestination, ip)
	}

	if p.AllowPrivateAddress {
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() {
		return fmt.Errorf("%w: %s is a private address. Local model servers are a "+
			"real use, so this is opt-in — but the same request shape reaches an "+
			"internal service, and Kno stores what comes back", ErrRefusedDestination, ip)
	}
	return nil
}

// authorize attaches the credential bound to this destination.
//
// It refuses to attach a key to a request aimed anywhere but the bound host.
// That is the invariant the whole type exists for: a key bound to host A never
// reaches host B, whatever redirect or misconfiguration got the request there.
func (d *Destination) authorize(req *http.Request) error {
	if d.key == "" {
		return nil
	}
	if !sameHost(req.URL.Host, d.host) {
		return fmt.Errorf("%w: the credential is bound to %s and this request is "+
			"aimed at %s", ErrKeyBinding, d.host, req.URL.Host)
	}
	req.Header.Set(d.keyHeader, d.key)
	return nil
}

// sameHost compares hosts case-insensitively, treating a trailing dot as
// equivalent — "api.example.com." and "API.example.com" are one host, and a
// comparison that said otherwise would refuse a legitimate request.
//
// Deliberately an exact string comparison after that, with no IDNA folding. A
// Unicode host and its punycode form would compare as different and the request
// would be refused — which is the safe direction. Folding them would mean
// deciding that two spellings are the same host, and getting that wrong sends a
// credential somewhere it was not bound.
func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}

// CheckRedirect refuses any redirect that leaves the bound host.
//
// Go's net/http strips Authorization, WWW-Authenticate, Cookie, and Cookie2 on
// a cross-domain redirect — and NOT x-api-key, which is how Anthropic
// authenticates. So a base URL that returns a 302 elsewhere, whether
// compromised or merely misconfigured, forwards that key verbatim, with no
// plain-HTTP hop to refuse and nothing the user did wrong.
//
// Rather than enumerate which headers are safe, this refuses the redirect. An
// API endpoint has no legitimate reason to send a request somewhere else.
func (d *Destination) CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("%w: stopped after %d redirects", ErrRefusedDestination, maxRedirects)
	}
	if !sameHost(req.URL.Host, d.host) {
		return fmt.Errorf("%w: %s redirected to %s, and a credential bound to the "+
			"first must not follow it to the second", ErrRefusedDestination,
			d.host, req.URL.Host)
	}
	return nil
}

// maxRedirects bounds same-host redirect chains. Same-host redirects are
// legitimate (a version path moving), but a loop is not.
const maxRedirects = 3

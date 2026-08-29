package langsmith

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// checkEndpoint validates the configured endpoint before any request is
// made.
//
// The API key travels to this host on every request, so the host itself must
// be one the key is allowed to reach. These rules are copied from
// adapters/agent/internal/transport's Destination check (destination.go) —
// the same policy the agent adapters enforce for the same reason — and are
// kept here, deliberately unshared, because that package is internal to
// adapters/agent and this adapter must not import it. If the two drift, this
// file is the one that needs reviewing.
func checkEndpoint(raw string, allowInsecureBaseURL, allowPrivateAddress bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		// Neither the URL nor the parse error is echoed. A malformed URL can
		// still carry a credential — "https://user:sk-secret@host:notaport/v1"
		// fails to parse, and both %q and url.Parse's own message would quote
		// it back, twice.
		return fmt.Errorf("langsmith: the endpoint could not be parsed")
	}

	// Userinfo never travels. A user who typed https://sk-abc@host has put a
	// credential in a string that is persisted on the Run, emitted on
	// RunStarted, and rendered in --json and logs. Refused rather than
	// stripped: silently rewriting what someone typed hides that they leaked
	// it into their shell history too.
	if u.User != nil {
		return fmt.Errorf("langsmith: the endpoint carries a credential in its userinfo; " +
			"remove it and set LANGSMITH_API_KEY instead")
	}

	switch u.Scheme {
	case "https":
	case "http":
		if !allowInsecureBaseURL {
			return fmt.Errorf("langsmith: %s is plain HTTP, which sends the request and the "+
				"API key in the clear; set LANGSMITH_ENDPOINT to an https URL, or pass "+
				"--allow-insecure-base-url to accept the risk", u.Host)
		}
	default:
		return fmt.Errorf("langsmith: scheme %q is not http or https", u.Scheme)
	}

	if u.Host == "" {
		return fmt.Errorf("langsmith: the endpoint %q has no host", raw)
	}
	return checkAddress(u.Hostname(), allowPrivateAddress)
}

// checkAddress applies the private-address rules. Copied from
// transport/destination.go's checkAddress, same rationale line for line.
//
// A hostname that is not a literal IP is allowed through: resolving it here
// would be a TOCTOU check anyway, since the name can resolve differently by
// the time the request goes out. The literal-IP cases are the ones worth
// refusing, because they are how someone reaches an internal endpoint
// deliberately.
func checkAddress(hostname string, allowPrivateAddress bool) error {
	ip := net.ParseIP(hostname)
	if ip == nil {
		// A name that is not a canonical IP literal but is also not a
		// plausible DNS name is a spelling of an address chosen to slip past
		// this check. net.ParseIP rejects 127.1, 2130706433, and 0x7f.1; the
		// resolver accepts all three as 127.0.0.1. No provider is spelled
		// that way.
		if !plausibleDNSName(hostname) {
			return fmt.Errorf("langsmith: %q is neither a hostname nor a canonical IP "+
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
		return fmt.Errorf("langsmith: %s is link-local, which is where cloud instance "+
			"metadata lives; this is refused with no override", ip)
	}

	if allowPrivateAddress {
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() {
		return fmt.Errorf("langsmith: %s is a private address. A self-hosted LangSmith "+
			"deployment is a real use, so this is opt-in — but the same request shape "+
			"reaches an internal service, and the API key would travel to it", ip)
	}
	return nil
}

// checkResolved applies the address policy to an address the resolver
// actually produced. Copied from transport/destination.go's checkResolved,
// same rationale line for line.
//
// This is the enforcement half of the pair. checkEndpoint reads a URL the
// user typed; this reads where the connection is about to go, which is the
// only thing that cannot be spelled around. A hostname resolving to
// 169.254.169.254 is refused here even though it looked like an ordinary
// name, and "localhost" is refused here as loopback even though the typed
// URL passed the config-time check.
func checkResolved(addr string, allowPrivateAddress bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return checkAddress(host, allowPrivateAddress)
}

// plausibleDNSName reports whether h looks like a hostname rather than an
// address in disguise. Copied from transport/destination.go.
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

package main

import (
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// Error kinds separate an unusable source from a source whose content failed
// a check. errShape (envelope wrong, transport failed) makes a source
// unreachable in live mode and an internal error in fixture mode; errSelect
// and errRow are content failures the checks judge and gate.
var (
	errShape  = errors.New("source shape unusable")
	errSelect = errors.New("source selection failed")
	errRow    = errors.New("source row malformed")
)

// fetchTimeout bounds a single source fetch. The sources are price-of-record
// pages, not a production dependency: a slow page is a failed check, and a
// bounded failure beats a hung run.
const fetchTimeout = 30 * time.Second

// maxFetchBytes caps a response body. The largest source is OpenRouter's model
// list at roughly 640 KB; ten MiB is an envelope-rejection threshold, not a
// target.
const maxFetchBytes = 10 << 20

// userAgent identifies this tool to the sources. Stable and honest: these are
// hand-entered prices in a dated table, and the people who run the pages
// deserve to know who is reading them.
const userAgent = "kno-pricingcheck/0.1 (https://github.com/knograph/kno)"

// sourceSpec names one price-of-record source and its fixture file.
type sourceSpec struct {
	name string
	url  string
	file string
}

// sources is the fixed set the detector reads. The host allowlist is derived
// from it, so a new source is an explicit reviewable addition, not a string a
// fetch can wander to.
var sources = []sourceSpec{
	{name: "openrouter", url: "https://openrouter.ai/api/v1/models", file: "openrouter.json"},
	{name: "anthropic", url: "https://platform.claude.com/docs/en/about-claude/pricing", file: "anthropic.html"},
	{name: "openai", url: "https://developers.openai.com/api/docs/models/compare", file: "openai.html"},
	// AWS's machine-readable Bedrock offers index: the price-of-record for the
	// +10% regional multiplier check (docs/debt.md#41(d) repayment). Vertex
	// has no equivalent machine-readable source — see checkRegionalMultiplier.
	{name: "bedrock", url: "https://api.pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonBedrock/current/index.json", file: "bedrock.json"},
}

// sourceURL returns the fetch URL for a named source.
func sourceURL(name string) string {
	for _, s := range sources {
		if s.name == name {
			return s.url
		}
	}
	return ""
}

// fetch is the program's network entry point, indirected so tests can stub
// it. Production code never calls http directly; loadSources and
// recordFixtures go through here.
var fetch = fetchHTTP

// fetchHTTP retrieves url under the fetch hygiene rules: a timeout, a
// response-size cap, a host allowlist, a stable identifying User-Agent, and
// no cross-host redirects. The open internet deserves the same scrutiny the
// plugin boundary gets.
func fetchHTTP(url string) ([]byte, error) {
	allow := make(map[string]bool, len(sources))
	for _, s := range sources {
		allow[hostOf(s.url)] = true
	}
	client := &http.Client{
		Timeout: fetchTimeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if !allow[req.URL.Host] {
				return fmt.Errorf("redirect to host %q refused: not in allowlist", req.URL.Host)
			}
			return nil
		},
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body; close error is not actionable
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}
	if len(body) > maxFetchBytes {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", url, maxFetchBytes)
	}
	return body, nil
}

// hostOf returns the host of a URL, without scheme or path.
func hostOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.Index(u, "/"); i >= 0 {
		u = u[:i]
	}
	return u
}

// row is one model's prices as one source reports them, normalized to
// micro-USD per MTok. A nil rate means the source does not publish that
// dimension: OpenRouter carries only input and output, and OpenAI's page
// publishes no cache-write rate.
// sourceData is one source's load outcome: rows when the source parsed, or
// the error that prevented parsing. errShape marks the source unusable —
// wrong envelope, transport failure — which is an internal error on fixtures
// and a fail-soft "no data" report live. errSelect and errRow are content
// failures the checks judge and gate on.
type sourceData struct {
	name    string
	rows    []row
	regions bedrockRegions // set only for the bedrock source
	err     error
}

// shapeBroken reports whether the source was unusable as data.
func (s sourceData) shapeBroken() bool {
	return s.err != nil && errors.Is(s.err, errShape)
}

type row struct {
	source    string // which source reported it
	scheme    string // table scheme the model belongs to ("anthropic", "openai")
	model     string // the id exactly as the source spells it
	canonical string // shared spelling, for cross-source matching

	input        *big.Rat
	cachedRead   *big.Rat
	cacheWrite5m *big.Rat
	cacheWrite1h *big.Rat
	output       *big.Rat
}

// canonicalModel reduces a model id to the shared spelling the cross-source
// check matches on: lowercase, with every run of non-alphanumerics a single
// dash. "claude-opus-4.8" and "Claude Opus 4.8" both become
// "claude-opus-4-8", and the table's own keys are already in this form.
//
// Raw ids stay raw everywhere the table is consulted — Lookup and the prefix
// rule see exactly what the source spelled. Canonicalization exists only to
// decide whether two SOURCES mean the same model.
func canonicalModel(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// stripParenthetical removes a parenthetical qualifier — "( retired, except
// on Bedrock and Google Cloud )" — so the model name that remains is the one
// the table keys by.
func stripParenthetical(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '(':
			depth++
		case r == ')':
			if depth > 0 {
				depth--
			}
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

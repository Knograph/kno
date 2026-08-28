package main

import (
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// readFixture loads a fixture relative to the package directory.
func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", path, err)
	}
	return body
}

func findRow(t *testing.T, rows []row, scheme, canonical string) row {
	t.Helper()
	for _, r := range rows {
		if r.scheme == scheme && r.canonical == canonical {
			return r
		}
	}
	t.Fatalf("no row for %s/%s among %d rows", scheme, canonical, len(rows))
	return row{}
}

// TestParseOpenRouterFixture parses the recorded capture of
// https://openrouter.ai/api/v1/models (2026-08-28).
func TestParseOpenRouterFixture(t *testing.T) {
	t.Parallel()
	rows, err := parseOpenRouter(readFixture(t, "testdata/openrouter.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) < 100 {
		t.Errorf("expected the full model list, got %d rows", len(rows))
	}
	opus := findRow(t, rows, "anthropic", "claude-opus-5")
	if opus.input.Cmp(big.NewRat(5_000_000, 1)) != 0 {
		t.Errorf("claude-opus-5 input = %s, want $5.00/MTok", opus.input)
	}
	if opus.output.Cmp(big.NewRat(25_000_000, 1)) != 0 {
		t.Errorf("claude-opus-5 output = %s, want $25.00/MTok", opus.output)
	}
	sol := findRow(t, rows, "openai", "gpt-5-6-sol")
	// OpenRouter reports gpt-5.6-sol at half of OpenAI's page price; this is
	// the divergence the suppression exists for.
	if sol.input.Cmp(big.NewRat(2_000_000, 1)) != 0 {
		t.Errorf("gpt-5.6-sol input = %s, want $2.00/MTok", sol.input)
	}
}

// TestParseAnthropicFixture parses the recorded capture of
// https://platform.claude.com/docs/en/about-claude/pricing (2026-08-28).
func TestParseAnthropicFixture(t *testing.T) {
	t.Parallel()
	rows, err := parseAnthropic(readFixture(t, "testdata/anthropic.html"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 15 {
		t.Errorf("expected 15 data rows, got %d", len(rows))
	}
	opus := findRow(t, rows, "anthropic", "claude-opus-5")
	want := []struct {
		got  *big.Rat
		want int64
		name string
	}{
		{opus.input, 5_000_000, "input"},
		{opus.cacheWrite5m, 6_250_000, "5m cache write"},
		{opus.cacheWrite1h, 10_000_000, "1h cache write"},
		{opus.cachedRead, 500_000, "cached read"},
		{opus.output, 25_000_000, "output"},
	}
	for _, w := range want {
		if w.got.Cmp(big.NewRat(w.want, 1)) != 0 {
			t.Errorf("claude-opus-5 %s = %s, want %d", w.name, w.got, w.want)
		}
	}
	// Both cache-write columns must survive the capture (the committed
	// header literal spans them, and check 3 gates on them).
	if rows[0].cacheWrite5m == nil || rows[0].cacheWrite1h == nil {
		t.Fatal("cache-write columns missing from the parsed table")
	}
	// The parenthetical qualifiers must be stripped from the model name.
	for _, want := range []string{"claude-mythos-5", "claude-opus-4-1", "claude-haiku-3-5"} {
		findRow(t, rows, "anthropic", want)
	}
}

// TestParseOpenAIFixture parses the recorded capture of
// https://developers.openai.com/api/docs/models/compare (2026-08-28).
func TestParseOpenAIFixture(t *testing.T) {
	t.Parallel()
	rows, err := parseOpenAI(readFixture(t, "testdata/openai.html"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 models, got %d", len(rows))
	}
	sol := findRow(t, rows, "openai", "gpt-5-6-sol")
	if sol.input.Cmp(big.NewRat(4_000_000, 1)) != 0 {
		t.Errorf("gpt-5.6-sol input = %s, want $4.00/MTok", sol.input)
	}
	if sol.cachedRead.Cmp(big.NewRat(400_000, 1)) != 0 {
		t.Errorf("gpt-5.6-sol cached read = %s, want $0.40/MTok", sol.cachedRead)
	}
	if sol.output.Cmp(big.NewRat(20_000_000, 1)) != 0 {
		t.Errorf("gpt-5.6-sol output = %s, want $20.00/MTok", sol.output)
	}
	if sol.cacheWrite5m != nil || sol.cacheWrite1h != nil {
		t.Error("OpenAI publishes no cache-write rate; the page must not invent one")
	}
	// The comparison page prices by per-1M-token labels; gpt-5.6-luna is the
	// cheap tail and a good anchor for the label-sibling extraction.
	luna := findRow(t, rows, "openai", "gpt-5-6-luna")
	if luna.input.Cmp(big.NewRat(200_000, 1)) != 0 {
		t.Errorf("gpt-5.6-luna input = %s, want $0.20/MTok", luna.input)
	}
}

// TestParseBrokenFixtures drives every error path with the synthetic broken
// set: selection ambiguity, no selection, malformed rows, and a wrong JSON
// envelope.
func TestParseBrokenFixtures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		file     string
		parse    func([]byte) ([]row, error)
		wantKind error
	}{
		{"testdata/broken/anthropic-batch.html", parseAnthropic, errSelect},
		{"testdata/broken/anthropic-shifted.html", parseAnthropic, errSelect},
		{"testdata/broken/anthropic-duplicated.html", parseAnthropic, errRow},
		{"testdata/broken/anthropic-badcell.html", parseAnthropic, errRow},
		{"testdata/broken/openrouter-bad.json", parseOpenRouter, errShape},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			_, err := tt.parse(readFixture(t, tt.file))
			if !errors.Is(err, tt.wantKind) {
				t.Fatalf("got error %v, want kind %v", err, tt.wantKind)
			}
		})
	}
}

func TestCanonicalModel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"claude-opus-4.8", "claude-opus-4-8"},
		{"Claude Opus 5", "claude-opus-5"},
		{"Claude Mythos 5 (limited availability)", "claude-mythos-5-limited-availability"},
		{"openai/gpt-5.6-sol-pro", "openai-gpt-5-6-sol-pro"},
		{"gpt-5.6-sol", "gpt-5-6-sol"},
		{"claude-opus-5:batch", "claude-opus-5-batch"},
		{"GPT-5.6 Sol", "gpt-5-6-sol"},
		{"  padded  ", "padded"},
		{"", ""},
		{"---", ""},
		{"a__b", "a-b"},
	}
	for _, tt := range tests {
		if got := canonicalModel(tt.in); got != tt.want {
			t.Errorf("canonicalModel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStripParenthetical(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"Claude Mythos 5 (limited availability)", "Claude Mythos 5 "},
		{"Claude Opus 4.1 (retired, except on Bedrock and Google Cloud)", "Claude Opus 4.1 "},
		{"Claude Fable 5", "Claude Fable 5"},
		{"a (b (c)) d", "a  d"},
	}
	for _, tt := range tests {
		if got := stripParenthetical(tt.in); got != tt.want {
			t.Errorf("stripParenthetical(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSplitProviderID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, scheme, model string
		ok                bool
	}{
		{"openai/gpt-5.6-sol", "openai", "gpt-5.6-sol", true},
		{"anthropic/claude-opus-5", "anthropic", "claude-opus-5", true},
		{"no-slash-id", "", "", false},
	}
	for _, tt := range tests {
		scheme, model, ok := splitProviderID(tt.in)
		if scheme != tt.scheme || model != tt.model || ok != tt.ok {
			t.Errorf("splitProviderID(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.in, scheme, model, ok, tt.scheme, tt.model, tt.ok)
		}
	}
}

// TestFetchHTTP pins the fetch hygiene rules against a local server: status
// checks, the User-Agent, the response-size cap, and redirect policy.
func TestFetchHTTP(t *testing.T) {
	t.Parallel()
	// The handler runs in the server's goroutine; the request's User-Agent is
	// carried back over a channel so the test reads it with a happens-before,
	// not a shared variable.
	uaCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case uaCh <- r.Header.Get("User-Agent"):
		default: // first request only; later requests must never block on it
		}
		switch r.URL.Path {
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", maxFetchBytes+1)))
		case "/redirect-away":
			http.Redirect(w, r, "https://evil.example.com/steal", http.StatusFound)
		case "/fail":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	t.Cleanup(srv.Close)

	body, err := fetchHTTP(srv.URL + "/ok")
	if err != nil {
		t.Fatalf("GET ok: %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
	if ua := <-uaCh; ua != userAgent {
		t.Errorf("User-Agent = %q, want %q", ua, userAgent)
	}

	if _, err := fetchHTTP(srv.URL + "/fail"); err == nil {
		t.Error("non-200 must be an error")
	}
	if _, err := fetchHTTP(srv.URL + "/big"); err == nil {
		t.Error("oversized response must be an error")
	}
	if _, err := fetchHTTP(srv.URL + "/redirect-away"); err == nil {
		t.Error("cross-host redirect must be refused")
	}
}

func TestFetchHTTPRefusesUnknownHost(t *testing.T) {
	t.Parallel()
	// A URL on a host outside the allowlist is a request error before any
	// connection: CheckRedirect only covers redirects, and the initial host
	// is governed by the caller. fetchHTTP is called with source URLs only;
	// this test pins that a garbage URL fails rather than fetches.
	if _, err := fetchHTTP("http://example.invalid/"); err == nil {
		t.Error("unreachable host should error")
	}
}

func TestTrimHTMLAndMinifyJSON(t *testing.T) {
	t.Parallel()
	// The header must span the committed literal's full column set — the
	// parser selects by it, and both cache-write columns are load-bearing.
	htmlIn := `<html><head><title>x</title></head><body><script>var s="<table></table>";</script><table><thead><tr><th>Model</th><th>Base Input Tokens</th><th>5m Cache Writes</th><th>1h Cache Writes</th><th>Cache Hits &amp; Refreshes</th><th>Output Tokens</th></tr></thead><tbody><tr><td>Claude Opus 5</td><td>$5 / MTok</td><td>$6.25 / MTok</td><td>$10 / MTok</td><td>$0.50 / MTok</td><td>$25 / MTok</td></tr></tbody></table></body></html>`
	out, err := trimHTML([]byte(htmlIn))
	if err != nil {
		t.Fatalf("trimHTML: %v", err)
	}
	if strings.Contains(string(out), "script") || strings.Contains(string(out), "<head>") {
		t.Errorf("trimHTML kept script or head: %s", out)
	}
	if !strings.Contains(string(out), "Claude Opus 5") || !strings.Contains(string(out), "$5 / MTok") {
		t.Errorf("trimHTML dropped table content: %s", out)
	}
	rows, err := parseAnthropic(out)
	if err != nil {
		t.Fatalf("trimmed html must still parse: %v", err)
	}
	if len(rows) != 1 || rows[0].canonical != "claude-opus-5" {
		t.Errorf("parsed %d rows from trimmed html, want 1 opus-5", len(rows))
	}

	jsonIn := `{ "data" : [ { "id" : "openai/gpt-5.6-sol", "pricing" : { "prompt" : "0.000002", "completion" : "0.00002" } } ] }`
	mini, err := minifyJSON([]byte(jsonIn))
	if err != nil {
		t.Fatalf("minifyJSON: %v", err)
	}
	if strings.ContainsAny(string(mini), " \t\n") {
		t.Errorf("minified JSON still has whitespace: %s", mini)
	}
	if !strings.Contains(string(mini), `"prompt":"0.000002"`) {
		t.Errorf("minifier must preserve decimal strings byte-for-byte: %s", mini)
	}
	rows, err = parseOpenRouter(mini)
	if err != nil {
		t.Fatalf("minified JSON must still parse: %v", err)
	}
	if len(rows) != 1 || rows[0].input.Cmp(big.NewRat(2_000_000, 1)) != 0 {
		t.Errorf("parsed %d rows, input %s, want 1 row at $2.00/MTok", len(rows), rows[0].input)
	}
}

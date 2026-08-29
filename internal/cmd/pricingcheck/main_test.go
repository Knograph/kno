package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func runCfg(fixturesDir string) cfg {
	return cfg{
		maxAgeDays:   90,
		tableVersion: "2026-08-21",
		fixturesDir:  fixturesDir,
		out:          &bytes.Buffer{},
		errOut:       &bytes.Buffer{},
	}
}

func captureRun(c cfg) (int, string, string) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	c.out, c.errOut = out, errOut
	return run(c), out.String(), errOut.String()
}

// fixtureSet copies the good fixtures into a temp dir, optionally replacing
// one file with the broken variant.
func fixtureSet(t *testing.T, replace map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"openrouter.json", "anthropic.html", "openai.html", "bedrock.json"} {
		body, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		if alt, ok := replace[f]; ok {
			body, err = os.ReadFile(filepath.Join("testdata/broken", alt))
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, f), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// detailLine is the exact contract for a FAIL or REPORT line: the verdict
// word, "check N: ", the check name, and the detail.
var detailLine = regexp.MustCompile(`^(FAIL|REPORT) check [0-9]+: [^—]+— .+$`)

// TestRunFixturesClean runs against the recorded fixtures: every gated check
// passes, the pending exclusions are reported (never gated), and the output
// honors the line contract — only detail lines begin with FAIL or REPORT.
func TestRunFixturesClean(t *testing.T) {
	t.Parallel()
	exit, out, _ := captureRun(runCfg("testdata"))
	if exit != 0 {
		t.Fatalf("clean fixtures must exit 0, got %d\n%s", exit, out)
	}
	for _, want := range []string{
		"CHECK 0: table version — PASS",
		"CHECK 1: source agreement — PASS",
		"CHECK 2: table selection — PASS",
		"CHECK 3: cache ratios — PASS",
		"CHECK 4: discovery — PASS",
		"CHECK 5: prefix collisions — PASS",
		"CHECK 6: regional multiplier — REPORT",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if !strings.HasPrefix(out, advisoryHeader+"\n") {
		t.Errorf("text report must open with the advisory header\n%s", out)
	}
	reportLines := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "FAIL") || strings.HasPrefix(line, "REPORT") {
			if !detailLine.MatchString(line) {
				t.Errorf("detail line violates the contract: %q", line)
			}
			reportLines++
		}
		if line != advisoryHeader && !strings.HasPrefix(line, "FAIL") && !strings.HasPrefix(line, "REPORT") &&
			!strings.HasPrefix(line, "CHECK ") {
			t.Errorf("line begins with a forbidden word or shape: %q", line)
		}
	}
	// docs/debt.md#46 repaid: no pending exclusions remain, so the only
	// report-only finding a clean run carries is check 6's standing vertex
	// line — a scheme whose regional multiplier nothing live-guards.
	if reportLines != 1 {
		t.Errorf("expected 1 report detail line (the vertex standing line), got %d\n%s", reportLines, out)
	}
}

func TestRunExit1BrokenAnthropic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		file  string
		want  string
		check string
	}{
		{"batch", "anthropic-batch.html", "2 tables match the committed header literal", "check 2: table selection"},
		{"shifted", "anthropic-shifted.html", "no table has the committed header", "check 2: table selection"},
		{"duplicated", "anthropic-duplicated.html", "price row malformed", "check 3: cache ratios"},
		{"badcell", "anthropic-badcell.html", "price row malformed", "check 3: cache ratios"},
		{"fast missing", "anthropic-fast-missing.html", "no table has the committed fast-mode header", "check 2: table selection"},
		{"fast duplicated", "anthropic-fast-duplicated.html", "2 tables match the committed fast-mode header", "check 2: table selection"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := fixtureSet(t, map[string]string{"anthropic.html": tt.file})
			exit, out, _ := captureRun(runCfg(dir))
			if exit != 1 {
				t.Fatalf("broken fixture must exit 1, got %d\n%s", exit, out)
			}
			if !strings.Contains(out, "FAIL "+tt.check+" — ") || !strings.Contains(out, tt.want) {
				t.Errorf("missing FAIL line with %q\n%s", tt.want, out)
			}
		})
	}
}

func TestRunExit2UnusableFixture(t *testing.T) {
	t.Parallel()
	dir := fixtureSet(t, map[string]string{"openrouter.json": "openrouter-bad.json"})
	exit, _, errOut := captureRun(runCfg(dir))
	if exit != 2 {
		t.Fatalf("wrong envelope must exit 2, got %d", exit)
	}
	if !strings.Contains(errOut, "fixture for openrouter is unusable") {
		t.Errorf("stderr must say which fixture failed: %s", errOut)
	}
}

func TestRunJSONShape(t *testing.T) {
	t.Parallel()
	c := runCfg("testdata")
	c.jsonOut = true
	exit, out, _ := captureRun(c)
	if exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	var rep reportOut
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("JSON report does not parse: %v\n%s", err, out)
	}
	if rep.Schema != "pricingcheck/report/v1" {
		t.Errorf("schema = %q", rep.Schema)
	}
	if rep.TableVersion != "2026-08-21" || rep.MaxAgeDays != 90 {
		t.Errorf("table_version/max_age_days = %s/%d", rep.TableVersion, rep.MaxAgeDays)
	}
	for _, s := range []string{"anthropic", "openai", "openrouter", "bedrock"} {
		if rep.Sources[s] != "ok" {
			t.Errorf("sources[%s] = %q, want ok", s, rep.Sources[s])
		}
	}
	if len(rep.Checks) != 7 {
		t.Errorf("got %d checks, want 7", len(rep.Checks))
	}
	if len(rep.FailedGated) != 0 {
		t.Errorf("failed_gated = %v, want none", rep.FailedGated)
	}
	// The one standing report finding is check 6's vertex line: a scheme
	// whose regional multiplier no machine-readable source guards.
	if len(rep.Reported) != 1 {
		t.Errorf("reported = %d findings, want 1 (the standing vertex line)", len(rep.Reported))
	}
}

func TestRunConfigErrors(t *testing.T) {
	t.Parallel()
	c := runCfg("testdata")
	c.tableVersion = "aug-21"
	if exit, _, errOut := captureRun(c); exit != 2 || !strings.Contains(errOut, "not a YYYY-MM-DD") {
		t.Errorf("bad table version: exit %d, stderr %s", exit, errOut)
	}
	c = runCfg("testdata")
	c.maxAgeDays = 0
	if exit, _, errOut := captureRun(c); exit != 2 || !strings.Contains(errOut, "--max-age-days must be at least 1") {
		t.Errorf("bad max age: exit %d, stderr %s", exit, errOut)
	}
}

// TestRunStaleTableGates uses an injected old table version: check 0 must
// fail and exit 1.
func TestRunStaleTableGates(t *testing.T) {
	t.Parallel()
	c := runCfg("testdata")
	c.tableVersion = "2024-01-01"
	exit, out, _ := captureRun(c)
	if exit != 1 {
		t.Fatalf("stale table must exit 1, got %d", exit)
	}
	if !strings.Contains(out, "FAIL check 0: table version — ") {
		t.Errorf("missing FAIL check 0 line\n%s", out)
	}
}

// TestRunLiveFailSoft stubs the network to fail: live sources become "no
// data" reports and never gate.
func TestRunLiveFailSoft(t *testing.T) {
	orig := fetch
	fetch = func(_ string) ([]byte, error) { return nil, errors.New("network down") }
	defer func() { fetch = orig }()

	c := runCfg("testdata")
	c.live = true
	exit, out, _ := captureRun(c)
	if exit != 0 {
		t.Fatalf("unreachable live sources must fail soft (exit 0), got %d\n%s", exit, out)
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("no FAIL lines allowed on unreachable sources\n%s", out)
	}
	for _, want := range []string{
		"openrouter has no data; agreement not checked",
		"anthropic has no data; table selection not checked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing fail-soft line %q\n%s", want, out)
		}
	}
}

// TestRunRecord stubs the network with canned bodies and records a fixture
// set, then verifies the fixtures parse and note.txt carries the provenance.
func TestRunRecord(t *testing.T) {
	orig := fetch
	fetch = func(url string) ([]byte, error) {
		if strings.Contains(url, "openrouter") {
			return []byte(`{ "data" : [ { "id" : "openai/gpt-5.6-sol", "pricing" : { "prompt" : "0.000002", "completion" : "0.00002" } } ] }`), nil
		}
		if strings.Contains(url, "platform.claude.com") {
			return []byte(`<html><body><script>var x=1;</script><table><thead><tr><th>Model</th><th>Base Input Tokens</th><th>5m Cache Writes</th><th>1h Cache Writes</th><th>Cache Hits &amp; Refreshes</th><th>Output Tokens</th></tr></thead><tbody><tr><td>Claude Opus 5</td><td>$5 / MTok</td><td>$6.25 / MTok</td><td>$10 / MTok</td><td>$0.50 / MTok</td><td>$25 / MTok</td></tr></tbody></table><table><thead><tr><th>Model</th><th>Input</th><th>Output</th></tr></thead><tbody><tr><td>Claude Opus 5 / Claude Opus 4.8</td><td>$10 / MTok</td><td>$50 / MTok</td></tr></tbody></table></body></html>`), nil
		}
		if strings.Contains(url, "amazonaws.com") {
			return []byte(`{"products":{"sku-a":{"attributes":{"region":"us-east-1","locationType":"AWS Region","usagetype":"AnthropicClaude-Sonnet45InputTokens"}}},"terms":{"OnDemand":{"sku-a":{"code-a":{"priceDimensions":{"code-a":{"pricePerUnit":{"USD":"0.0030000000"}}}}}}}}`), nil
		}
		// The header row and the section content are siblings inside one
		// wrapper; the label rows are the content container's direct children.
		return []byte(`<html><body><a href="/api/docs/models/gpt-5.6-sol"></a><div><div><div>Pricing</div><div>Per 1M tokens</div></div><div><div><div>Input</div><div>$4.00</div></div><div><div>Cached Input</div><div>$0.40</div></div><div><div>Output</div><div>$20.00</div></div></div></div></body></html>`), nil
	}
	defer func() { fetch = orig }()

	c := runCfg("testdata")
	c.record = true
	c.fixturesDir = t.TempDir()
	exit, _, errOut := captureRun(c)
	if exit != 0 {
		t.Fatalf("record must exit 0, got %d: %s", exit, errOut)
	}
	for _, f := range []string{"openrouter.json", "anthropic.html", "openai.html", "bedrock.json", "note.txt"} {
		if _, err := os.Stat(filepath.Join(c.fixturesDir, f)); err != nil {
			t.Errorf("fixture %s not written: %v", f, err)
		}
	}
	note, err := os.ReadFile(filepath.Join(c.fixturesDir, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	noteStr := string(note)
	for _, want := range []string{
		"https://openrouter.ai/api/v1/models",
		"https://platform.claude.com/docs/en/about-claude/pricing",
		"https://developers.openai.com/api/docs/models/compare",
	} {
		if !strings.Contains(noteStr, want) {
			t.Errorf("note.txt missing %q", want)
		}
	}
	// sha256 hex of the untrimmed captures: 64 hex chars per file.
	if got := regexp.MustCompile(`sha256: [0-9a-f]{64}`).FindAllString(noteStr, -1); len(got) != 4 {
		t.Errorf("note.txt must carry 4 sha256 digests, got %v", got)
	}
	// The recorded fixtures must parse.
	if _, err := parseOpenRouter(readFixture(t, filepath.Join(c.fixturesDir, "openrouter.json"))); err != nil {
		t.Errorf("recorded openrouter fixture: %v", err)
	}
	if _, err := parseAnthropic(readFixture(t, filepath.Join(c.fixturesDir, "anthropic.html"))); err != nil {
		t.Errorf("recorded anthropic fixture: %v", err)
	}
	if _, err := parseOpenAI(readFixture(t, filepath.Join(c.fixturesDir, "openai.html"))); err != nil {
		t.Errorf("recorded openai fixture: %v", err)
	}
}

func TestRunRecordFetchFailureLoud(t *testing.T) {
	orig := fetch
	fetch = func(_ string) ([]byte, error) { return nil, errors.New("boom") }
	defer func() { fetch = orig }()

	c := runCfg("testdata")
	c.record = true
	c.fixturesDir = t.TempDir()
	exit, _, errOut := captureRun(c)
	if exit != 2 {
		t.Fatalf("failed record must exit 2, got %d", exit)
	}
	if !strings.Contains(errOut, "recording openrouter") || !strings.Contains(errOut, "boom") {
		t.Errorf("fetch failure must be reported loudly: %s", errOut)
	}
}

func TestParseFlagsEnvArming(t *testing.T) {
	t.Setenv("KNO_LIVE_TESTS", "")
	t.Setenv("KNO_RECORD_FIXTURES", "")
	_, errOut := &bytes.Buffer{}, &bytes.Buffer{}

	c, err := parseFlags([]string{"--live", "--record"}, &bytes.Buffer{}, errOut)
	if err != nil {
		t.Fatal(err)
	}
	if c.live || c.record {
		t.Error("flags without their env vars must be disarmed")
	}
	if !strings.Contains(errOut.String(), "KNO_LIVE_TESTS=1") || !strings.Contains(errOut.String(), "KNO_RECORD_FIXTURES=1") {
		t.Errorf("disarmed flags must say why on stderr: %s", errOut.String())
	}

	t.Setenv("KNO_LIVE_TESTS", "1")
	t.Setenv("KNO_RECORD_FIXTURES", "1")
	c, err = parseFlags([]string{"--live"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !c.live || c.record {
		t.Error("env-armed --live must enable live mode")
	}

	if _, err := parseFlags([]string{"--nonsense"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("unknown flag must error")
	}
	if _, err := parseFlags([]string{"stray-arg"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("positional args must error")
	}
}

// TestParseFlagsLiveArming pins the --live arming contract at the
// parseFlags level: the Makefile and the drift workflow invoke --live, and
// KNO_LIVE_TESTS=1 is the only thing between a CI run and a developer's
// machine hitting the live pages. An unset env var (Getenv "" is unset for
// this purpose) must leave live false; the env set to 1 must leave it true.
// Not parallel: t.Setenv forbids it.
func TestParseFlagsLiveArming(t *testing.T) {
	t.Run("unset env disarms", func(t *testing.T) {
		t.Setenv("KNO_LIVE_TESTS", "")
		c, err := parseFlags([]string{"--live"}, &bytes.Buffer{}, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		if c.live {
			t.Error("--live without KNO_LIVE_TESTS=1 must leave live false")
		}
	})
	t.Run("armed env enables", func(t *testing.T) {
		t.Setenv("KNO_LIVE_TESTS", "1")
		c, err := parseFlags([]string{"--live"}, &bytes.Buffer{}, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		if !c.live {
			t.Error("--live with KNO_LIVE_TESTS=1 must leave live true")
		}
	})
}

func TestDefaultFixturesDir(t *testing.T) {
	t.Parallel()
	if !strings.HasSuffix(filepath.Clean(defaultFixturesDir()), filepath.Join("internal", "cmd", "pricingcheck", "testdata")) {
		t.Errorf("default fixtures dir = %q", defaultFixturesDir())
	}
}

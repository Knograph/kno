// Command pricingcheck is the price-drift detector for the committed pricing
// table in adapters/agent/pricing. It compares that table — its version, its
// coverage, and its namespace — against the price-of-record pages, so a
// stale table, a restructured page, or a variant that carries its own price
// is a dated, reviewable signal instead of a silent drift.
//
// Sources (see source.go): OpenRouter's model list, Anthropic's pricing
// page, and OpenAI's model comparison page. Offline by default, reading
// recorded fixtures from the --fixtures directory; --live fetches the
// sources (armed by KNO_LIVE_TESTS=1); --record captures and trims the live
// sources into the fixtures directory (armed by KNO_RECORD_FIXTURES=1). A
// flag given without its env var is ignored with a note on stderr.
//
// Checks:
//
//	0  gate   table version parses as YYYY-MM-DD and is within --max-age-days
//	1  report OpenRouter against the price-of-record pages (gpt-5.6-sol
//	          suppressed; a dead suppression fails)
//	2  gate   the anthropic price table is selected by the committed header
//	          literal, and by exactly one table
//	3  gate   cache read is 0.10x input (both providers); anthropic cache
//	          writes are 1.25x (5m) and 2.00x (1h) of input
//	4  report models on the price-of-record pages the table does not price
//	5  gate+  prefix-resolving variants Lookup refuses; committed pending
//	          exclusions (docs/debt.md#46) are reported every run; an
//	          exclusion whose model is now priced fails
//	6  gate   the regional +10% multiplier (docs/debt.md#41(d)) is confirmed
//	          by AWS's Bedrock price list; vertex has no machine-readable
//	          source and is reported every run
//
// Output contract. The text report is one CHECK summary line per check, plus
// one detail line per finding. Detail lines begin with exactly one of two
// words, and nothing else — not the summary lines, not notes — begins with
// either:
//
//	FAIL check 2: table selection — <detail>        (gated; exit 1)
//	REPORT check 5: prefix collisions — <detail>    (report-only; exit 0)
//
// The word is the verdict: FAIL findings are the gated failures, REPORT
// findings are the report-only surface. Consumers dedupe on the first
// FAIL-or-REPORT line with digit runs collapsed, so the identity of a line
// (check number, check name, model name) precedes its digit-bearing detail.
// --json emits the same content as schema "pricingcheck/report/v1" with a
// failed_gated/reported split of findings.
//
// Exit codes: 0 every gated check passed; 1 at least one gated check failed
// (report-only findings never affect the exit code); 2 internal error — bad
// configuration, or a fixture that is unusable as data (wrong envelope).
// Live sources that fail to fetch fail SOFT: they are reported as "no data"
// and never gate, so a flaky network cannot page anyone.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/knograph/kno/adapters/agent/pricing"
)

// cfg is the parsed command line.
type cfg struct {
	live         bool
	record       bool
	maxAgeDays   int
	tableVersion string
	fixturesDir  string
	jsonOut      bool
	out          io.Writer
	errOut       io.Writer
}

func main() {
	c, err := parseFlags(os.Args[1:], os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pricingcheck: %v\n", err)
		os.Exit(2)
	}
	os.Exit(run(c))
}

// parseFlags builds the config. The flags exist so humans can read the help
// and CI can drive the run; live and record are ARMED by their env vars, so
// a developer's local run can never accidentally hit the live pages.
func parseFlags(args []string, out, errOut io.Writer) (cfg, error) {
	fs := flag.NewFlagSet("pricingcheck", flag.ContinueOnError)
	fs.SetOutput(errOut)
	live := fs.Bool("live", false, "fetch the price-of-record sources live (armed by KNO_LIVE_TESTS=1)")
	record := fs.Bool("record", false, "capture and trim the live sources into --fixtures (armed by KNO_RECORD_FIXTURES=1)")
	maxAgeDays := fs.Int("max-age-days", 90, "fail check 0 when the table version is older than this many days")
	tableVersion := fs.String("table-version", pricing.Version, "table version date (YYYY-MM-DD) to check")
	fixturesDir := fs.String("fixtures", defaultFixturesDir(), "directory holding the recorded source fixtures")
	jsonOut := fs.Bool("json", false, "emit the report as JSON (schema pricingcheck/report/v1)")
	if err := fs.Parse(args); err != nil {
		return cfg{}, err
	}
	if fs.NArg() != 0 {
		return cfg{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	c := cfg{
		live:         *live,
		record:       *record,
		maxAgeDays:   *maxAgeDays,
		tableVersion: *tableVersion,
		fixturesDir:  *fixturesDir,
		jsonOut:      *jsonOut,
		out:          out,
		errOut:       errOut,
	}
	if c.live && os.Getenv("KNO_LIVE_TESTS") != "1" {
		_, _ = fmt.Fprintln(errOut, "pricingcheck: --live requires KNO_LIVE_TESTS=1; running offline")
		c.live = false
	}
	if c.record && os.Getenv("KNO_RECORD_FIXTURES") != "1" {
		_, _ = fmt.Fprintln(errOut, "pricingcheck: --record requires KNO_RECORD_FIXTURES=1; ignoring")
		c.record = false
	}
	return c, nil
}

// run executes the checks and returns the exit code.
func run(c cfg) int {
	if _, err := time.Parse("2006-01-02", c.tableVersion); err != nil {
		_, _ = fmt.Fprintf(c.errOut, "pricingcheck: table version %q is not a YYYY-MM-DD date: %v\n", c.tableVersion, err)
		return 2
	}
	if c.maxAgeDays < 1 {
		_, _ = fmt.Fprintln(c.errOut, "pricingcheck: --max-age-days must be at least 1")
		return 2
	}
	if c.record {
		if err := recordFixtures(c.fixturesDir); err != nil {
			_, _ = fmt.Fprintf(c.errOut, "pricingcheck: recording fixtures: %v\n", err)
			return 2
		}
		_, _ = fmt.Fprintf(c.errOut, "pricingcheck: fixtures recorded to %s (sha256 in note.txt)\n", c.fixturesDir)
		return 0
	}
	sourceMap := loadSources(c)
	if !c.live {
		// A fixture that is unusable as data is an internal error, not a
		// check outcome: it means the fixture set is broken, and checking
		// against it would report nothing useful.
		for name, sd := range sourceMap {
			if sd.shapeBroken() {
				_, _ = fmt.Fprintf(c.errOut, "pricingcheck: fixture for %s is unusable: %v\n", name, sd.err)
				return 2
			}
		}
	}
	in := checkInput{
		sources:      sourceMap,
		regionPrices: regionPricesOf(sourceMap),
		tableVersion: c.tableVersion,
		maxAgeDays:   c.maxAgeDays,
	}
	results := []checkResult{
		checkTableVersion(in),
		checkAgreement(in),
		checkSelection(in),
		checkRatios(in),
		checkDiscovery(in),
		checkPrefixCollisions(in),
		checkRegionalMultiplier(in),
	}
	rep := buildReport(results, sourceMap, in)
	var err error
	if c.jsonOut {
		err = renderJSON(c.out, rep)
	} else {
		err = renderText(c.out, rep)
	}
	if err != nil {
		_, _ = fmt.Fprintf(c.errOut, "pricingcheck: writing report: %v\n", err)
		return 2
	}
	for _, cr := range results {
		if cr.Verdict == verdictFail {
			return 1
		}
	}
	return 0
}

// loadSources reads every source, live or from fixtures.
func loadSources(c cfg) map[string]sourceData {
	out := make(map[string]sourceData, len(sources))
	for _, s := range sources {
		var body []byte
		var err error
		if c.live {
			body, err = fetch(s.url)
		} else {
			body, err = os.ReadFile(filepath.Join(c.fixturesDir, s.file))
		}
		if err != nil {
			out[s.name] = sourceData{name: s.name, err: fmt.Errorf("%w: %v", errShape, err)}
			continue
		}
		rows, regions, perr := parseSource(s.name, body)
		if perr != nil {
			out[s.name] = sourceData{name: s.name, err: perr}
			continue
		}
		out[s.name] = sourceData{name: s.name, rows: rows, regions: regions}
	}
	return out
}

// parseSource dispatches a fixture body to its parser. The bedrock source
// parses to regional prices instead of rows — its checks read the spread
// across regions, not the per-model table.
func parseSource(name string, body []byte) ([]row, bedrockRegions, error) {
	switch name {
	case "openrouter":
		rows, err := parseOpenRouter(body)
		return rows, nil, err
	case "anthropic":
		rows, err := parseAnthropic(body)
		return rows, nil, err
	case "openai":
		rows, err := parseOpenAI(body)
		return rows, nil, err
	case "bedrock":
		regions, err := parseBedrock(body)
		return nil, regions, err
	}
	return nil, nil, fmt.Errorf("%w: unknown source %q", errShape, name)
}

// defaultFixturesDir is the package's own testdata directory, resolved from
// this file's path so the binary works from any working directory.
func defaultFixturesDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "testdata"
	}
	return filepath.Join(filepath.Dir(file), "testdata")
}

//go:build integration

package openaicompat_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/adapters/agent/openaicompat"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// Everything in this file spends real money, and nothing in it runs unless two
// separate switches are thrown: the `integration` build tag, and KNO_LIVE_TESTS
// in the environment. `make test` and `make test-integration` reach neither.

// Environment this path reads.
const (
	// envLiveTests opts in to calling a real provider at all.
	envLiveTests = "KNO_LIVE_TESTS"

	// envMaxCostUSD is the ceiling every live call is authorized against.
	//
	// It is READ HERE, by code, and turned into a budget.Guard that refuses the
	// call that would breach it. That sentence is the whole of docs/debt.md#11:
	// the nightly job was once armed with real credentials and a cap comment
	// asserting a protection that did not exist, and the Makefile compensated
	// with a grep for this string in any .go file — a check that a comment
	// satisfied. The grep is deleted in the same change that adds this, because
	// a proxy for enforcement that outlives the real thing is worse than no
	// check: it reads as green.
	envMaxCostUSD = "KNO_MAX_COST_USD"

	// envRecord asks the live path to rewrite testdata/fixtures.
	envRecord = "KNO_RECORD_FIXTURES"

	// envModel and envBaseURL point the live path somewhere other than the
	// default, for recording against a compatible provider.
	envModel   = "KNO_LIVE_MODEL"
	envBaseURL = "KNO_LIVE_BASE_URL"
)

// requireLive skips unless the live switch is set to exactly "1".
//
// "1", not "not empty". This gate used to test `== ""`, so KNO_LIVE_TESTS=0 —
// which every reader would take for "off", and which a shell writes when it
// exports a false boolean — opted IN and spent money. Every sibling gate in the
// repo (cli/main_test.go, anthropic/record_test.go) tests `!= "1"`, and a switch
// that means the opposite of what its value says in one place out of three is
// the shape of docs/debt.md#63: a guard that keeps passing while it stops
// guarding.
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv(envLiveTests) != "1" {
		t.Skipf("%s is not 1; this test calls a real provider and spends real money",
			envLiveTests)
	}
}

// liveGuard builds the budget guard every live call is authorized against.
//
// A guard rather than a bare comparison, because the guard is what the engine
// itself uses: it denies at the boundary rather than past it, it accounts for
// concurrent reservations, and it is the one implementation of "never spend the
// user's money silently" in this repository. A second, simpler ceiling written
// for tests would be a second thing to be wrong.
//
// Fatal rather than skip when the variable is missing. A skip would let a
// misconfigured nightly job report green having tested nothing, which is the
// same shape of silence entry 11 records.
func liveGuard(t *testing.T) *budget.Guard {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv(envMaxCostUSD))
	if raw == "" {
		t.Fatalf("%s is not set. A live run without a stated ceiling is exactly the "+
			"silent-overspend trap docs/debt.md#11 exists for; set it to the most "+
			"this run may spend, in dollars", envMaxCostUSD)
	}
	dollars, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("%s=%q is not a number of dollars: %v", envMaxCostUSD, raw, err)
	}
	if dollars <= 0 {
		t.Fatalf("%s=%q is not a spendable ceiling", envMaxCostUSD, raw)
	}
	micros := int64(dollars * 1_000_000)
	if micros <= 0 {
		t.Fatalf("%s=%q rounds to nothing at micro-USD resolution", envMaxCostUSD, raw)
	}

	t.Logf("live spend ceiling: $%.4f", float64(micros)/1_000_000)
	// No ConfirmFunc and a zero threshold: a nightly job has nobody to ask, and
	// the ceiling is the consent. Guard.Authorize denies rather than prompting.
	return budget.New(budget.Limits{MaxCostUSDMicros: micros}, nil, 0)
}

// spendOne runs one Case through the guard, exactly as core does.
//
// Estimate, then Authorize, then Invoke, then Settle at what the Response says
// it cost — the same four steps in the same order as core.invokeOnce, so a live
// test cannot spend on a path the engine would have refused.
func spendOne(t *testing.T, g *budget.Guard, a *openaicompat.Agent, c *core.Case) *knov1.Response {
	t.Helper()

	est, err := a.Estimate(t.Context(), c)
	if err != nil {
		t.Fatalf("Estimate: %v. An unpriced model under a ceiling is a refusal, "+
			"not a guess", err)
	}
	res, err := g.Authorize(t.Context(), est)
	if err != nil {
		t.Fatalf("the spend ceiling refused this Case: %v. Raise %s or record "+
			"fewer fixtures; nothing was spent", err, envMaxCostUSD)
	}
	defer res.Release()

	resp, err := a.Invoke(t.Context(), c)
	if err != nil {
		// The attempt consumed a call whether or not it answered — and it may
		// have consumed money too. When the provider reported a usage block
		// alongside its failure the adapter carries the charge on the error, so
		// settle THAT rather than zero: a paid call recorded as free is spend
		// the cap cannot see, and a resume restores the understated figure and
		// spends the difference again. See docs/debt.md#43.
		//
		// core cannot do this yet — invokeOnce settles every Invoke error as
		// Spend{Calls: 1} with no dollars — so this path is currently the only
		// place the observation is acted on.
		spend := budget.Spend{Calls: 1}
		if micros, ok := openaicompat.BilledCostOf(err); ok {
			spend.CostUSDMicros = micros
			t.Logf("the provider billed $%.6f for a call that failed",
				float64(micros)/1_000_000)
		}
		res.Settle(spend)
		t.Fatalf("Invoke: %v", err)
	}
	res.Settle(budget.Spend{
		Calls:         1,
		CostUSDMicros: resp.GetCostUsdMicros(),
		Tokens:        resp.GetPromptTokens() + resp.GetCompletionTokens(),
	})

	// docs/debt.md#36 asks the nightly job to report when the local token count
	// UNDER-estimates the provider's own. Under-counting is the direction that
	// breaks a cap — "a bound that can be too low is not a bound" — and it is
	// invisible without a live provider to compare against, because the estimate
	// and the fixtures were built from the same approximation.
	//
	// Reported rather than failed: one Case is a sample, and a nightly job that
	// goes red on a single long prompt gets muted. The entry's trigger is "any
	// call whose input count we under-estimated", which is a thing to look at,
	// not a build break.
	if reported := resp.GetPromptTokens(); reported > 0 && !resp.GetUsageEstimated() {
		// est.Tokens is input plus the output ceiling; the input term alone is
		// what compares against prompt_tokens.
		estimated := est.Tokens - a.OutputCeiling()
		switch {
		case estimated < reported:
			t.Errorf("UNDER-ESTIMATED the prompt: %d tokens estimated against %d "+
				"reported. This is the direction that walks a run past its cap; "+
				"docs/debt.md#36 names exactly this as its repayment trigger",
				estimated, reported)
		default:
			t.Logf("prompt tokens: estimated %d, reported %d (%.1fx over)",
				estimated, reported, float64(estimated)/float64(reported))
		}
	}
	return resp
}

// liveAgent builds an Agent aimed at a real provider, plus a recorder.
func liveAgent(t *testing.T) (*openaicompat.Agent, *recorder) {
	t.Helper()

	model := os.Getenv(envModel)
	if model == "" {
		model = pricedModel
	}
	ref := "openai:" + model
	if base := os.Getenv(envBaseURL); base != "" {
		ref += "@" + base
	}
	parsed, err := agentref.Parse(ref)
	if err != nil {
		t.Fatalf("parsing %q: %v", ref, err)
	}

	// The recorder replaces the transport's connection pool, which means the
	// DIAL-TIME address check does not run on this path — it is installed only
	// when the supplied transport is an *http.Transport. Narrow and deliberate:
	// the configuration-time address check, the redirect refusal, and the
	// host-bound credential all still apply, and this path runs only against a
	// host the operator named on purpose.
	rec := &recorder{next: http.DefaultTransport}

	a, err := openaicompat.New(openaicompat.Options{
		Ref:        parsed,
		HTTPClient: &http.Client{Transport: rec},
		Timeout:    60 * time.Second,
		UserAgent:  "kno-fixture-recorder",
		// A short ceiling: fixtures are shape, not prose, and every output
		// token is money.
		MaxOutputTokens: 64,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, rec
}

// recorder keeps the last reply, so a fixture records what actually came back
// rather than what this test believes came back.
type recorder struct {
	next http.RoundTripper

	status  int
	headers http.Header
	body    []byte
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.next.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	body, readErr := readAll(resp)
	if readErr != nil {
		return nil, readErr
	}
	r.status, r.headers, r.body = resp.StatusCode, resp.Header.Clone(), body
	return resp, nil
}

// maxRecordedBytes bounds what a recording will hold in memory and commit.
//
// A fixture is a small JSON reply; anything past this is a provider doing
// something a fixture should not immortalize.
const maxRecordedBytes = 1 << 20

// readAll drains a response body and puts it back, so the recording is the
// bytes the adapter itself will parse rather than a second fetch that could
// differ.
func readAll(resp *http.Response) ([]byte, error) {
	if resp.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxRecordedBytes))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return b, nil
}

// TestLiveInvokeStaysInsideItsCeiling is the smoke test the nightly job runs.
//
// It proves the two things a fixture cannot: that the request shape is one a
// real provider accepts, and that the ceiling in the environment is enforced by
// something rather than asserted by something.
func TestLiveInvokeStaysInsideItsCeiling(t *testing.T) {
	requireLive(t)

	g := liveGuard(t)
	a, _ := liveAgent(t)

	resp := spendOne(t, g, a, &core.Case{
		Id:    "live-smoke",
		Input: "Reply with the single word: ok",
		Split: knov1.Split_SPLIT_DEV,
	})

	if resp.GetResolvedModel() == "" {
		t.Error("no resolved_model. A resume compares it, and an alias that " +
			"re-points mid-run would blend two models into one aggregate unnoticed")
	}
	if resp.GetPromptTokens() == 0 && !resp.GetUsageEstimated() {
		t.Error("no tokens reported and the cost is not marked estimated, so a " +
			"report would add a zero into its total as if the call were free")
	}
	if resp.GetCostUsdMicros() <= 0 {
		t.Error("cost_usd_micros is zero, which is what makes a dollar cap " +
			"unenforceable")
	}

	spent := g.Spent()
	t.Logf("live spend: $%.6f over %d call(s), %d tokens",
		float64(spent.CostUSDMicros)/1_000_000, spent.Calls, spent.Tokens)
	if spent.CostUSDMicros > g.Limits().MaxCostUSDMicros {
		t.Errorf("spent %d micro-USD against a ceiling of %d",
			spent.CostUSDMicros, g.Limits().MaxCostUSDMicros)
	}
}

// TestRecord rewrites testdata/fixtures from live replies.
//
// This is the target `make record-fixtures` discovers. It spends real money,
// against the same guard as every other live path, over the synthetic corpus in
// testdata/corpus.txt — never a user's evals, because a fixture is committed to
// this repository forever and CLAUDE.md is explicit that traces are customer
// data.
//
// It records only what testdata/README.md's allowlist permits, and it refuses
// to WRITE a fixture that trips the same scan the test suite applies. A
// recorder that can produce a file the scanner rejects is a recorder that will
// eventually produce one nobody scanned.
func TestRecord(t *testing.T) {
	requireLive(t)
	if os.Getenv(envRecord) == "" {
		t.Skipf("%s is not set; recording overwrites checked-in fixtures", envRecord)
	}

	g := liveGuard(t)
	a, rec := liveAgent(t)

	for i, input := range corpus(t) {
		name := fmt.Sprintf("recorded-%02d", i)
		t.Run(name, func(t *testing.T) {
			spendOne(t, g, a, &core.Case{
				Id:    name,
				Input: input,
				Split: knov1.Split_SPLIT_DEV,
			})
			writeFixture(t, name, rec)
		})
	}

	spent := g.Spent()
	t.Logf("recording spent $%.6f over %d call(s)",
		float64(spent.CostUSDMicros)/1_000_000, spent.Calls)
}

// corpus reads the synthetic Case inputs.
func corpus(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", "corpus.txt")) //nolint:gosec // its own testdata
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		t.Fatal("the corpus is empty")
	}
	return out
}

// writeFixture emits one fixture in the allowlist format.
func writeFixture(t *testing.T, name string, rec *recorder) {
	t.Helper()

	var b strings.Builder
	b.WriteString("# kno adapter fixture v1\n")
	b.WriteString("name: " + name + "\n")
	b.WriteString("source: recorded by TestRecord against a live provider\n")
	b.WriteString("status: " + strconv.Itoa(rec.status) + "\n")
	// The response-header allowlist, spelled out. Everything else the provider
	// sent — organization and project identifiers, request IDs, cookies — is
	// dropped by never being considered.
	for _, h := range []string{"Content-Type", "Retry-After"} {
		if v := rec.headers.Get(h); v != "" {
			b.WriteString(strings.ToLower(h) + ": " + v + "\n")
		}
	}
	b.WriteString("\n")
	b.Write(rec.body)
	if len(rec.body) == 0 || rec.body[len(rec.body)-1] != '\n' {
		b.WriteString("\n")
	}

	out := b.String()
	lower := strings.ToLower(out)
	for _, bad := range forbiddenInFixtures {
		if strings.Contains(lower, bad) {
			t.Fatalf("refusing to write %s: the reply contains %q. A fixture is "+
				"committed forever, so this is a refusal rather than a scrub — a "+
				"scrub would hide that the provider echoed it back at all", name, bad)
		}
	}

	path := filepath.Join(fixtureDir, name+".fixture")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	t.Logf("wrote %s", path)
}

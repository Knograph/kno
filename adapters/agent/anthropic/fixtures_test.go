package anthropic_test

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/anthropic"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// fixtureDir is where recorded exchanges live.
const fixtureDir = "testdata/fixtures"

// fixtureFiles is the ALLOWLIST — the complete set of files a fixture may
// contain.
//
// An allowlist rather than a denylist of header names. A denylist can only
// remove what someone anticipated: gitleaks matches key SHAPES, not an
// organization ID, a project ID, or a session cookie, and the failure is silent
// and permanent because the fixture is committed forever. So no headers are
// recorded at all, in either direction, and anything outside this set fails the
// scan rather than being scrubbed.
var fixtureFiles = map[string]bool{
	"case.txt":      true,
	"request.json":  true,
	"response.json": true,
	"status":        true,
	"note.txt":      true,
}

// fixture is one recorded exchange.
type fixture struct {
	name     string
	input    string
	request  string
	response string
	status   int
}

// evalCase is the synthetic Case this exchange was recorded against.
//
// Synthetic and checked in beside the fixture. A user's evals are customer
// data; recording against them would commit customer data to the repository
// forever.
func (f fixture) evalCase() *knov1.Case {
	return &knov1.Case{Id: f.name, Input: f.input}
}

// loadFixtures reads every recorded exchange.
func loadFixtures(t *testing.T) []fixture {
	t.Helper()

	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("reading %s: %v", fixtureDir, err)
	}

	var out []fixture
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(fixtureDir, e.Name())
		status, err := strconv.Atoi(strings.TrimSpace(read(t, dir, "status")))
		if err != nil {
			t.Fatalf("%s: status is not a number: %v", e.Name(), err)
		}
		out = append(out, fixture{
			name:     e.Name(),
			input:    strings.TrimRight(read(t, dir, "case.txt"), "\n"),
			request:  strings.TrimSpace(read(t, dir, "request.json")),
			response: read(t, dir, "response.json"),
			status:   status,
		})
	}
	if len(out) == 0 {
		t.Fatalf("no fixtures in %s; the adapter's determinism rests on them", fixtureDir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.Join(dir, name), err)
	}
	return string(b)
}

// replay serves one fixture and returns what the adapter made of it.
//
// The error is deliberately dropped here: the fixtures that carry one are
// asserted on separately, and the callers of replay are asking what the
// RESPONSE says.
func replay(t *testing.T, f fixture) *core.Response {
	t.Helper()
	resp, _ := replayWithError(t, f)
	return resp
}

// replayWithError serves one fixture and returns both halves.
func replayWithError(t *testing.T, f fixture) (*core.Response, error) {
	t.Helper()

	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.response))
	})
	a := newAgent(t, srv)

	resp, err := a.Invoke(t.Context(), f.evalCase())

	// The request golden. A fixture is only evidence about the RESPONSE if the
	// request that produced it is still the request the adapter sends — a
	// changed prompt shape would otherwise keep passing against a recording of
	// an exchange that can no longer happen.
	if got := strings.TrimSpace(rec.body(t, 0)); got != f.request {
		t.Errorf("the request no longer matches the one this fixture was recorded "+
			"against;\n got %s\nwant %s\nre-record with `make record-fixtures`", got, f.request)
	}
	return resp, err
}

// TestFixturesReplay asserts each recorded exchange still means what it meant.
func TestFixturesReplay(t *testing.T) {
	t.Parallel()

	// Keyed by fixture name so a new fixture with no assertion is a failure
	// rather than a silent addition.
	checks := map[string]func(*testing.T, *core.Response, error){
		"simple-answer": func(t *testing.T, r *core.Response, err error) {
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if r.GetOutput() != "4" {
				t.Errorf("output = %q, want %q", r.GetOutput(), "4")
			}
			if r.GetStopReason() != knov1.StopReason_STOP_REASON_STOP {
				t.Errorf("stop_reason = %v, want STOP", r.GetStopReason())
			}
			if r.GetPromptTokens() != 16 || r.GetCompletionTokens() != 5 {
				t.Errorf("tokens = %d/%d, want 16/5", r.GetPromptTokens(), r.GetCompletionTokens())
			}
			// 16 * $3/MTok rounded up, plus 5 * $15/MTok rounded up.
			if want := int64(48 + 75); r.GetCostUsdMicros() != want {
				t.Errorf("cost = %d micro-USD, want %d", r.GetCostUsdMicros(), want)
			}
			if r.GetUsageEstimated() {
				t.Error("a reported usage block was marked estimated")
			}
		},

		"cached-prefix": func(t *testing.T, r *core.Response, err error) {
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if want := int64(42 + 1024); r.GetPromptTokens() != want {
				t.Errorf("prompt_tokens = %d, want %d — input_tokens counts only what "+
					"follows the last cache breakpoint, so billed input is the sum",
					r.GetPromptTokens(), want)
			}
			if r.GetCachedTokens() != 1024 {
				t.Errorf("cached_tokens = %d, want 1024", r.GetCachedTokens())
			}
			// 42 * $3 + 1024 * $0.30 + 18 * $15, per MTok, each rounded up.
			if want := int64(126 + 308 + 270); r.GetCostUsdMicros() != want {
				t.Errorf("cost = %d micro-USD, want %d; a cache read priced at the "+
					"fresh input rate would be 10x this term",
					r.GetCostUsdMicros(), want)
			}
		},

		"truncated": func(t *testing.T, r *core.Response, err error) {
			if err != nil {
				t.Fatalf("a well-formed 200 with a truncated answer became an error: %v", err)
			}
			if r.GetStopReason() != knov1.StopReason_STOP_REASON_LENGTH {
				t.Errorf("stop_reason = %v, want LENGTH", r.GetStopReason())
			}
			if !strings.HasSuffix(r.GetOutput(), "991") {
				t.Error("the partial answer did not survive; it is the measurement")
			}
		},

		"refusal-preoutput": func(t *testing.T, r *core.Response, err error) {
			if err != nil {
				t.Fatalf("a refusal became an error: %v", err)
			}
			if !r.GetRefused() {
				t.Error("refused is false; without it a refusing account reports a " +
					"clean baseline of 0.000")
			}
			if r.GetStopReason() != knov1.StopReason_STOP_REASON_CONTENT_FILTER {
				t.Errorf("stop_reason = %v, want CONTENT_FILTER", r.GetStopReason())
			}
			if r.GetCostUsdMicros() != 0 || r.GetUsageEstimated() {
				t.Errorf("cost = %d, usage_estimated = %v; a pre-output refusal is "+
					"documented as unbilled, and that is a measurement rather than a guess",
					r.GetCostUsdMicros(), r.GetUsageEstimated())
			}
		},

		"no-usage-block": func(t *testing.T, r *core.Response, err error) {
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if !r.GetUsageEstimated() {
				t.Error("usage_estimated is false for a cost nothing reported")
			}
			if r.GetCostUsdMicros() <= 0 {
				t.Error("settled at zero, which is what makes a dollar cap unenforceable")
			}
		},

		"rate-limited": func(t *testing.T, _ *core.Response, err error) {
			if !errors.Is(err, errs.ErrRateLimited) {
				t.Errorf("err = %v, want ErrRateLimited", err)
			}
		},

		"overloaded": func(t *testing.T, _ *core.Response, err error) {
			if !errors.Is(err, errs.ErrTransportTransient) {
				t.Errorf("err = %v, want ErrTransportTransient — 529 is not in "+
					"net/http's status constants, which is how it lands in a "+
					"terminal branch by accident", err)
			}
		},
	}

	for _, f := range loadFixtures(t) {
		check, ok := checks[f.name]
		if !ok {
			t.Errorf("fixture %q has no assertion; a recording nothing checks is "+
				"a file, not a test", f.name)
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			resp, err := replayWithError(t, f)
			check(t, resp, err)
		})
	}
}

// TestFixturesCarryNothingTheyShouldNot.
//
// Fixtures are committed forever, so a credential or a header that reached one
// is a credential in git history. The allowlist is the control: a file outside
// it fails rather than being scrubbed, because scrubbing can only remove what
// someone anticipated.
func TestFixturesCarryNothingTheyShouldNot(t *testing.T) {
	t.Parallel()

	// Header names that must not appear anywhere in a fixture, in any casing.
	// Not the control — the file allowlist is — but a second reading of the same
	// rule, so a future fixture format that DID record headers fails here too.
	banned := []string{
		"x-api-key", "authorization", "cookie", "set-cookie",
		"anthropic-beta", "openai-organization", "openai-project",
		"proxy-authorization", "bearer ",
	}

	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("reading %s: %v", fixtureDir, err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			// The directory's own README is prose about the rules, not a
			// recording. Only fixture directories are scanned.
			continue
		}
		dir := filepath.Join(fixtureDir, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, f := range files {
			if !fixtureFiles[f.Name()] {
				t.Errorf("%s/%s is not in the fixture allowlist; a fixture may contain "+
					"only %v", e.Name(), f.Name(), sortedKeys(fixtureFiles))
				continue
			}
			body := strings.ToLower(read(t, dir, f.Name()))
			for _, b := range banned {
				if strings.Contains(body, b) {
					t.Errorf("%s/%s mentions %q; fixtures record no headers in either "+
						"direction", e.Name(), f.Name(), b)
				}
			}
		}
	}
}

// TestFixturesAreRecordedAgainstASyntheticCorpus.
//
// Every Case a fixture was recorded against is checked in next to it, so a
// reviewer can see what was sent without running anything — and so no user's
// evals can reach the repository through a recording.
func TestFixturesAreRecordedAgainstASyntheticCorpus(t *testing.T) {
	t.Parallel()

	for _, f := range loadFixtures(t) {
		if f.input == "" {
			t.Errorf("fixture %q has an empty case.txt", f.name)
		}
		if !strings.Contains(f.request, jsonEscape(f.input)) {
			t.Errorf("fixture %q records a request that does not carry its own Case, "+
				"so the recording and the corpus have drifted apart", f.name)
		}
	}
}

// TestTheAdapterHasNoLiveDefault.
//
// The whole fixture suite is worthless if a test can reach the network by
// accident. Nothing in this package constructs an Agent against
// DefaultBaseURL without a credential, and this asserts that the default path
// refuses rather than dials.
func TestTheAdapterHasNoLiveDefault(t *testing.T) {
	t.Setenv(anthropic.DefaultKeyEnv, "")

	if _, err := anthropic.New(anthropic.Options{Model: testModel, MaxOutputTokens: 8}); err == nil {
		t.Fatal("an Agent aimed at the live API was constructed with no credential")
	}
}

// jsonEscape is the subset of JSON string escaping a Case input can need.
//
// Hand-rolled because ADR-0001 bans encoding/json outside format.go, and the
// only characters the synthetic corpus carries are quotes and backslashes. A
// corpus that needs more will fail this check loudly rather than pass wrongly.
func jsonEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

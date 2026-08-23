package openaicompat_test

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// fixtureDir holds recorded provider replies. See testdata/README.md.
const fixtureDir = "testdata/fixtures"

// permittedFixtureHeaders is the ALLOWLIST — the closed set of things a fixture
// may say about a reply.
//
// An allowlist rather than a denylist, and the difference is not pedantry. A
// denylist can only remove what someone anticipated: `sk-…` matches, and
// OpenAI-Organization, OpenAI-Project, anthropic-beta, x-request-id, and
// Set-Cookie do not, and gitleaks catches key SHAPES rather than org IDs. The
// failure would be silent and permanent, because a fixture is committed
// forever.
//
// Note what is absent: there is no slot for a REQUEST header at all, and none
// for a request body. The request body carries the Case input, which is
// customer data. What a request should say is asserted against httptest
// instead, where nothing is committed.
var permittedFixtureHeaders = map[string]bool{
	"name":         true,
	"source":       true,
	"note":         true,
	"status":       true,
	"content-type": true,
	"retry-after":  true,
}

// forbiddenInFixtures are names that must never appear anywhere in a fixture,
// header or body.
//
// The allowlist above is the real defense; this is the second line, and it
// catches the case the allowlist cannot — a credential that arrived inside the
// response BODY because a provider echoed a request back.
var forbiddenInFixtures = []string{
	"authorization",
	"x-api-key",
	"api-key",
	"cookie",
	"proxy-authorization",
	"openai-organization",
	"openai-project",
	"anthropic-beta",
	"bearer ",
	"sk-proj-",
	"sk-ant-",
}

type fixture struct {
	path    string
	headers map[string]string
	body    string
}

// loadFixtures reads every fixture, failing on a malformed one.
func loadFixtures(t *testing.T) []fixture {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(fixtureDir, "*.fixture"))
	if err != nil {
		t.Fatalf("globbing fixtures: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no fixtures. Adapter tests in PR CI never touch the network, so " +
			"with no fixtures this suite proves nothing about a real provider reply")
	}
	sort.Strings(paths)

	out := make([]fixture, 0, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p) //nolint:gosec // a test reading its own testdata
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		head, body, ok := strings.Cut(string(raw), "\n\n")
		if !ok {
			t.Fatalf("%s has no blank line separating its headers from the body", p)
		}
		f := fixture{path: p, headers: map[string]string{}, body: body}
		for _, line := range strings.Split(head, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				t.Fatalf("%s: %q is not `name: value`", p, line)
			}
			f.headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
		out = append(out, f)
	}
	return out
}

// TestFixturesCarryOnlyWhatTheAllowlistPermits.
//
// A fixture is committed to this repository forever. Recorded provider traffic
// carries both what was sent and what came back, and CLAUDE.md is explicit that
// traces are customer data. The scan runs on every `go test`, not only in the
// recording path, so a fixture added by hand is checked the same way one
// written by `make record-fixtures` is.
func TestFixturesCarryOnlyWhatTheAllowlistPermits(t *testing.T) {
	t.Parallel()

	for _, f := range loadFixtures(t) {
		t.Run(filepath.Base(f.path), func(t *testing.T) {
			t.Parallel()

			for k := range f.headers {
				if !permittedFixtureHeaders[k] {
					t.Errorf("header %q is not in the allowlist. Adding a field to a "+
						"fixture is a decision about what gets committed forever, so "+
						"the allowlist is edited deliberately or not at all", k)
				}
			}
			for _, required := range []string{"name", "source", "status"} {
				if f.headers[required] == "" {
					t.Errorf("no %s header; a fixture with no stated provenance cannot "+
						"be told from one that was recorded", required)
				}
			}

			lower := strings.ToLower(string(mustRead(t, f.path)))
			for _, bad := range forbiddenInFixtures {
				if strings.Contains(lower, bad) {
					t.Errorf("the fixture contains %q", bad)
				}
			}
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a test reading its own testdata
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

// TestRecordedRepliesMapToRecordableOutcomes replays every fixture.
//
// The assertions are INVARIANTS rather than per-fixture expectations, so a
// re-recording that changes the wording of an answer does not break the suite
// while a re-recording that changes its SHAPE does. Each invariant is one the
// engine depends on downstream:
//
//   - a settled cost is never zero, measured or inferred, because a zero
//     settlement makes a dollar cap unenforceable;
//   - a refusal is a Response and carries text, because erroring it inflates
//     the error rate and an empty one scores as a missing answer;
//   - a reply that reported usage is not marked estimated, because a report
//     that cannot tell the two apart cannot say how much of a total it measured;
//   - an error reply is classified by whether a second attempt could differ.
func TestRecordedRepliesMapToRecordableOutcomes(t *testing.T) {
	t.Parallel()

	for _, f := range loadFixtures(t) {
		t.Run(f.headers["name"], func(t *testing.T) {
			t.Parallel()

			status, err := strconv.Atoi(f.headers["status"])
			if err != nil {
				t.Fatalf("status: %v", err)
			}

			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range f.headers {
					if k == "content-type" || k == "retry-after" {
						w.Header().Set(k, v)
					}
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(f.body))
			})
			a := newAgent(t, srv)
			c := newCase("fixture-case", "What is the capital of France?")

			est, estErr := a.Estimate(t.Context(), c)
			if estErr != nil {
				t.Fatalf("Estimate: %v", estErr)
			}
			resp, err := a.Invoke(t.Context(), c)

			if status >= 400 {
				if err == nil {
					t.Fatalf("HTTP %d produced no error, so a failed call would be "+
						"scored as an answer", status)
				}
				wantRetryable := status == http.StatusTooManyRequests || status >= 500
				if got := retryable(err); got != wantRetryable {
					t.Errorf("retryable = %v, want %v: %v", got, wantRetryable, err)
				}
				if status == http.StatusTooManyRequests {
					if _, ok := retryAfterOf(err); !ok {
						t.Error("the recorded Retry-After did not reach core")
					}
				}
				var act *errs.Actionable
				if !errors.As(err, &act) {
					t.Error("the error is not classified, so the CLI exits 1 for it " +
						"whatever actually happened")
				}
				return
			}

			if err != nil {
				t.Fatalf("a recorded 200 produced an error: %v", err)
			}
			if resp.GetCostUsdMicros() <= 0 {
				t.Errorf("cost_usd_micros = %d; a zero settlement hands the whole "+
					"reservation back to the guard", resp.GetCostUsdMicros())
			}
			if resp.GetUsageEstimated() && resp.GetCostUsdMicros() != est.CostUSDMicros {
				t.Errorf("an inferred cost of %d does not equal the reservation of %d; "+
					"the guard and the store would disagree across a resume",
					resp.GetCostUsdMicros(), est.CostUSDMicros)
			}
			if !resp.GetUsageEstimated() && resp.GetPromptTokens() == 0 {
				t.Error("the cost is marked measured but no tokens were reported")
			}
			if resp.GetRefused() && resp.GetOutput() == "" {
				t.Error("a refusal reached the Goal with no text, so it scores as a " +
					"missing answer rather than a declined one")
			}
			if resp.GetStopReason() == knov1.StopReason_STOP_REASON_CONTENT_FILTER &&
				!resp.GetRefused() {
				t.Error("a content-filter stop did not set refused, which is the " +
					"authoritative field for the run-level count")
			}
			if resp.GetCaseId() != c.GetId() {
				t.Errorf("case_id = %q, want %q", resp.GetCaseId(), c.GetId())
			}
		})
	}
}

// TestTheFixtureSetCoversEveryOutcomeTheMappingDistinguishes.
//
// A fixture set that only ever records the happy path proves the happy path.
// The outcomes below are the ones whose mishandling produces a wrong NUMBER
// rather than a visible failure, so each has to be represented.
func TestTheFixtureSetCoversEveryOutcomeTheMappingDistinguishes(t *testing.T) {
	t.Parallel()

	want := map[string]bool{
		"an ordinary answer":        false,
		"a refusal":                 false,
		"a truncation":              false,
		"a reply with no usage":     false,
		"a rate limit":              false,
		"a terminal provider error": false,
	}

	for _, f := range loadFixtures(t) {
		status, err := strconv.Atoi(f.headers["status"])
		if err != nil {
			t.Fatalf("%s: status: %v", f.path, err)
		}
		body := f.body
		switch {
		case status == http.StatusTooManyRequests:
			want["a rate limit"] = true
		case status >= 400:
			want["a terminal provider error"] = true
		case strings.Contains(body, `"content_filter"`), strings.Contains(body, `"refusal": "`):
			want["a refusal"] = true
		case strings.Contains(body, `"length"`):
			want["a truncation"] = true
		case !strings.Contains(body, `"usage"`):
			want["a reply with no usage"] = true
		default:
			want["an ordinary answer"] = true
		}
	}

	for outcome, covered := range want {
		if !covered {
			t.Errorf("no fixture records %s; its mapping is exercised only against "+
				"a hand-written body in this package's own tests", outcome)
		}
	}
}

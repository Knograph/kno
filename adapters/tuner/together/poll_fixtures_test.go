package together_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/tuner/together"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// This file converts #184's inline httptest table (a single submit/status
// round trip) to the on-disk fixture story the tuner-bridge plan's Step 6
// specifies: a NEW fixture kind, the poll SEQUENCE — poll-01.json ...
// poll-NN.json, replayed in order — so the state machine
// VALIDATING_FILES -> QUEUED -> RUNNING -> DEPLOYING -> SUCCEEDED is
// deterministic and every branch, including FAILED with the provider's
// error text, has a fixture. Criterion 23.

// pollFixtureDir is where recorded poll-sequence exchanges live.
const pollFixtureDir = "testdata/fixtures"

// pollFixtureAllowlist is what a poll-sequence fixture directory may
// contain: the same case.txt-shaped allowlist
// adapters/agent/anthropic/testdata/fixtures/README.md documents, adapted
// per the tuner-bridge plan's Step 6(2): "case.txt is replaced by
// training_data.jsonl" (unused by these fixtures — no upload step is
// implemented yet, see together.go's own note) "and poll-NN.json" is a new
// allowed shape. No headers are recorded in either direction, in either
// package, for the same reason: a denylist can only remove a header name
// someone anticipated.
var pollFixtureStaticFiles = map[string]bool{
	"request.json":        true,
	"response.json":       true,
	"status":              true,
	"note.txt":            true,
	"training_data.jsonl": true,
}

var pollFileRE = regexp.MustCompile(`^poll-\d+\.json$`)

// pollFixture is one recorded submit-then-poll-to-terminal exchange.
type pollFixture struct {
	name           string
	submitRequest  string
	submitResponse string
	submitStatus   int
	polls          []string // one statusResponse body per successive GET, in order
}

func loadPollFixtures(t *testing.T) []pollFixture {
	t.Helper()
	entries, err := os.ReadDir(pollFixtureDir)
	if err != nil {
		t.Fatalf("reading %s: %v", pollFixtureDir, err)
	}

	var out []pollFixture
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(pollFixtureDir, e.Name())
		status, err := strconv.Atoi(strings.TrimSpace(pollRead(t, dir, "status")))
		if err != nil {
			t.Fatalf("%s: status is not a number: %v", e.Name(), err)
		}

		files, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		var pollNames []string
		for _, f := range files {
			if pollFileRE.MatchString(f.Name()) {
				pollNames = append(pollNames, f.Name())
			}
		}
		if len(pollNames) == 0 {
			t.Fatalf("%s: no poll-NN.json files — the poll sequence has nothing to replay", e.Name())
		}
		sort.Strings(pollNames)

		var polls []string
		for _, name := range pollNames {
			polls = append(polls, pollRead(t, dir, name))
		}

		out = append(out, pollFixture{
			name:           e.Name(),
			submitRequest:  strings.TrimSpace(pollRead(t, dir, "request.json")),
			submitResponse: pollRead(t, dir, "response.json"),
			submitStatus:   status,
			polls:          polls,
		})
	}
	if len(out) == 0 {
		t.Fatalf("no fixtures in %s; the adapter's poll-sequence parsing rests on them", pollFixtureDir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func pollRead(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.Join(dir, name), err)
	}
	return string(b)
}

// pollSequenceServer serves one fixture's submit response once, then each
// poll-NN.json body in order on successive GETs — no network beyond this
// local test server.
func pollSequenceServer(t *testing.T, f pollFixture) (*httptest.Server, *string) {
	t.Helper()
	var gotRequest string
	pollIdx := 0

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/fine-tunes", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		gotRequest = strings.TrimSpace(string(body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.submitStatus)
		_, _ = w.Write([]byte(f.submitResponse))
	})
	mux.HandleFunc("GET /v1/fine-tunes/", func(w http.ResponseWriter, _ *http.Request) {
		if pollIdx >= len(f.polls) {
			t.Fatalf("%s: polled %d times, but the fixture has only %d poll-NN.json files",
				f.name, pollIdx+1, len(f.polls))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.polls[pollIdx]))
		pollIdx++
	})
	return httptest.NewServer(mux), &gotRequest
}

func newPollTuner(t *testing.T, srv *httptest.Server) *together.Tuner {
	t.Helper()
	tuner, err := together.New(together.Options{
		BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true,
		KeyEnv: bindKey(t, srv.URL),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tuner
}

// submitJob is the job every poll-sequence fixture in this test file was
// recorded against — a fixed shape so request.json's bytes are pinned per
// fixture and reviewable.
func submitJob(suffix string) *core.TuningJob {
	return &core.TuningJob{
		BaseModel: &core.AgentRef{Ref: "together:meta-llama/Llama-3-8b", Target: "meta-llama/Llama-3-8b"},
		Epochs:    3,
		Suffix:    suffix,
	}
}

// TestPollSequenceReplaysFromFixturesToSuccess is acceptance criterion 23's
// success half: VALIDATING_FILES -> QUEUED -> RUNNING -> DEPLOYING ->
// SUCCEEDED, deterministic, no network beyond the local fixture server.
func TestPollSequenceReplaysFromFixturesToSuccess(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-test")

	f := loadOnePollFixture(t, "poll-sequence")
	srv, gotRequest := pollSequenceServer(t, f)
	defer srv.Close()
	tuner := newPollTuner(t, srv)

	ref, err := tuner.Submit(context.Background(), submitJob("kno-run-1-all-in"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if *gotRequest != f.submitRequest {
		t.Errorf("the submit request no longer matches the one this fixture was recorded "+
			"against;\n got %s\nwant %s\nre-record with `make record-fixtures`", *gotRequest, f.submitRequest)
	}

	want := []knov1.JobStatus{
		knov1.JobStatus_JOB_STATUS_VALIDATING_FILES,
		knov1.JobStatus_JOB_STATUS_QUEUED,
		knov1.JobStatus_JOB_STATUS_RUNNING,
		knov1.JobStatus_JOB_STATUS_DEPLOYING,
		knov1.JobStatus_JOB_STATUS_SUCCEEDED,
	}
	var last *core.JobState
	for i, wantStatus := range want {
		state, err := tuner.Status(context.Background(), ref)
		if err != nil {
			t.Fatalf("Status poll %d: %v", i+1, err)
		}
		if state.GetStatus() != wantStatus {
			t.Errorf("poll %d: status = %v, want %v", i+1, state.GetStatus(), wantStatus)
		}
		last = state
	}
	if last.GetTunedModel() == nil {
		t.Error("the terminal SUCCEEDED poll must produce a tuned model")
	}
	if last.GetActualCostUsdMicros() != 6_120_000 {
		t.Errorf("ActualCostUsdMicros = %d, want 6120000", last.GetActualCostUsdMicros())
	}
}

// TestPollSequenceFailedFixtureSurfacesProviderErrorVerbatim is acceptance
// criterion 23's failure half: a FAILED fixture surfaces the provider's
// error text verbatim, per JobState.error's own godoc.
func TestPollSequenceFailedFixtureSurfacesProviderErrorVerbatim(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-test")

	f := loadOnePollFixture(t, "poll-failed")
	srv, _ := pollSequenceServer(t, f)
	defer srv.Close()
	tuner := newPollTuner(t, srv)

	ref, err := tuner.Submit(context.Background(), submitJob("kno-run-1-cluster-x"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	var last *core.JobState
	for i := range f.polls {
		state, err := tuner.Status(context.Background(), ref)
		if err != nil {
			t.Fatalf("Status poll %d: %v", i+1, err)
		}
		last = state
	}
	if last.GetStatus() != knov1.JobStatus_JOB_STATUS_FAILED {
		t.Fatalf("terminal status = %v, want FAILED", last.GetStatus())
	}
	const wantErr = "training file failed validation: line 42 has no assistant message"
	if last.GetError() != wantErr {
		t.Errorf("Error = %q, want %q (verbatim)", last.GetError(), wantErr)
	}
}

func loadOnePollFixture(t *testing.T, name string) pollFixture {
	t.Helper()
	for _, f := range loadPollFixtures(t) {
		if f.name == name {
			return f
		}
	}
	t.Fatalf("no fixture named %s in %s", name, pollFixtureDir)
	return pollFixture{}
}

// TestPollFixturesCarryNothingTheyShouldNot enforces the allowlist:
// request.json, response.json, status, note.txt, training_data.jsonl, and
// poll-NN.json — nothing else, and no headers are recorded in either
// direction. Criterion 22.
func TestPollFixturesCarryNothingTheyShouldNot(t *testing.T) {
	entries, err := os.ReadDir(pollFixtureDir)
	if err != nil {
		t.Fatalf("reading %s: %v", pollFixtureDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(pollFixtureDir, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, f := range files {
			if pollFixtureStaticFiles[f.Name()] || pollFileRE.MatchString(f.Name()) {
				continue
			}
			t.Errorf("%s/%s is not in the fixture allowlist", e.Name(), f.Name())
		}
	}
}

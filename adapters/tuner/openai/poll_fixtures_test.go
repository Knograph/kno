package openai_test

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

	"github.com/knograph/kno/adapters/tuner/openai"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// This file is together/poll_fixtures_test.go's own pattern, mirrored for
// this package: the FULL job lifecycle — submit, poll to a terminal
// status, and (on the success path) the Deploy readiness probe — replayed
// from on-disk fixtures, deterministic, no network beyond a local test
// server. See docs/plans/2026-09-02-openai-tuner.md's test plan: "recorded
// fixtures for the full job lifecycle including a failure."

// pollFixtureDir is where recorded poll-sequence exchanges live.
const pollFixtureDir = "testdata/fixtures"

// pollFixtureStaticFiles is what a poll-sequence fixture directory may
// contain besides poll-NN.json — the allowlist together's own
// pollFixtureStaticFiles documents, extended with probe.json/probe-status
// for this adapter's Deploy readiness check.
var pollFixtureStaticFiles = map[string]bool{
	"request.json":        true,
	"response.json":       true,
	"status":              true,
	"probe.json":          true,
	"probe-status":        true,
	"note.txt":            true,
	"training_data.jsonl": true,
}

var pollFileRE = regexp.MustCompile(`^poll-\d+\.json$`)

// pollFixture is one recorded submit-then-poll-to-terminal exchange, plus
// an optional Deploy readiness probe.
type pollFixture struct {
	name           string
	submitRequest  string
	submitResponse string
	submitStatus   int
	polls          []string // one statusResponse body per successive GET, in order

	// probeBody/probeStatus are the Deploy readiness probe's response —
	// present only for fixtures that reach SUCCEEDED (poll-sequence has
	// one; poll-failed does not, since Deploy is never called on a failed
	// job).
	probeBody   string
	probeStatus int
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

		f := pollFixture{
			name:           e.Name(),
			submitRequest:  strings.TrimSpace(pollRead(t, dir, "request.json")),
			submitResponse: pollRead(t, dir, "response.json"),
			submitStatus:   status,
			polls:          polls,
		}
		if pollExists(dir, "probe.json") {
			f.probeBody = pollRead(t, dir, "probe.json")
			probeStatus, err := strconv.Atoi(strings.TrimSpace(pollRead(t, dir, "probe-status")))
			if err != nil {
				t.Fatalf("%s: probe-status is not a number: %v", e.Name(), err)
			}
			f.probeStatus = probeStatus
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatalf("no fixtures in %s; the adapter's poll-sequence parsing rests on them", pollFixtureDir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func pollExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func pollRead(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.Join(dir, name), err)
	}
	return string(b)
}

// pollSequenceServer serves one fixture's submit response once, each
// poll-NN.json body in order on successive GETs to the job-status route,
// and (when the fixture carries one) the readiness-probe body on a GET to
// the models route — no network beyond this local test server.
func pollSequenceServer(t *testing.T, f pollFixture) (*httptest.Server, *string) {
	t.Helper()
	var gotRequest string
	pollIdx := 0

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/fine_tuning/jobs", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		gotRequest = strings.TrimSpace(string(body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.submitStatus)
		_, _ = w.Write([]byte(f.submitResponse))
	})
	mux.HandleFunc("GET /v1/fine_tuning/jobs/", func(w http.ResponseWriter, _ *http.Request) {
		// Once the recorded sequence is exhausted, keep serving the LAST
		// (terminal) body — a real provider answers the same terminal
		// status on every further poll, and Deploy's own Status call is
		// exactly one such extra poll past the loop that already drove the
		// job to its terminal state.
		idx := pollIdx
		if idx >= len(f.polls) {
			idx = len(f.polls) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.polls[idx]))
		pollIdx++
	})
	mux.HandleFunc("GET /v1/models/", func(w http.ResponseWriter, _ *http.Request) {
		if f.probeBody == "" {
			t.Fatalf("%s: Deploy probed readiness, but the fixture carries no probe.json", f.name)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.probeStatus)
		_, _ = w.Write([]byte(f.probeBody))
	})
	return httptest.NewServer(mux), &gotRequest
}

func newPollTuner(t *testing.T, srv *httptest.Server) *openai.Tuner {
	t.Helper()
	tuner, err := openai.New(openai.Options{
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
// fixture and reviewable, mirroring together/poll_fixtures_test.go's own
// submitJob.
func submitJob(suffix string) *core.TuningJob {
	return &core.TuningJob{
		BaseModel: &core.AgentRef{Ref: "openai:gpt-5.6-terra", Target: "gpt-5.6-terra"},
		Epochs:    3,
		Suffix:    suffix,
	}
}

// TestPollSequenceReplaysFromFixturesToSuccessAndDeploys is the success
// path: VALIDATING_FILES -> QUEUED -> RUNNING -> SUCCEEDED, then Deploy's
// readiness probe answering 200 — Ready true with a non-zero ReadyAt.
func TestPollSequenceReplaysFromFixturesToSuccessAndDeploys(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

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
	if ref.GetSubmittedAt() == "" {
		t.Error("SubmittedAt must be a non-empty RFC 3339 string, converted from the fixture's unix created_at")
	}

	want := []knov1.JobStatus{
		knov1.JobStatus_JOB_STATUS_VALIDATING_FILES,
		knov1.JobStatus_JOB_STATUS_QUEUED,
		knov1.JobStatus_JOB_STATUS_RUNNING,
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
		t.Fatal("the terminal SUCCEEDED poll must produce a tuned model")
	}
	// ActualCostUsdMicros stays zero — see openai.go's statusResponse doc:
	// OpenAI's job status API reports no dollar figure, unlike Together's
	// (unconfirmed) total_price_usd_micros.
	if last.GetActualCostUsdMicros() != 0 {
		t.Errorf("ActualCostUsdMicros = %d, want 0 (the provider reports no cost field)", last.GetActualCostUsdMicros())
	}

	ref.Id = last.GetRef().GetId()
	ep, err := tuner.Deploy(context.Background(), ref)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !ep.Ready {
		t.Fatal("Deploy must report Ready true when the readiness probe answers 200")
	}
	if ep.ReadyAt.IsZero() {
		t.Error("a Ready Endpoint must carry a non-zero ReadyAt — bridge.DeployGroup (#208) refuses one that does not")
	}
	if ep.Replicas != 0 {
		t.Errorf("Replicas = %d, want 0 (no billed replica on an auto-serving provider)", ep.Replicas)
	}
	if err := tuner.Teardown(context.Background(), ep); err != nil {
		t.Errorf("Teardown after a successful Deploy: %v", err)
	}
}

// TestPollSequenceFailedFixtureSurfacesProviderErrorVerbatim is the failure
// path: a FAILED fixture surfaces the provider's error text verbatim, per
// JobState.error's own godoc — Deploy is never called on this path.
func TestPollSequenceFailedFixtureSurfacesProviderErrorVerbatim(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

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

	if _, err := tuner.Deploy(context.Background(), ref); err == nil {
		t.Error("Deploy must refuse a job with no tuned model")
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

// TestPollFixturesCarryNothingTheyShouldNot enforces the allowlist —
// together's own TestPollFixturesCarryNothingTheyShouldNot, mirrored.
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

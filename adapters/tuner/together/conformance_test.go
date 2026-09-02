package together_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/knograph/kno/adapters/tuner/together"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/coretest"
)

// TestTunerConformance wires this package into coretest.ConformTuner — the
// deploy-required, per-minute, wire-suffix half of the cross-adapter Tuner
// conformance suite docs/plans/2026-09-02-openai-tuner.md's test plan
// calls for, exercised against BOTH this package and adapters/tuner/openai
// (see its own conformance_test.go).
//
// A self-contained in-memory fixture server, not testdata/fixtures: the
// harness needs Deploy, Teardown, ListJobs and ListEndpoints in addition to
// Submit/Status, which the on-disk poll-sequence fixtures (poll_fixtures_
// test.go) do not model — matching the inline-mux pattern this package's
// own together_test.go already uses for TestListJobsMatchesOnlyBySuffix.
func TestTunerConformance(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-test")

	const jobID = "ft-conform-001"
	const endpointID = "ep-conform-001"
	const baseModel = "meta-llama/Llama-3-8b"
	const suffix = "kno-conform"
	tunedModel := baseModel + "-" + suffix

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/fine-tunes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": jobID, "status": "running", "created_at": "2026-09-02T00:00:00Z",
		})
	})
	mux.HandleFunc("GET /v1/fine-tunes/"+jobID, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": jobID, "status": "succeeded", "fine_tuned_model": tunedModel,
		})
	})
	mux.HandleFunc("POST /v1/endpoints", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": endpointID, "state": "started", "model": tunedModel,
		})
	})
	mux.HandleFunc("DELETE /v1/endpoints/"+endpointID, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/fine-tunes", func(w http.ResponseWriter, _ *http.Request) {
		// Together's ListJobs filters CLIENT-SIDE (no server-side query
		// param) — this endpoint returns every job, matched and unmatched,
		// which is what proves the adapter's own filtering, not the
		// server's, is what the harness is exercising.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": jobID, "status": "succeeded", "suffix": suffix, "created_at": "2026-09-02T00:00:00Z"},
				{"id": "ft-other", "status": "running", "suffix": "kno-conform-does-not-exist", "created_at": "2026-09-02T00:00:00Z"},
			},
		})
	})
	mux.HandleFunc("GET /v1/endpoints", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": endpointID, "state": "started", "model": tunedModel},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	coretest.ConformTuner(t, coretest.TunerScenario{
		NewTuner: func() (core.Tuner, error) {
			return together.New(together.Options{
				BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true,
				KeyEnv: bindKey(t, srv.URL),
			})
		},
		Job: &core.TuningJob{
			BaseModel: &core.AgentRef{Ref: "together:" + baseModel, Target: baseModel},
			Epochs:    3,
			Suffix:    suffix,
		},
		Suffix:         suffix,
		NegativeSuffix: suffix + "-does-not-exist",
		// A Together dedicated endpoint IS a real, listable provider
		// resource once Deploy succeeds — the structural opposite of
		// openai's own conformance scenario (EndpointsAlwaysEmpty: true
		// there). See coretest.TunerScenario.EndpointsAlwaysEmpty's doc.
		EndpointsAlwaysEmpty: false,
	})
}

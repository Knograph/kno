package openai_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/knograph/kno/adapters/tuner/openai"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/coretest"
)

// TestTunerConformance wires this package into coretest.ConformTuner — the
// auto-serve, per-token, metadata-tagged half of the cross-adapter Tuner
// conformance suite docs/plans/2026-09-02-openai-tuner.md's test plan
// calls for, exercised against BOTH adapters/tuner/together (see its own
// conformance_test.go) and this package.
//
// A self-contained in-memory fixture server, not testdata/fixtures: the
// harness needs Deploy, Teardown, ListJobs and ListEndpoints in addition to
// Submit/Status, which the on-disk poll-sequence fixtures (poll_fixtures_
// test.go) do not model — matching the inline-mux pattern this package's
// own openai_test.go already uses for TestListJobsMatchesOnlyByMetadataSuffix.
func TestTunerConformance(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")

	const jobID = "ftjob-conform-001"
	const tunedModel = "ft:gpt-5.6-terra:kno-conform"
	const suffix = "kno-conform"

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/fine_tuning/jobs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": jobID, "status": "running", "created_at": 1_800_000_000,
		})
	})
	mux.HandleFunc("GET /v1/fine_tuning/jobs/"+jobID, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": jobID, "status": "succeeded", "fine_tuned_model": tunedModel,
		})
	})
	mux.HandleFunc("GET /v1/models/"+tunedModel, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": tunedModel})
	})
	mux.HandleFunc("GET /v1/fine_tuning/jobs", func(w http.ResponseWriter, r *http.Request) {
		matches := r.URL.Query().Get("metadata[kno_suffix]") == suffix
		w.Header().Set("Content-Type", "application/json")
		if !matches {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id": jobID, "status": "succeeded", "created_at": 1_800_000_000,
					"metadata": map[string]string{"kno_suffix": suffix},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	coretest.ConformTuner(t, coretest.TunerScenario{
		NewTuner: func() (core.Tuner, error) {
			return openai.New(openai.Options{
				BaseURL: srv.URL, AllowPrivateAddress: true, AllowInsecureBaseURL: true,
				KeyEnv: bindKey(t, srv.URL),
			})
		},
		Job: &core.TuningJob{
			BaseModel: &core.AgentRef{Ref: "openai:gpt-5.6-terra", Target: "gpt-5.6-terra"},
			Epochs:    3,
			Suffix:    suffix,
		},
		Suffix:         suffix,
		NegativeSuffix: suffix + "-does-not-exist",
		// §3 of docs/plans/2026-09-02-openai-tuner.md: OpenAI creates no
		// dedicated-endpoint resource, so ListEndpoints is always empty —
		// the structural difference from together's own conformance
		// scenario (EndpointsAlwaysEmpty: false there).
		EndpointsAlwaysEmpty: true,
	})
}

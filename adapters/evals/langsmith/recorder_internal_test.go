//go:build integration

package langsmith

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestRecordLiveFixtures captures the live dataset envelope and its first
// page into testdata/live/ so a fixture review can compare hand-authored
// shapes against evidence. Refuses to write a response that echoes the key.
//
// Internal on purpose: the recording must go through the package's own
// client — the same redirect refusal, 429 retry, and address policy the
// adapter ships — so what is recorded is what the adapter would have read,
// not what a bare http.Client happens to see.
func TestRecordLiveFixtures(t *testing.T) {
	if os.Getenv("KNO_LIVE_TESTS") != "1" {
		t.Skip("KNO_LIVE_TESTS!=1; skipping live LangSmith test")
	}
	if os.Getenv("KNO_RECORD_FIXTURES") != "1" {
		t.Skip("KNO_RECORD_FIXTURES!=1; skipping fixture recording")
	}
	if os.Getenv(DefaultKeyEnv) == "" {
		t.Skip("LANGSMITH_API_KEY unset")
	}
	dataset := os.Getenv("KNO_LIVE_DATASET")
	if dataset == "" {
		t.Skip("KNO_LIVE_DATASET unset")
	}
	ev, err := New(Options{
		Dataset: dataset,
		// AllowPrivateAddress is only for local self-hosting (kind, docker,
		// the agent transport precedent); the hosted endpoint needs neither
		// flag.
		AllowPrivateAddress: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	key := os.Getenv(DefaultKeyEnv)
	ctx := context.Background()

	get := func(path string, query url.Values) []byte {
		t.Helper()
		resp, err := ev.do(ctx, path, query)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if int64(len(body)) > maxPageBytes {
			t.Fatalf("GET %s: response exceeded %d bytes", path, maxPageBytes)
		}
		return body
	}

	q := url.Values{}
	q.Set("name", dataset)
	q.Set("limit", strconv.Itoa(pageSize))
	datasets := get("/datasets", q)

	// The examples endpoint is keyed by the dataset ID, which only the
	// /datasets response carries; the recorder resolves it the same way the
	// adapter would.
	var env struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(datasets, &env); err != nil || len(env.Items) == 0 {
		t.Fatalf("the datasets response did not carry a dataset id: %v", err)
	}
	q = url.Values{}
	q.Set("dataset_id", env.Items[0].ID)
	q.Set("limit", strconv.Itoa(pageSize))
	examples := get("/examples", q)

	for name, body := range map[string]string{
		"datasets-live.json": string(datasets),
		"examples-live.json": string(examples),
	} {
		if strings.Contains(body, key) {
			t.Fatalf("%s echoes the API key; refusing to record it", name)
		}
	}
	dir := filepath.Join("testdata", "live")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "datasets-live.json"), datasets, 0o600); err != nil {
		t.Fatalf("writing datasets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "examples-live.json"), examples, 0o600); err != nil {
		t.Fatalf("writing examples: %v", err)
	}
}

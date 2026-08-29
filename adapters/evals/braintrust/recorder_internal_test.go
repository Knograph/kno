//go:build integration

package braintrust

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecordLiveFixtures captures the live dataset list and the first fetch
// page into testdata/live/ so a fixture review can compare hand-authored
// shapes against evidence. Refuses to write a response that echoes a key.
//
// Internal on purpose: the recording must go through the package's own
// client — the same redirect refusal, 429 retry, and address policy the
// adapter ships — so what is recorded is what the adapter would have read,
// not what a bare http.Client happens to see.
func TestRecordLiveFixtures(t *testing.T) {
	if os.Getenv("KNO_LIVE_TESTS") != "1" {
		t.Skip("KNO_LIVE_TESTS!=1; skipping live Braintrust test")
	}
	if os.Getenv("KNO_RECORD_FIXTURES") != "1" {
		t.Skip("KNO_RECORD_FIXTURES!=1; skipping fixture recording")
	}
	if os.Getenv(KeyEnv) == "" {
		t.Skip("BRAINTRUST_API_KEY unset")
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

	secret := os.Getenv(KeyEnv)
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
	q.Set("dataset_name", dataset)
	list := get("/v1/dataset", q)

	ds, err := decodeDatasetList(list)
	if err != nil {
		t.Fatalf("decoding the live dataset list: %v", err)
	}
	if len(ds) != 1 {
		t.Fatalf("the live dataset list has %d entries, want 1", len(ds))
	}
	if ds[0].ID == "" {
		t.Fatal("the live dataset has no id")
	}

	q = url.Values{}
	q.Set("limit", "100")
	page := get("/v1/dataset/"+url.PathEscape(ds[0].ID)+"/fetch", q)

	for name, body := range map[string]string{
		"dataset-live.json": string(list),
		"page-live.json":    string(page),
	} {
		if strings.Contains(body, secret) || strings.Contains(body, ev.bearer) {
			t.Fatalf("%s echoes a key; refusing to record it", name)
		}
	}
	dir := filepath.Join("testdata", "live")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dataset-live.json"), list, 0o600); err != nil {
		t.Fatalf("writing dataset: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "page-live.json"), page, 0o600); err != nil {
		t.Fatalf("writing page: %v", err)
	}
}

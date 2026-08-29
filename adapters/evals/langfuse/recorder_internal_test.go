//go:build integration

package langfuse

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

// TestRecordLiveFixtures captures the live dataset object and its first
// items page into testdata/live/ so a fixture review can compare
// hand-authored shapes against evidence. Refuses to write a response that
// echoes a key.
//
// Internal on purpose: the recording must go through the package's own
// client — the same redirect refusal, 429 retry, and address policy the
// adapter ships — so what is recorded is what the adapter would have read,
// not what a bare http.Client happens to see.
func TestRecordLiveFixtures(t *testing.T) {
	if os.Getenv("KNO_LIVE_TESTS") != "1" {
		t.Skip("KNO_LIVE_TESTS!=1; skipping live Langfuse test")
	}
	if os.Getenv("KNO_RECORD_FIXTURES") != "1" {
		t.Skip("KNO_RECORD_FIXTURES!=1; skipping fixture recording")
	}
	if os.Getenv(PublicKeyEnv) == "" || os.Getenv(SecretKeyEnv) == "" {
		t.Skip("LANGFUSE_PUBLIC_KEY or LANGFUSE_SECRET_KEY unset")
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

	secret := os.Getenv(SecretKeyEnv)
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

	ds := get("/api/public/v2/datasets/"+url.PathEscape(dataset), nil)

	q := url.Values{}
	q.Set("datasetName", dataset)
	q.Set("page", "1")
	q.Set("limit", "100")
	items := get("/api/public/dataset-items", q)

	for name, body := range map[string]string{
		"dataset-live.json": string(ds),
		"items-live.json":   string(items),
	} {
		if strings.Contains(body, secret) || strings.Contains(body, ev.basic) {
			t.Fatalf("%s echoes a key; refusing to record it", name)
		}
	}
	dir := filepath.Join("testdata", "live")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dataset-live.json"), ds, 0o600); err != nil {
		t.Fatalf("writing dataset: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "items-live.json"), items, 0o600); err != nil {
		t.Fatalf("writing items: %v", err)
	}
}

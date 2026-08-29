//go:build integration

// This file spends real money. It is the only file in this package that can,
// it is behind a build tag, and it refuses to run unless three separate
// things are true at once.
//
// `make record-fixtures` invokes it as:
//
//	KNO_RECORD_FIXTURES=1 KNO_LIVE_TESTS=1 go test -tags=integration -run TestRecord ./adapters/...
//
// and the Makefile refuses to invoke it at all unless KNO_MAX_COST_USD is set
// AND some Go file actually reads it. This is that file: the cap below is
// enforced by code, not asserted by a comment. See docs/debt.md#11, where a
// nightly job was once armed with real credentials and a cap nothing read.

package bedrock

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/knograph/kno/core"
)

// recordModel is what fixtures are recorded against.
//
// Pinned rather than read from the environment: a fixture recorded against a
// different model is not a re-recording of the same fixture, and the request
// golden would flip to it silently.
const recordModel = "anthropic.claude-sonnet-4-5-20250929-v1:0"

// recordMaxOutputTokens matches what the fixture replay tests configure. The
// request golden compares byte for byte, so these two numbers are one number.
const recordMaxOutputTokens = 1024

// recordSystem is the system prompt the fixture exchange is recorded against.
const recordSystem = "S"

// captureRoundTripper records the exchange while the request proceeds
// normally. The request body is read once for the signature, so it is
// re-spliced before the inner transport sees it.
type captureRoundTripper struct {
	inner http.RoundTripper
	req   []byte
	resp  []byte
}

func (c *captureRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	c.req = append([]byte(nil), body...)

	res, err := c.inner.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	respBody, err := io.ReadAll(res.Body)
	if err != nil {
		res.Body.Close()
		return nil, err
	}
	res.Body.Close()
	res.Body = io.NopCloser(bytes.NewReader(respBody))
	c.resp = append([]byte(nil), respBody...)
	return res, nil
}

// TestRecord re-records the fixture that can be produced on demand.
//
// testdata/fixtures/converse-ok is hand-authored rather than recorded, so this
// target is how an operator with live Bedrock access replaces it with the
// real exchange. It refuses every other route out: no env, no run; no
// KNO_LIVE_TESTS, no run; no cap, no run; test-vector credentials, no run.
func TestRecord(t *testing.T) {
	if os.Getenv("KNO_RECORD_FIXTURES") != "1" {
		t.Skip("set KNO_RECORD_FIXTURES=1 to re-record; this spends real money")
	}
	if os.Getenv("KNO_LIVE_TESTS") != "1" {
		t.Fatal("KNO_RECORD_FIXTURES is set without KNO_LIVE_TESTS; recording calls " +
			"the live API and must pass the same gate as any other live test")
	}

	capUSDMicros := spendCapUSDMicros(t)

	for _, env := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_REGION"} {
		if os.Getenv(env) == "" {
			t.Fatalf("%s is not set; recording needs live credentials", env)
		}
	}
	if os.Getenv("AWS_ACCESS_KEY_ID") == "AKIAI44QH8DHBEXAMPLE" {
		t.Fatal("the test vector access key is set; that is not a live credential")
	}

	capture := &captureRoundTripper{inner: http.DefaultTransport}
	a, err := New(Options{
		Model:           recordModel,
		MaxOutputTokens: recordMaxOutputTokens,
		System:          recordSystem,
		HTTPClient:      &http.Client{Transport: capture},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := a.Invoke(t.Context(), &core.Case{Id: "c1", Input: "hello"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	t.Logf("recorded exchange: %d prompt / %d completion / %d cached, $%d micros",
		resp.PromptTokens, resp.CompletionTokens, resp.CachedTokens, resp.CostUsdMicros)
	if resp.CostUsdMicros > capUSDMicros {
		t.Fatalf("spent %d micros, over the cap", resp.CostUsdMicros)
	}
	if capture.resp == nil {
		t.Fatal("no response was captured; the exchange produced nothing to record")
	}

	dir := filepath.Join("testdata", "fixtures", "converse-ok")
	for name, content := range map[string][]byte{
		"request.json":  capture.req,
		"response.json": capture.resp,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
}

// spendCapUSDMicros reads the spend cap the Makefile arms, fatally.
//
// The cap is in whole dollars — a human-set budget — and is converted to
// micro-USD, which is what this adapter's reservations count in.
func spendCapUSDMicros(t *testing.T) int64 {
	t.Helper()
	raw := os.Getenv("KNO_MAX_COST_USD")
	if raw == "" {
		t.Fatal("KNO_MAX_COST_USD is not set; the Makefile refuses to run recording without a spend cap")
	}
	usd, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || usd <= 0 {
		t.Fatalf("KNO_MAX_COST_USD = %q, want a positive whole-dollar cap", raw)
	}
	return usd * 1_000_000
}

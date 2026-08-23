//go:build integration

// This file spends real money. It is the only file in the package that can, it
// is behind a build tag, and it refuses to run unless three separate things
// are true at once.
//
// `make record-fixtures` invokes it as:
//
//	KNO_RECORD_FIXTURES=1 KNO_LIVE_TESTS=1 go test -tags=integration -run TestRecord ./adapters/...
//
// and the Makefile refuses to invoke it at all unless KNO_MAX_COST_USD is set
// AND some Go file actually reads it. This is that file: the cap below is
// enforced by code, not asserted by a comment. See docs/debt.md#11, where a
// nightly job was once armed with real credentials and a cap nothing read.

package anthropic_test

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/knograph/kno/adapters/agent/anthropic"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// recordModel is what fixtures are recorded against.
//
// Pinned rather than read from the environment: a fixture recorded against a
// different model is not a re-recording of the same fixture, and the request
// golden would flip to it silently.
const recordModel = "claude-sonnet-4-6"

// recordMaxOutputTokens matches what the replay tests configure. The request
// golden compares byte for byte, so these two numbers are one number.
const recordMaxOutputTokens = 1024

// TestRecord re-records the fixtures that can be produced on demand.
//
// Error fixtures — a 429, a 529 — are NOT re-recorded: they cannot be provoked
// deliberately without abusing the provider, and the misbehaving server in
// errors_test.go drives those paths deterministically instead. They are left
// exactly as they are, and this says so rather than quietly skipping them.
func TestRecord(t *testing.T) {
	if os.Getenv("KNO_RECORD_FIXTURES") != "1" {
		t.Skip("set KNO_RECORD_FIXTURES=1 to re-record; this spends real money")
	}
	if os.Getenv("KNO_LIVE_TESTS") != "1" {
		t.Fatal("KNO_RECORD_FIXTURES is set without KNO_LIVE_TESTS; recording calls " +
			"the live API and must pass the same gate as any other live test")
	}

	capUSDMicros := spendCapUSDMicros(t)
	if os.Getenv(anthropic.DefaultKeyEnv) == "" {
		t.Fatalf("%s is not set", anthropic.DefaultKeyEnv)
	}

	rec := &recorder{next: http.DefaultTransport}
	a, err := anthropic.New(anthropic.Options{
		Model:           recordModel,
		MaxOutputTokens: recordMaxOutputTokens,
		HTTPClient:      &http.Client{Transport: rec},
		UserAgent:       "kno-record-fixtures",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The pessimistic per-call ceiling, reserved BEFORE each call. Settling
	// afterwards would be a cap discovered after the money is gone, which is
	// exactly the failure the whole milestone exists to close.
	perCall := a.WorstCase().CostUSDMicros
	if perCall <= 0 {
		t.Fatalf("no worst-case estimate for %s, so the cap cannot be enforced", recordModel)
	}

	var spent int64
	for _, f := range loadFixtures(t) {
		if f.status != http.StatusOK {
			t.Logf("%s: not re-recorded — a %d cannot be provoked deliberately; "+
				"errors_test.go drives that path against the misbehaving server",
				f.name, f.status)
			continue
		}

		if spent+perCall > capUSDMicros {
			t.Fatalf("stopping at %s: %d + %d micro-USD would exceed KNO_MAX_COST_USD "+
				"(%d micro-USD). Raise it deliberately or record fewer fixtures.",
				f.name, spent, perCall, capUSDMicros)
		}

		rec.reset()
		resp, err := a.Invoke(t.Context(), f.evalCase())
		if err != nil {
			t.Errorf("%s: %v", f.name, err)
			continue
		}
		spent += settledOrCeiling(resp, perCall)

		write(t, f.name, "request.json", rec.request)
		write(t, f.name, "response.json", append(rec.response, '\n'))
		write(t, f.name, "status", []byte(strconv.Itoa(rec.status)))
	}

	t.Logf("recorded within KNO_MAX_COST_USD: %d of %d micro-USD", spent, capUSDMicros)
}

// spendCapUSDMicros reads and enforces KNO_MAX_COST_USD.
//
// The name of this variable appearing in a Go file is what the Makefile's guard
// checks for. Its VALUE being honored is what the guard cannot check, which is
// why the arithmetic above runs before each call rather than after the loop.
func spendCapUSDMicros(t *testing.T) int64 {
	t.Helper()

	raw := os.Getenv("KNO_MAX_COST_USD")
	if raw == "" {
		t.Fatal("KNO_MAX_COST_USD is not set; recording will not run without a ceiling")
	}
	usd, err := strconv.ParseFloat(raw, 64)
	if err != nil || usd <= 0 {
		t.Fatalf("KNO_MAX_COST_USD = %q, which is not a positive number of dollars", raw)
	}
	return int64(usd * 1_000_000)
}

// settledOrCeiling charges the reservation when the provider reported nothing.
//
// Never zero: a zero settlement is what makes a cap unenforceable, and the
// no-usage-block fixture is one of the ones being recorded.
func settledOrCeiling(r *knov1.Response, ceiling int64) int64 {
	if r.GetCostUsdMicros() > 0 {
		return r.GetCostUsdMicros()
	}
	return ceiling
}

// write saves one fixture file.
func write(t *testing.T, fixture, name string, body []byte) {
	t.Helper()
	if !fixtureFiles[name] {
		t.Fatalf("%s is not in the fixture allowlist", name)
	}
	path := filepath.Join(fixtureDir, fixture, name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// recorder captures request and response BODIES, and nothing else.
//
// Not the headers. There is deliberately no field for them: a denylist of
// header names can only remove what someone anticipated, and gitleaks matches
// key shapes rather than organization IDs, project IDs, or session cookies. A
// recorder that cannot see a header cannot leak one.
type recorder struct {
	next     http.RoundTripper
	request  []byte
	response []byte
	status   int
}

func (r *recorder) reset() {
	r.request, r.response, r.status = nil, nil, 0
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		r.request = body
		// Put it back. The transport clears GetBody deliberately, so consuming
		// the body here without restoring it would send an empty request.
		req.Body = io.NopCloser(bytes.NewReader(body))
	}

	resp, err := r.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	r.response = body
	r.status = resp.StatusCode
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

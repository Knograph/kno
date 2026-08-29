package vertex

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestFixturesCarryNothingTheyShouldNot pins the fixture allowlist: exactly
// the four files a fixture directory may contain, nothing else — and no
// header files, which is where a recorded credential would land.
func TestFixturesCarryNothingTheyShouldNot(t *testing.T) {
	t.Parallel()

	root := filepath.Join("testdata", "fixtures")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		switch base {
		case "request.json", "response.json", "note.txt":
			// Allowlisted. This adapter's fixtures are hand-authored and pinned
			// in the replay test instead of recorded against a live API.
		default:
			t.Errorf("fixture %s is not on the allowlist (request.json, response.json, note.txt)", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking fixtures: %v", err)
	}
}

// TestRawPredictOKFixtureReplays pins the hand-authored fixture as a full
// exchange: the request Kno sends is byte-identical to the recorded request,
// and the response settles to the exact figures the note.txt promises.
func TestRawPredictOKFixtureReplays(t *testing.T) {
	t.Parallel()

	requestBody, err := os.ReadFile(filepath.Join("testdata", "fixtures", "rawpredict-ok", "request.json"))
	if err != nil {
		t.Fatalf("reading request.json: %v", err)
	}
	// Editors append a trailing newline; the transport's body does not.
	requestBody = bytes.TrimSuffix(requestBody, []byte("\n"))
	responseBody, err := os.ReadFile(filepath.Join("testdata", "fixtures", "rawpredict-ok", "response.json"))
	if err != nil {
		t.Fatalf("reading response.json: %v", err)
	}

	a, rec := newAgent(t, Options{MaxOutputTokens: 1024, System: "S"},
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(responseBody)
		})

	resp, err := a.Invoke(t.Context(), aCase("c1", "hello"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	sent := rec.body(t, 0)
	if !bytes.Equal(sent, requestBody) {
		t.Errorf("sent body differs from request.json:\n--- sent ---\n%s\n--- fixture ---\n%s", sent, requestBody)
	}

	if resp.Output != "hello from vertex" || resp.StopReason != knov1.StopReason_STOP_REASON_STOP || resp.Refused {
		t.Errorf("response = {Output %q, StopReason %v, Refused %v}", resp.Output, resp.StopReason, resp.Refused)
	}
	if resp.PromptTokens != 1600 || resp.CompletionTokens != 200 || resp.CachedTokens != 500 {
		t.Errorf("tokens = prompt %d completion %d cached %d, want 1600/200/500",
			resp.PromptTokens, resp.CompletionTokens, resp.CachedTokens)
	}
	if resp.CostUsdMicros <= 0 {
		t.Errorf("CostUsdMicros = %d, want > 0", resp.CostUsdMicros)
	}
}

// TestFixturesCarryNoKeyMaterial scans every fixture for anything that looks
// like a credential: AWS access keys, PEM private keys, JWT fragments, and
// service-account identities.
func TestFixturesCarryNoKeyMaterial(t *testing.T) {
	t.Parallel()

	root := filepath.Join("testdata", "fixtures")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, pattern := range []string{
			"AKIA[0-9A-Z]{16}",
			"wJalrXUtnFEMI", // the SigV4 test vector secret
			"-----BEGIN",    // PEM private keys
			"eyJ",           // JWT header fragments
			"AWS_ACCESS_KEY_ID=",
			"GOOGLE_APPLICATION_CREDENTIALS=",
			"client_email",
			"private_key",
		} {
			if strings.Contains(string(b), pattern) {
				t.Errorf("%s contains %q", path, pattern)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking fixtures: %v", err)
	}
}

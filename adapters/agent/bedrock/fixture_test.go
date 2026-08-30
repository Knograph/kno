package bedrock

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
			// Allowlisted. The anthropic adapter also allows a `status` file
			// and a `case.txt`; this adapter's fixtures are hand-authored and
			// pinned in the replay test instead.
		default:
			t.Errorf("fixture %s is not on the allowlist (request.json, response.json, note.txt)", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking fixtures: %v", err)
	}
}

// TestConverseOKFixtureReplays pins the hand-authored fixture as a full
// exchange: the request Kno sends is byte-identical to the recorded request,
// and the response settles to the exact figures the note.txt promises.
func TestConverseOKFixtureReplays(t *testing.T) {
	t.Parallel()

	requestBody, err := os.ReadFile(filepath.Join("testdata", "fixtures", "converse-ok", "request.json"))
	if err != nil {
		t.Fatalf("reading request.json: %v", err)
	}
	// Editors append a trailing newline; the transport's body does not.
	requestBody = bytes.TrimSuffix(requestBody, []byte("\n"))
	responseBody, err := os.ReadFile(filepath.Join("testdata", "fixtures", "converse-ok", "response.json"))
	if err != nil {
		t.Fatalf("reading response.json: %v", err)
	}

	model := "anthropic.claude-sonnet-4-5-20250929-v1:0"
	a, rec := newAgent(t, Options{Model: model, MaxOutputTokens: 1024, System: "S"},
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

	if resp.Output != "hello from bedrock" || resp.StopReason != knov1.StopReason_STOP_REASON_STOP || resp.Refused {
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
// like a credential: AWS access keys, AWS secret keys, and JWT fragments.
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
			"client_email", // a service-account key's identity
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

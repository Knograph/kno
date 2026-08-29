package vertex

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/knograph/kno/core/errs"
)

// TestResolveCreds walks the credential chain's rows.
func TestResolveCreds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		env      map[string]string
		readFile func(string) ([]byte, error)
		want     googleCreds
		wantErr  string
	}{
		{
			name:     "service account file wins and names the project",
			env:      map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": "/k/sa.json", "GOOGLE_CLOUD_REGION": "us-central1"},
			readFile: func(string) ([]byte, error) { return []byte(testSAJSON), nil },
			want: googleCreds{
				Project:     "kno-test-proj",
				Region:      "us-central1",
				PrivateKey:  testSAKey,
				ClientEmail: "kno-test@kno-test-proj.iam.gserviceaccount.com",
			},
		},
		{
			name:     "explicit project wins over the file's project_id",
			env:      map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": "/k/sa.json", "GOOGLE_CLOUD_REGION": "us-central1"},
			readFile: func(string) ([]byte, error) { return []byte(testSAJSON), nil },
			want: googleCreds{
				Project:     "explicit-proj",
				Region:      "us-central1",
				PrivateKey:  testSAKey,
				ClientEmail: "kno-test@kno-test-proj.iam.gserviceaccount.com",
			},
		},
		{
			name:     "env project and region without a file",
			env:      map[string]string{"GOOGLE_CLOUD_PROJECT": "env-proj", "GOOGLE_CLOUD_REGION": "europe-west1"},
			readFile: func(string) ([]byte, error) { t.Fatal("readFile must not run"); return nil, nil },
			want:     googleCreds{Project: "env-proj", Region: "europe-west1"},
		},
		{
			name:    "no credentials at all",
			env:     map[string]string{},
			wantErr: "GOOGLE_CLOUD_PROJECT is unset",
		},
		{
			name:    "project without a region",
			env:     map[string]string{"GOOGLE_CLOUD_PROJECT": "env-proj"},
			wantErr: "no region",
		},
		{
			name:     "file unreadable",
			env:      map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": "/k/sa.json"},
			readFile: func(string) ([]byte, error) { return nil, errors.New("no such file") },
			wantErr:  "could not be read",
		},
		{
			name:     "file is not service-account JSON",
			env:      map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": "/k/sa.json"},
			readFile: func(string) ([]byte, error) { return []byte("not json"), nil },
			wantErr:  "not a service-account JSON",
		},
		{
			name:     "wrong key type refused",
			env:      map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": "/k/sa.json"},
			readFile: func(string) ([]byte, error) { return []byte(`{"type":"authorized_user"}`), nil },
			wantErr:  "not service_account",
		},
		{
			name:     "key file without a key",
			env:      map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": "/k/sa.json"},
			readFile: func(string) ([]byte, error) { return []byte(`{"type":"service_account"}`), nil },
			wantErr:  "no private_key or client_email",
		},
		{
			name: "file without a project id and no env project",
			env:  map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": "/k/sa.json", "GOOGLE_CLOUD_REGION": "us-central1"},
			readFile: func(string) ([]byte, error) {
				noProject := strings.ReplaceAll(testSAJSON, `"project_id":"kno-test-proj",`, "")
				return []byte(noProject), nil
			},
			wantErr: "no project id",
		},
		{
			name:     "file without a region",
			env:      map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": "/k/sa.json"},
			readFile: func(string) ([]byte, error) { return []byte(testSAJSON), nil },
			wantErr:  "no region",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			project := tt.env["GOOGLE_CLOUD_PROJECT"]
			if tt.name == "explicit project wins over the file's project_id" {
				project = "explicit-proj"
			}
			got, err := resolveCreds(
				Options{Project: project},
				func(k string) string { return tt.env[k] },
				tt.readFile,
			)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveCreds succeeded, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("err = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveCreds: %v", err)
			}
			if got != tt.want {
				t.Errorf("creds = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestCredentialChainRefusalsNameTheLimits asserts the refusals say plainly
// what is NOT read: no gcloud application-default credentials, no metadata
// server, no ambient identity.
func TestCredentialChainRefusalsNameTheLimits(t *testing.T) {
	t.Parallel()

	_, err := resolveCreds(Options{}, func(string) string { return "" }, testReadFile)
	if err == nil {
		t.Fatal("resolveCreds succeeded with nothing set")
	}
	for _, want := range []string{
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_CLOUD_PROJECT",
		"GOOGLE_CLOUD_REGION",
		"gcloud application-default",
		"ADC metadata server",
		"GCE metadata server",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
}

// TestRedact asserts the key and the home directory never survive a
// diagnostic.
func TestRedact(t *testing.T) {
	t.Parallel()

	key := "-----BEGIN PRIVATE KEY-----\nMIIEvQ...SECRET...\n-----END PRIVATE KEY-----"
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	path := home + "/.config/gcloud"
	got := redact("key "+key+" path "+path, key)
	if strings.Contains(got, "SECRET") {
		t.Errorf("redact left the key in %q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Errorf("redact did not mask the key: %q", got)
	}
	if strings.Contains(got, home) {
		t.Errorf("redact left the home directory in %q", got)
	}
	if !strings.Contains(got, "~/.config/gcloud") {
		t.Errorf("redact did not elide the home path: %q", got)
	}
	if redact("") != "" {
		t.Error("redact of empty is not empty")
	}
}

// TestParsePrivateKey accepts both RSA encodings and refuses everything else.
func TestParsePrivateKey(t *testing.T) {
	t.Parallel()

	pkcs8, err := parsePrivateKey(testSAKey)
	if err != nil {
		t.Fatalf("PKCS#8: %v", err)
	}
	if pkcs8.N.BitLen() != 2048 {
		t.Errorf("key is %d bits, want 2048", pkcs8.N.BitLen())
	}

	for _, bad := range []string{"", "not a pem", "-----BEGIN EC PRIVATE KEY-----\nMA==\n-----END EC PRIVATE KEY-----"} {
		if _, err := parsePrivateKey(bad); err == nil {
			t.Errorf("parsePrivateKey(%q) succeeded", bad)
		}
	}
}

// TestNoRegionRefusal asserts the region requirement is ErrInvalidInput, not
// authentication: it is configuration, and the fix says so.
func TestNoRegionRefusal(t *testing.T) {
	t.Parallel()

	_, err := resolveCreds(
		Options{},
		func(k string) string {
			if k == "GOOGLE_CLOUD_PROJECT" {
				return "env-proj"
			}
			return ""
		},
		testReadFile,
	)
	if err == nil {
		t.Fatal("resolveCreds succeeded without a region")
	}
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("err = %v, want ErrInvalidInput", err)
	}
}

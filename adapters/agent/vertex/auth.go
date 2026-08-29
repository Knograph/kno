package vertex

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"strings"

	"github.com/knograph/kno/core/errs"
)

// This file resolves the credential chain, environment-only, and holds the
// service-account pieces the rest of the adapter needs. The rules are the
// plan's, spelled here:
//
//   - GOOGLE_APPLICATION_CREDENTIALS, when set, names a service-account JSON
//     file. The JSON is parsed for `private_key` and `client_email` ONLY —
//     nothing else in it is read, nothing about it is logged, and every
//     error that could carry key material runs it through redact().
//   - Otherwise the chain reads GOOGLE_CLOUD_PROJECT and the region.
//   - What is NOT read, and the refusal says so plainly: no gcloud
//     application-default credentials, no ADC metadata server, no ambient
//     GCE metadata — a run on a VM with a default service account gets a
//     refusal, not a silent ambient credential.

// googleCreds is everything the adapter needs from the credential chain.
//
// PrivateKey and ClientEmail come from the service-account JSON; Project and
// Region come from that file's project_id or the environment. The key is
// kept only long enough to sign JWTs, and never appears in an error, a log
// line, or a trace.
type googleCreds struct {
	Project     string
	Region      string
	PrivateKey  string
	ClientEmail string
}

// resolveCreds walks the environment-only credential chain.
//
// GOOGLE_APPLICATION_CREDENTIALS wins when set: the file names the service
// account AND the project, and a region env must still be present because the
// endpoint is regional. Without the file, GOOGLE_CLOUD_PROJECT and the region
// are required — there is no ambient fallback, by design (see the file godoc).
func resolveCreds(opts Options, getenv func(string) string, readFile func(string) ([]byte, error)) (googleCreds, error) {
	path := getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if path != "" {
		return fromServiceAccountJSON(path, opts.Project, getenv, readFile)
	}

	project := opts.Project
	if project == "" {
		project = getenv("GOOGLE_CLOUD_PROJECT")
	}
	region := getenv("GOOGLE_CLOUD_REGION")
	if project == "" {
		return googleCreds{}, ErrAuthentication.
			WithFix("set GOOGLE_APPLICATION_CREDENTIALS to the path of the service-account " +
				"JSON, or set GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_REGION — Kno reads " +
				"only these; it never falls back to gcloud application-default " +
				"credentials, the ADC metadata server, or the GCE metadata server").
			Wrap(fmt.Errorf("vertex: GOOGLE_CLOUD_PROJECT is unset"))
	}
	if region == "" {
		return googleCreds{}, errs.ErrInvalidInput.
			WithFix("set GOOGLE_CLOUD_REGION; the endpoint is regional and Kno never guesses a region").
			Wrap(fmt.Errorf("vertex: no region"))
	}
	return googleCreds{Project: project, Region: region}, nil
}

// fromServiceAccountJSON loads a service-account key file.
//
// The JSON is parsed for private_key and client_email only — a service-account
// file also carries client_id, token_uri, and a whole key-management surface
// this adapter does not use, and reading none of it is a security property,
// not a simplification. The private key leaves the file only into the JWT
// signer, and redact() masks it at every error-construction point.
func fromServiceAccountJSON(path string, projectHint string, getenv func(string) string, readFile func(string) ([]byte, error)) (googleCreds, error) {
	raw, err := readFile(path)
	if err != nil {
		return googleCreds{}, ErrAuthentication.
			WithFix(fmt.Sprintf("check %s and the file's permissions — Kno reads "+
				"only the service-account JSON's private_key and client_email, never "+
				"the rest of the file", redact(path))).
			Wrap(fmt.Errorf("vertex: GOOGLE_APPLICATION_CREDENTIALS points at %s, which "+
				"could not be read", redact(path)))
	}

	var sa struct {
		Type        string `json:"type"`
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal(raw, &sa); err != nil {
		return googleCreds{}, ErrAuthentication.
			WithFix("point GOOGLE_APPLICATION_CREDENTIALS at the service-account JSON " +
				"from the GCP console, not at a wrapper or a key in another format").
			Wrap(fmt.Errorf("vertex: %s is not a service-account JSON", redact(path)))
	}
	if sa.Type != "service_account" {
		return googleCreds{}, ErrAuthentication.
			WithFix("download the service-account key from the GCP console; Kno accepts " +
				"only the service_account key type").
			Wrap(fmt.Errorf("vertex: %s names type %q, not service_account", redact(path), sa.Type))
	}
	if sa.PrivateKey == "" || sa.ClientEmail == "" {
		return googleCreds{}, ErrAuthentication.
			WithFix("the file must carry private_key and client_email; check that it is " +
				"a complete service-account key").
			Wrap(fmt.Errorf("vertex: %s has no private_key or client_email", redact(path)))
	}

	region := getenv("GOOGLE_CLOUD_REGION")
	if region == "" {
		return googleCreds{}, errs.ErrInvalidInput.
			WithFix("set GOOGLE_CLOUD_REGION; the endpoint is regional and Kno never guesses a region").
			Wrap(fmt.Errorf("vertex: no region"))
	}

	project := projectHint
	if project == "" {
		project = sa.ProjectID
	}
	if project == "" {
		return googleCreds{}, errs.ErrInvalidInput.
			WithFix("the endpoint path carries the project id; set GOOGLE_CLOUD_PROJECT " +
				"or use a key file that names project_id").
			Wrap(fmt.Errorf("vertex: no project id"))
	}

	return googleCreds{
		Project:     project,
		Region:      region,
		PrivateKey:  sa.PrivateKey,
		ClientEmail: sa.ClientEmail,
	}, nil
}

// redact masks credential material that could have leaked into a diagnostic,
// and elides the home directory from paths.
//
// The private key is the credential. It must never appear in an error, a log
// line, or a trace; every error in this package whose cause could carry key
// material runs its input through redact() first, with the key itself as the
// secret. The path mask matters too: a path is environment shape a user may
// not want echoed in a shared log.
func redact(s string, secrets ...string) string {
	if s == "" {
		return s
	}
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		s = strings.ReplaceAll(s, home, "~")
	}
	return s
}

// parsePrivateKey turns a PEM service-account key into an RSA key.
//
// Google issues PKCS#8 ("PRIVATE KEY"); PKCS#1 ("RSA PRIVATE KEY") is parsed
// too because key-handling tools re-encode. Anything else — a passphrase, an
// EC key, a bare blob — is refused with the key itself masked.
func parsePrivateKey(pemBlock string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemBlock))
	if block == nil {
		return nil, fmt.Errorf("not a PEM private key")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, fmt.Errorf("the service-account key is not RSA")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	return nil, fmt.Errorf("the service-account key is not a supported RSA form")
}

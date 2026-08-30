package bedrock

import (
	"errors"
	"strings"
	"testing"

	"github.com/knograph/kno/core/errs"
)

// TestResolveCredsPinsTheRefusalRows exercises the credential chain edge by
// edge. The chain is deliberately short — three variables — so the rows are
// enumerable, and each refusal names the exact variable and the exact reason.
func TestResolveCredsPinsTheRefusalRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string

		// want is the sentinel the refusal must satisfy.
		want error
		// fixSubstr is a phrase the refusal's Fix must carry.
		fixSubstr string
		// causeSubstr is a phrase the wrapped cause must carry.
		causeSubstr string
	}{
		{
			name:        "no access key",
			env:         map[string]string{"AWS_SECRET_ACCESS_KEY": "x", "AWS_REGION": "us-east-1"},
			want:        ErrAuthentication,
			fixSubstr:   "never ~/.aws/credentials, profiles, SSO, or the instance metadata server",
			causeSubstr: "AWS_ACCESS_KEY_ID is unset",
		},
		{
			name:        "no secret key",
			env:         map[string]string{"AWS_ACCESS_KEY_ID": "x", "AWS_REGION": "us-east-1"},
			want:        ErrAuthentication,
			fixSubstr:   "never the shared credential files or SSO",
			causeSubstr: "AWS_SECRET_ACCESS_KEY is unset",
		},
		{
			name:        "no region",
			env:         map[string]string{"AWS_ACCESS_KEY_ID": "x", "AWS_SECRET_ACCESS_KEY": "y"},
			want:        ErrInvalidInput,
			fixSubstr:   "the endpoint is the regional bedrock-runtime host, and Kno never guesses a region",
			causeSubstr: "AWS_REGION is unset",
		},
		{
			name: "empty access key is a refusal, not a credential",
			env:  map[string]string{"AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "y", "AWS_REGION": "us-east-1"},
			want: ErrAuthentication,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveCreds(func(key string) string { return tc.env[key] })
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want errors.Is(err, %v)", err, tc.want)
			}
			var actionable *errs.Actionable
			if errors.As(err, &actionable) && tc.fixSubstr != "" {
				if fix := actionable.Fix; !strings.Contains(fix, tc.fixSubstr) {
					t.Errorf("fix %q does not name the refusal the way the contract demands (missing %q)", fix, tc.fixSubstr)
				}
			}
			if tc.causeSubstr != "" && !strings.Contains(err.Error(), tc.causeSubstr) {
				t.Errorf("error text %q does not name the variable", err)
			}
		})
	}
}

// TestResolveCredsHappyPath pins the exact fields the adapter signs with.
func TestResolveCredsHappyPath(t *testing.T) {
	t.Parallel()

	c, err := resolveCreds(testGetenv)
	if err != nil {
		t.Fatalf("resolveCreds: %v", err)
	}
	if c.accessKey != testCreds["AWS_ACCESS_KEY_ID"] {
		t.Errorf("accessKey = %q", c.accessKey)
	}
	if c.secretKey != testCreds["AWS_SECRET_ACCESS_KEY"] {
		t.Errorf("secretKey = %q", c.secretKey)
	}
	if c.region != "us-east-1" {
		t.Errorf("region = %q", c.region)
	}
	if c.sessionToken != "" {
		t.Errorf("sessionToken = %q, want empty", c.sessionToken)
	}
}

// TestResolveCredsSessionTokenIsOptional pins that a non-STS key stays
// token-free and an STS key is carried.
func TestResolveCredsSessionTokenIsOptional(t *testing.T) {
	t.Parallel()

	with := map[string]string{
		"AWS_ACCESS_KEY_ID":     "x",
		"AWS_SECRET_ACCESS_KEY": "y",
		"AWS_SESSION_TOKEN":     "AQoDYXdzEPT//////////wEXAMPLE",
		"AWS_REGION":            "eu-west-1",
	}
	c, err := resolveCreds(func(key string) string { return with[key] })
	if err != nil {
		t.Fatalf("resolveCreds: %v", err)
	}
	if c.sessionToken != "AQoDYXdzEPT//////////wEXAMPLE" {
		t.Errorf("sessionToken = %q", c.sessionToken)
	}
	if c.region != "eu-west-1" {
		t.Errorf("region = %q", c.region)
	}
}

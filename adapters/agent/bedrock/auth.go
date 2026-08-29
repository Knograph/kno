package bedrock

import (
	"fmt"
	"os"
)

// This file is the credential chain — and it is deliberately SHORT, because
// the chain itself is deliberately short. Bedrock is reached with exactly
// three environment variables: the access key, the secret key, and a region.
//
// What is NOT read is as load-bearing as what is: not ~/.aws/credentials, not
// the shared config file, not profiles, not SSO, not the IMDS metadata server,
// not any other ambient source. The AWS SDKs climb that chain in order, which
// means a developer laptop with a stale ~/.aws/credentials file authenticates
// as a DIFFERENT identity than the one Kno told the user it was using — a
// spend attribution that is wrong in a way that never errors. The refusal text
// says this plainly, because "set AWS_ACCESS_KEY_ID" is not a useful fix to
// someone whose environment already provides one of these.

// envCreds is the credential set the environment resolved to.
//
// sessionToken empty means a non-STS key: AWS_ACCESS_KEY_ID plus
// AWS_SECRET_ACCESS_KEY, no AWS_SESSION_TOKEN. A session token is the chain's
// only STS path, and its presence switches the signature to include the signed
// x-amz-security-token header.
type envCreds struct {
	accessKey    string
	secretKey    string
	sessionToken string
	region       string
}

// resolveCreds reads the credential environment and refuses what is missing.
//
// Every refusal names the exact variable and the exact reason, because the two
// honest failures look alike from the user's side: "AWS_ACCESS_KEY_ID is unset"
// and "AWS_SECRET_ACCESS_KEY is unset" need the same command to fix, but only
// one of them has been run.
func resolveCreds(getenv func(string) string) (envCreds, error) {
	var c envCreds
	c.accessKey = getenv("AWS_ACCESS_KEY_ID")
	c.secretKey = getenv("AWS_SECRET_ACCESS_KEY")
	c.sessionToken = getenv("AWS_SESSION_TOKEN")
	c.region = getenv("AWS_REGION")

	if c.accessKey == "" {
		return envCreds{}, ErrAuthentication.WithFix(
			"export AWS_ACCESS_KEY_ID with the access key of an AWS identity that " +
				"has Bedrock access; Kno reads only these variables — " +
				"AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN, " +
				"AWS_REGION — and never ~/.aws/credentials, profiles, SSO, or the " +
				"instance metadata server",
		).Wrap(fmt.Errorf("bedrock: AWS_ACCESS_KEY_ID is unset"))
	}
	if c.secretKey == "" {
		return envCreds{}, ErrAuthentication.WithFix(
			"export AWS_SECRET_ACCESS_KEY to go with AWS_ACCESS_KEY_ID; Kno reads " +
				"only the four AWS_* variables named above, and never the shared " +
				"credential files or SSO",
		).Wrap(fmt.Errorf("bedrock: AWS_SECRET_ACCESS_KEY is unset"))
	}
	if c.region == "" {
		return envCreds{}, ErrInvalidInput.WithFix(
			"export AWS_REGION, for example us-east-1; the endpoint is the " +
				"regional bedrock-runtime host, and Kno never guesses a region " +
				"from the credentials, the profile, or any metadata server",
		).Wrap(fmt.Errorf("bedrock: AWS_REGION is unset"))
	}
	return c, nil
}

// resolveCredsEnv is the os-backed binding, split from the pure function so
// the refusal rows are testable without mutating the process environment.
func resolveCredsEnv() (envCreds, error) { return resolveCreds(os.Getenv) }

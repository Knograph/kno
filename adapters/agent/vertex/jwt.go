package vertex

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/knograph/kno/adapters/agent/internal/agenterr"
	"github.com/knograph/kno/core/errs"
)

// This file is the JWT→access-token exchange: a signed JWT (RS256 with the
// service account's private key) posted to the token endpoint with
// grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer and scope
// cloud-platform, and the resulting access token cached until expiry.
//
// The signature construction is pinned by RFC 7515 A.2 — the published RS256
// known-answer — because a self round-trip catches nothing. The claims are
// pinned separately: the aud is the token endpoint, never a different host
// (see errors.go's refusal), and iat/exp bound the token's life.

// tokenURLs must agree with the exchange target, so the aud and the POST go
// to one spelling. tokenURL is declared in vertex.go; the aud is built from
// it, never spelled twice.
func tokenAud() string { return tokenURL }

// jwtHeader is the fixed JOSE header for the service-account exchange.
const jwtHeader = `{"alg":"RS256","typ":"JWT"}`

// jwtClaims is the assertion a service account signs. The bounds are exact
// for the token exchange: exp is one hour from iat (Google's own default
// lifetime for exchange JWTs), and a claim set with exp more than an hour
// out is refused by the endpoint.
type jwtClaims struct {
	Iss   string `json:"iss"`
	Scope string `json:"scope"`
	Aud   string `json:"aud"`
	Iat   int64  `json:"iat"`
	Exp   int64  `json:"exp"`
}

// signJWT builds and signs the assertion, RFC 7515 §3.1 shape.
func signJWT(key *rsa.PrivateKey, claims jwtClaims) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(jwtHeader))
	body, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("vertex: encoding the JWT claims: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(body)

	toSign := header + "." + payload
	digest := sha256.Sum256([]byte(toSign))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("vertex: signing the JWT: %w", err)
	}
	return toSign + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// tokenResponse is the token endpoint's 200.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// tokenError is the token endpoint's non-2xx, flat shape.
type tokenError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// exchangeToken posts a fresh JWT to the token endpoint.
//
// The endpoint is FIXED — the JWT is never retried against a different host
// — and every failure is refused with the endpoint named and the cause
// sanitized. The one failure worth naming specially is `invalid_grant`,
// which is what a skewing local clock produces: iat is in the future, or exp
// has already passed by the endpoint's clock, and the refusal says so rather
// than leaving a user to read OAuth's own laconic wording.
func (c *tokenCache) exchangeToken(ctx context.Context) (string, time.Time, error) {
	now := c.now()
	claims := jwtClaims{
		Iss:   c.creds.ClientEmail,
		Scope: "cloud-platform",
		Aud:   tokenAud(),
		Iat:   now.Unix(),
		Exp:   now.Add(time.Hour).Unix(),
	}
	key, err := parsePrivateKey(c.creds.PrivateKey)
	if err != nil {
		// The key must never reach the error. parsePrivateKey's message can
		// carry key material, so the redaction gets the key itself.
		return "", time.Time{}, ErrAuthentication.
			WithFix("re-download the service-account key from the GCP console; the " +
				"private_key must be an unencrypted RSA PEM block").
			Wrap(fmt.Errorf("vertex: the service-account key in %s is unusable: %s",
				redact(serviceAccountPathHint(c.creds)), redact(err.Error(), c.creds.PrivateKey)))
	}
	assertion, err := signJWT(key, claims)
	if err != nil {
		return "", time.Time{}, ErrAuthentication.
			Wrap(fmt.Errorf("vertex: signing the token request: %w", err))
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("vertex: building the token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := c.client
	res, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, errs.ErrTransportTransient.
			Wrap(fmt.Errorf("vertex: the token endpoint at %s did not answer: %w", tokenURL, err))
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", time.Time{}, errs.ErrTransportTransient.
			Wrap(fmt.Errorf("vertex: reading the token endpoint's answer: %w", err))
	}

	if res.StatusCode != http.StatusOK {
		var te tokenError
		_ = json.Unmarshal(body, &te)
		if te.Error == "invalid_grant" {
			// iat in the future, or exp already past — a local clock skewed
			// against Google's. The token endpoint is the honest arbiter of
			// the clock, and there is nothing to retry: the next minute's JWT
			// carries the same wrong clock until the machine's clock is fixed.
			// Run-fatal: the clock does not change mid-run, so every Case in
			// this run carries the same wrong iat. core reads this marker and
			// reports the refusal instead of "too many cases errored".
			return "", time.Time{}, agenterr.AsRunFatal(ErrAuthentication.
				WithFix("the token endpoint rejected the assertion's timestamps — " +
					"check this machine's clock (NTP), then retry; nothing else in " +
					"the request was wrong").
				Wrap(fmt.Errorf("vertex: the token endpoint at %s refused the "+
					"assertion with invalid_grant, which is what a local clock "+
					"skewed against Google's produces", tokenURL)))
		}
		return "", time.Time{}, ErrAuthentication.
			WithFix(fmt.Sprintf("check the service account and its permissions; the JWT is "+
				"signed and sent only to %s, never anywhere else", tokenURL)).
			Wrap(fmt.Errorf("vertex: the token endpoint at %s refused the "+
				"assertion: %s", tokenURL, sanitizeTe(te, string(body))))
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return "", time.Time{}, ErrMalformedResponse.
			Wrap(fmt.Errorf("vertex: the token endpoint at %s answered %d with "+
				"something that is not a token", tokenURL, res.StatusCode))
	}
	expires := now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	return tr.AccessToken, expires, nil
}

// sanitizeTe makes an OAuth error description safe to echo: it is provider
// text, and can quote parts of the assertion back.
func sanitizeTe(te tokenError, raw string) string {
	if te.ErrorDescription != "" {
		return te.Error + ": " + sanitize(te.ErrorDescription)
	}
	return sanitize(raw)
}

// tokenCache hands out access tokens, one live token at a time.
//
// Concurrent-safe: a stampede of Cases reaching the cache together must not
// produce a stampede of token exchanges. The first caller exchanges, the
// rest wait on the same result. A token is used until its expiry minus a
// safety margin; a mid-run expiry is the next exchange, which is a resumable
// stop, never a retry storm (the plan's STS rule, applied to OAuth).
type tokenCache struct {
	mu       sync.Mutex
	creds    googleCreds
	now      func() time.Time
	client   *http.Client
	token    string
	expires  time.Time
	exchange func(context.Context) (string, time.Time, error)
}

// newTokenCache builds the cache. The exchange function is a field so the
// auth tests can pin it without a network.
func newTokenCache(creds googleCreds, now func() time.Time) *tokenCache {
	c := &tokenCache{
		creds: creds,
		now:   now,
		// The token exchange reaches oauth2.googleapis.com directly, NOT
		// through the adapter's destination — the endpoint is fixed and
		// distinct. Its HTTP client is a plain one with the same policy
		// spirit: redirects are REFUSED, so an exchange can never be
		// replayed against a different host.
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return fmt.Errorf("vertex: the token endpoint at %s redirected, "+
					"and the JWT is never sent to another host", tokenURL)
			},
		},
	}
	c.exchange = c.exchangeToken
	return c
}

// bearer returns an Authorization value for a request, exchanging on miss.
//
// The transport calls this per request through its Authorize hook. The hook
// cannot return an error in the middle of signing a request that has already
// been built, so an exchange failure surfaces as the request error.
func (c *tokenCache) bearer(req *http.Request, body []byte) error {
	token, err := c.accessToken(ctxFrom(req))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// accessToken returns a live access token, exchanging when the cached one is
// absent or past its margin.
func (c *tokenCache) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expires.Add(-2*time.Minute)) {
		return c.token, nil
	}
	tok, exp, err := c.exchange(ctx)
	if err != nil {
		return "", err
	}
	c.token, c.expires = tok, exp
	return tok, nil
}

// ctxFrom recovers a context from a request, for the Authorize hook.
func ctxFrom(req *http.Request) context.Context {
	if req == nil || req.Context() == nil {
		return context.Background()
	}
	return req.Context()
}

// serviceAccountPathHint names where the key came from, without the key.
//
// The creds struct does not carry the path (the path is not a credential
// field, and carrying it invites it into errors). The hint is the email,
// which identifies the service account without exposing the key.
func serviceAccountPathHint(creds googleCreds) string {
	if creds.ClientEmail != "" {
		return creds.ClientEmail
	}
	return "GOOGLE_APPLICATION_CREDENTIALS"
}

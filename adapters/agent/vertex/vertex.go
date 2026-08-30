// Package vertex is the Agent adapter for Claude on Google Cloud Vertex AI's
// Anthropic-compatible :rawPredict endpoint.
//
// The transport differs from the `anthropic` scheme in the three ways that
// matter: the credential is a service account whose private key signs a JWT
// exchanged for an OAuth access token rather than a static API key, the
// project and region come from the environment rather than from a key
// binding, and the endpoint is FIXED — aiplatform.googleapis.com in the
// configured region — so no base URL is accepted or stored.
//
// The wire is the Anthropic Messages format with two named divergences
// (docs/plans/2026-08-29-bedrock-vertex-agents.md P0-1): `anthropic_version`
// travels in the BODY, not the `anthropic-version` header, and the model id
// is in the URL path, percent-encoded.
//
// The HTTP layer — host policy, redirect refusal, rate limiting, redaction —
// belongs to adapters/agent/internal/transport. Nothing here reimplements any
// of it.
package vertex

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/adapters/agent/internal/transport"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// Scheme is the agent-ref scheme this adapter serves. Taken from agentref
// rather than spelled again, so the parser and the adapter cannot disagree
// about what "vertex" is.
const Scheme = agentref.SchemeVertex

// tokenURL is the ONLY host the JWT→access-token exchange is ever sent to.
//
// The exchange is one POST to Google's token endpoint, and the refusal never
// retries it against a different host — see errors.go.
const tokenURL = "https://oauth2.googleapis.com/token"

// regionEndpoint builds the rawPredict endpoint root for a region.
//
// The host is derived from the region, and nothing else: the endpoint is
// where the request, the credential, and the pricing row meet, and every one
// of the three is fixed by the time a run starts. The private-address and
// plain-HTTP checks still apply to it, which is the enforcement that makes a
// misconfigured region fail loudly at construction rather than sending a
// bearer request to a host the user did not mean.
func regionEndpoint(region string) string {
	return "https://" + region + "-aiplatform.googleapis.com"
}

// rawPredictPath is the :rawPredict endpoint for a model, appended to the
// regional endpoint root.
//
// The model id sits in the PATH, percent-encoded: the `@` pin in a dated id
// ("claude-3-5-sonnet@20240620") must reach the router as %40 or the
// signature of the URL is a different URL than the one dialed. Slashes stay
// literal — a model id is one path segment, never a path.
func rawPredictPath(project, region, model string) string {
	return "/v1/projects/" + project + "/locations/" + region +
		"/publishers/anthropic/models/" + escapeModelID(model) + ":rawPredict"
}

// defaultWorstCasePromptBytes bounds the prompt term of WorstCase.
//
// Same bound as the sibling adapters: the models are the same family, so the
// context windows are the same. See that godoc for the reasoning.
const defaultWorstCasePromptBytes = 400_000

// maxWorstCasePromptBytes clamps MaxPromptBytes. See anthropic's same constant.
const maxWorstCasePromptBytes = 8 << 20 // 8 MiB

// samplingRemoved lists models that reject temperature, top_p, and top_k with
// a 400.
//
// The same family list the anthropic adapter carries, because the models are
// the same models — Vertex serves them under their plain ids. If the lists
// drift, one platform starts 400ing every Case while another works, and the
// failure reports as "too many cases errored" with nothing naming the cause.
var samplingRemoved = []string{
	"claude-fable-5",
	"claude-mythos-5",
	"claude-mythos-preview",
	"claude-opus-5",
	"claude-opus-4-8",
	"claude-opus-4-7",
	"claude-sonnet-5",
}

// Options configure an Agent.
//
// Deliberately plain Go types, like the sibling adapters — the shells that
// wrap this package cannot construct a transport.Destination. Note what is
// absent: no BaseURL, no KeyEnv, no AllowInsecureBaseURL, no
// AllowPrivateAddress. The endpoint is fixed by the region, the credential is
// fixed by the environment, and the opt-out flags that exist for
// user-configured endpoints do NOT exist here — a fixed cloud endpoint has no
// legitimate private or plain-HTTP spelling, and the refusal to offer a bypass
// is what keeps an env-misdirected region a loud construction error.
type Options struct {
	// Model is the Vertex model id VERBATIM: "claude-3-5-sonnet@20240620" or
	// the plain form "claude-sonnet-4-5". Required.
	Model string

	// Project is the GCP project id whose service account and endpoint are
	// used, when GOOGLE_APPLICATION_CREDENTIALS does not supply it. The
	// project in the endpoint path must be the project that owns the billing,
	// and the credential must be its service account; a mismatch 403s every
	// Case.
	Project string

	// MaxOutputTokens is the request's max_tokens. Required, for the same
	// reason the Messages API requires max_tokens: the estimate's output term
	// is unbounded without it.
	MaxOutputTokens int64

	// System is the system prompt sent with every Case.
	System string

	// Temperature is the sampling temperature, or nil to send none.
	// A pointer because 0 is a meaningful value and absent is what models in
	// samplingRemoved require. Refused at construction for a model that
	// rejects it, rather than 400ing every Case.
	Temperature *float64

	// Price overrides the table for this model, or nil to use the table.
	// Same contract as the sibling adapters': the override is the user's
	// assertion of their own contract, and the regional multiplier still
	// applies on top of it — the endpoint is regional either way.
	Price *knov1.Price

	// MaxPromptBytes overrides the prompt term of WorstCase. Zero uses
	// defaultWorstCasePromptBytes.
	MaxPromptBytes int64

	// Timeout bounds a single request. Zero uses the transport's default.
	Timeout time.Duration

	// UserAgent identifies Kno to the provider.
	UserAgent string

	// HTTPClient supplies the underlying client's TRANSPORT and TLS settings
	// only. Its redirect policy and timeout are overwritten by the transport,
	// always.
	HTTPClient *http.Client

	// endpointURL overrides the regional endpoint. Unexported, and only a test
	// seam: the exported surface offers no way to point this adapter
	// elsewhere, which is the point of a fixed endpoint.
	endpointURL string

	// getenv supplies the credential environment. Unexported test seam; the
	// production binding is os.Getenv.
	getenv func(string) string

	// readFile supplies the service-account JSON loader. Unexported test
	// seam; the production binding is os.ReadFile.
	readFile func(string) ([]byte, error)

	// now supplies the clock for the JWT's iat/exp and the token cache's
	// expiry. Unexported test seam.
	now func() time.Time
}

// Agent invokes Cases against Vertex AI's :rawPredict endpoint.
//
// Safe for concurrent use: everything it holds is read-only after New, and
// the transport underneath is an *http.Client.
type Agent struct {
	opts   Options
	creds  googleCreds
	tokens *tokenCache
	client *transport.Client
	base   string

	// asset is the injected Asset's content, empty on an Agent that carries
	// none. Set only by WithContext, and only on a COPY — the un-injected
	// receiver is the control arm of the measurement.
	asset string

	// worst is WorstCase, computed once. See the anthropic adapter's field.
	worst budget.Estimate
}

// New builds an Agent.
//
// Everything that can be refused is refused HERE, where the message is a
// startup error the user can read, rather than per Case, where the same
// mistake becomes a run of failures reported as "too many cases errored".
func New(opts Options) (*Agent, error) {
	if strings.TrimSpace(opts.Model) == "" {
		return nil, errs.ErrInvalidInput.
			WithFix("name a model, for example vertex:claude-3-5-sonnet@20240620").
			Wrap(fmt.Errorf("vertex: no model"))
	}
	if opts.MaxOutputTokens <= 0 {
		return nil, errs.ErrInvalidInput.
			WithFix("set --max-output-tokens; :rawPredict requires max_tokens and a cost cap cannot bound an unbounded output term").
			Wrap(fmt.Errorf("vertex: no output ceiling"))
	}
	if err := checkTemperature(opts.Model, opts.Temperature); err != nil {
		return nil, err
	}

	getenv := opts.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	readFile := opts.readFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	now := opts.now
	if now == nil {
		now = time.Now
	}
	creds, err := resolveCreds(opts, getenv, readFile)
	if err != nil {
		return nil, err
	}

	endpoint := opts.endpointURL
	if endpoint == "" {
		endpoint = regionEndpoint(creds.Region)
	}

	// A strict destination with an empty key: the static-key path no-ops and
	// the Authorize hook issues the bearer instead. The address and scheme
	// policy still apply in full, which is the "env-misdirected endpoint fails
	// loudly" guarantee — see regionEndpoint.
	dest, err := transport.NewDestination(endpoint, "", "", transport.Policy{})
	if err != nil {
		return nil, errs.ErrInvalidInput.
			WithFix(fmt.Sprintf("the region %q does not build a reachable "+
				"aiplatform.googleapis.com endpoint; check the region spelling",
				creds.Region)).
			Wrap(err)
	}

	tokens := newTokenCache(creds, now)

	client, err := transport.New(transport.Options{
		Dest:       dest,
		Timeout:    opts.Timeout,
		UserAgent:  opts.UserAgent,
		HTTPClient: opts.HTTPClient,
		Authorize:  tokens.bearer,
	})
	if err != nil {
		return nil, err
	}

	a := &Agent{
		opts: opts, creds: creds, tokens: tokens, client: client, base: endpoint,
	}
	a.worst = a.computeWorstCase()
	return a, nil
}

// checkTemperature refuses a sampling parameter a model will reject.
//
// Refused rather than dropped, for the same reason as the anthropic adapter:
// silently omitting it would run the measurement at the provider's default
// sampling and record temperature on the Run.
func checkTemperature(model string, t *float64) error {
	if t == nil {
		return nil
	}
	if *t < 0 || *t > 1 {
		return errs.ErrInvalidInput.
			WithFix("use a temperature between 0 and 1").
			Wrap(fmt.Errorf("vertex: temperature %v is outside the accepted range", *t))
	}
	if !generationParamsSupported(model) {
		return errs.ErrInvalidInput.
			WithFix("drop --temperature for this model; it controls reasoning depth through effort instead, and sending a sampling parameter would fail every Case with a 400").
			Wrap(fmt.Errorf("vertex: %s does not accept temperature", model))
	}
	return nil
}

// generationParamsSupported reports whether a model accepts sampling
// parameters.
//
// Vertex serves the same family under their plain ids, so the shared list
// matches directly — one list, all spellings.
func generationParamsSupported(model string) bool {
	for _, m := range samplingRemoved {
		if strings.HasPrefix(model, m) {
			return false
		}
	}
	return true
}

// Capabilities reports what this adapter supports.
//
// Static, never probed — see the anthropic adapter's godoc for why.
// Generation params are per model, not per adapter, for the same reason as
// the sibling adapters: a blanket temperature default would 400 every Case
// on a model that rejects it.
//
// There is NO seed capability. The Messages API has no seed parameter on
// Vertex's :rawPredict surface, and a false Capability would make the engine
// send a parameter the endpoint rejects per Case
// (docs/plans/2026-08-29-bedrock-vertex-agents.md P0-3).
func (a *Agent) Capabilities() *core.Capabilities {
	return &knov1.Capabilities{
		ContextInject:    true,
		KnowledgeWrite:   false,
		Stream:           false,
		TokenCounts:      true,
		GenerationParams: generationParamsSupported(a.opts.Model),
	}
}

// Model reports the model this Agent invokes, for the record a Run keeps.
func (a *Agent) Model() string { return a.opts.Model }

// BaseURL reports the endpoint root this Agent was built against.
func (a *Agent) BaseURL() string { return a.base }

// Region reports the GCP region the Agent reaches.
func (a *Agent) Region() string { return a.creds.Region }

// RoundTrips reports how many requests actually left this Agent.
//
// Counted at the RoundTripper rather than at Invoke, which is what makes it
// able to answer "did anything retry underneath the budget guard" — the
// question a per-call counter cannot.
func (a *Agent) RoundTrips() int64 { return a.client.RoundTrips() }

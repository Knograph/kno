// Package bedrock is the Agent adapter for Claude on AWS Bedrock's Converse
// API.
//
// The transport differs from the `anthropic` scheme in the three ways that
// matter: the request is signed with SigV4 rather than carrying a static key,
// the credential and region come from the environment rather than from a key
// binding, and the endpoint is FIXED — bedrock-runtime in the configured
// region — so no base URL is accepted or stored. Converse is also a different
// wire shape from the Messages API: `messages` are content-block arrays,
// inference settings live in `inferenceConfig`, the usage block is camelCase,
// and stop reasons are Converse's own vocabulary.
//
// The HTTP layer — host policy, redirect refusal, rate limiting, redaction —
// belongs to adapters/agent/internal/transport. Nothing here reimplements any
// of it.
package bedrock

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
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
// about what "bedrock" is.
const Scheme = agentref.SchemeBedrock

// serviceName is the SigV4 service this adapter signs for. The canonical URI,
// the credential scope, and the signing key all carry it.
const serviceName = "bedrock"

// regionEndpoint builds the Converse endpoint for a region.
//
// The host is derived from AWS_REGION, and nothing else: the endpoint is where
// the request, the credential, and the pricing row meet, and every one of the
// three is fixed by the time a run starts. The private-address and plain-HTTP
// checks still apply to it, which is the enforcement that makes a
// misconfigured AWS_REGION fail loudly at construction rather than sending a
// signed request to a host the user did not mean.
func regionEndpoint(region string) string {
	return "https://bedrock-runtime." + region + ".amazonaws.com"
}

// conversePath is the Converse endpoint for a model, appended to the regional
// endpoint root. The model id sits in the PATH, percent-encoded: an ARN model
// id carries colons, and the canonical URI and the wire URL must both spell
// them %3A or the signature never matches.
func conversePath(model string) string {
	return "/model/" + escapeModelID(model) + "/converse"
}

// defaultWorstCasePromptBytes bounds the prompt term of WorstCase.
//
// Same bound as the anthropic adapter: the models are the same family, so the
// context windows are the same. See that godoc for the reasoning.
const defaultWorstCasePromptBytes = 400_000

// maxWorstCasePromptBytes clamps MaxPromptBytes. See anthropic's same constant.
const maxWorstCasePromptBytes = 8 << 20 // 8 MiB

// samplingRemoved lists models that reject temperature, top_p, and top_k with
// a 400.
//
// The same family list the anthropic adapter carries, because the models are
// the same models — Bedrock serves them under an "anthropic." prefix. If the
// two lists drift, one platform starts 400ing every Case while the other
// works, and the failure reports as "too many cases errored" with nothing
// naming the cause.
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
	// Model is the Bedrock model id VERBATIM — a full resource path on
	// Bedrock, not a vendor name: "anthropic.claude-sonnet-4-5-20250929-v1:0",
	// or the full ARN form. Required.
	Model string

	// MaxOutputTokens is the request's maxTokens. Required, for the same
	// reason the Messages API requires max_tokens: the estimate's output term
	// is unbounded without it.
	MaxOutputTokens int64

	// System is the system prompt sent with every Case, as a top-level
	// `system` array.
	System string

	// Temperature is the sampling temperature, or nil to send none.
	// A pointer because 0 is a meaningful value and absent is what models in
	// samplingRemoved require. Refused at construction for a model that
	// rejects it, rather than 400ing every Case.
	Temperature *float64

	// Price overrides the table for this model, or nil to use the table.
	// Same contract as the anthropic adapter's: the override is the user's
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

	// now supplies the clock for the signature's date stamp. Unexported test
	// seam, for the skew-retry tests that must not depend on a live clock.
	now func() time.Time
}

// Agent invokes Cases against Bedrock's Converse API.
//
// Safe for concurrent use: everything it holds is read-only after New, and the
// transport underneath is an *http.Client.
type Agent struct {
	opts   Options
	creds  envCreds
	signer *signer
	client *transport.Client
	base   string

	// asset is the injected Asset's content, empty on an Agent that carries
	// none. Set only by WithContext, and only on a COPY — the un-injected
	// receiver is the control arm of the measurement.
	asset string

	// worst is WorstCase, computed once. See the anthropic adapter's field.
	worst budget.Estimate

	// retried marks the one allowed clock-skew retry. Per-Agent rather than
	// per-call: a skewing clock skews every call, and letting each Case burn
	// its own retry would be a per-Case version of the storm the plan forbids.
	// The second Case to hit the same skew gets the honest terminal error.
	// A pointer so the WithContext copy SHARES the budget — the two arms of a
	// measurement are one run, and the clock is one clock — and so the struct
	// copy in WithContext does not trip vet's copylocks.
	retried *atomic.Bool
}

// New builds an Agent.
//
// Everything that can be refused is refused HERE, where the message is a
// startup error the user can read, rather than per Case, where the same
// mistake becomes a run of failures reported as "too many cases errored".
func New(opts Options) (*Agent, error) {
	if strings.TrimSpace(opts.Model) == "" {
		return nil, errs.ErrInvalidInput.
			WithFix("name a model, for example bedrock:anthropic.claude-sonnet-4-5-20250929-v1:0").
			Wrap(fmt.Errorf("bedrock: no model"))
	}
	if opts.MaxOutputTokens <= 0 {
		return nil, errs.ErrInvalidInput.
			WithFix("set --max-output-tokens; Converse requires maxTokens and a cost cap cannot bound an unbounded output term").
			Wrap(fmt.Errorf("bedrock: no output ceiling"))
	}
	if err := checkTemperature(opts.Model, opts.Temperature); err != nil {
		return nil, err
	}

	getenv := opts.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	creds, err := resolveCreds(getenv)
	if err != nil {
		return nil, err
	}

	endpoint := opts.endpointURL
	if endpoint == "" {
		endpoint = regionEndpoint(creds.region)
	}

	// A strict destination with an empty key: the static-key path no-ops and
	// the Authorize hook signs instead. The address and scheme policy still
	// apply in full, which is the "env-misdirected endpoint fails loudly"
	// guarantee — see regionEndpoint.
	dest, err := transport.NewDestination(endpoint, "", "", transport.Policy{})
	if err != nil {
		return nil, errs.ErrInvalidInput.
			WithFix(fmt.Sprintf("AWS_REGION names %q, which does not build a "+
				"reachable bedrock-runtime endpoint; check the region spelling",
				creds.region)).
			Wrap(err)
	}

	s := newSigner(creds.accessKey, creds.secretKey, creds.sessionToken,
		creds.region, serviceName)
	if opts.now != nil {
		s.now = opts.now
	}

	client, err := transport.New(transport.Options{
		Dest:       dest,
		Timeout:    opts.Timeout,
		UserAgent:  opts.UserAgent,
		HTTPClient: opts.HTTPClient,
		Authorize:  s.sign,
	})
	if err != nil {
		return nil, err
	}

	a := &Agent{
		opts: opts, creds: creds, signer: s, client: client, base: endpoint,
		retried: &atomic.Bool{},
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
			Wrap(fmt.Errorf("bedrock: temperature %v is outside the accepted range", *t))
	}
	if !generationParamsSupported(model) {
		return errs.ErrInvalidInput.
			WithFix("drop --temperature for this model; it controls reasoning depth through effort instead, and sending a sampling parameter would fail every Case with a 400").
			Wrap(fmt.Errorf("bedrock: %s does not accept temperature", model))
	}
	return nil
}

// generationParamsSupported reports whether a model accepts sampling
// parameters.
//
// Bedrock spells the same family under an "anthropic." prefix, so the check
// strips the prefix and matches the shared list — one list, both spellings.
func generationParamsSupported(model string) bool {
	base := strings.TrimPrefix(model, "anthropic.")
	for _, m := range samplingRemoved {
		if strings.HasPrefix(base, m) {
			return false
		}
	}
	return true
}

// Capabilities reports what this adapter supports.
//
// Static, never probed — see the anthropic adapter's godoc for why.
// Generation params are per model, not per adapter, for the same reason as
// the anthropic adapter: a blanket temperature default would 400 every Case
// on a model that rejects it.
//
// There is NO seed capability. Converse's inferenceConfig has no seed
// parameter, and a false Capability would make the engine send a parameter
// the endpoint rejects per Case (docs/plans/2026-08-29-bedrock-vertex-agents.md
// P0-3). If a model family gains seed through additionalModelRequestFields,
// this reports it then — verified per model, never assumed.
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

// Region reports the AWS region the Agent signs for and reaches.
func (a *Agent) Region() string { return a.creds.region }

// RoundTrips reports how many requests actually left this Agent.
//
// Counted at the RoundTripper rather than at Invoke, which is what makes it
// able to answer "did anything retry underneath the budget guard" — the
// question a per-call counter cannot.
func (a *Agent) RoundTrips() int64 { return a.client.RoundTrips() }

var (
	_ core.Agent     = (*Agent)(nil)
	_ core.Capable   = (*Agent)(nil)
	_ core.Estimator = (*Agent)(nil)
)

// Package anthropic is the Agent adapter for Anthropic's Messages API.
//
// It is not the OpenAI-compatible adapter with a different base URL, and the
// difference is not cosmetic. The system prompt is a top-level field rather
// than a message; max_tokens is required rather than optional; the usage block
// counts cached input in separate fields, so the obvious reading of
// `input_tokens` under-reports a cached request by the whole cached prefix; and
// stop_reason carries a `refusal` value that must be scored rather than
// errored. Every one of those produces a subtly wrong NUMBER rather than a loud
// failure, which is why this is a second adapter and not a base-URL flag.
//
// The HTTP layer — host policy, credential binding, rate limiting, redirect
// refusal, redaction — belongs to adapters/agent/internal/transport. Nothing
// here reimplements any of it.
package anthropic

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/adapters/agent/internal/transport"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// Public constants a caller needs to configure this adapter.
const (
	// Scheme is the agent-ref scheme this adapter serves. Taken from agentref
	// rather than spelled again, so the parser and the adapter cannot disagree
	// about what "anthropic" is.
	Scheme = agentref.SchemeAnthropic

	// DefaultBaseURL is Anthropic's own endpoint.
	DefaultBaseURL = "https://api.anthropic.com"

	// DefaultKeyEnv names the environment variable holding the credential for
	// DefaultBaseURL's host, and ONLY for that host. A key bound to Anthropic
	// never travels to a base URL the user pointed elsewhere; see
	// transport.KeyBindings.
	DefaultKeyEnv = "ANTHROPIC_API_KEY"

	// KeyHeader is how the Messages API authenticates.
	//
	// Not Authorization. This is load-bearing beyond a header name: Go's
	// net/http strips Authorization, WWW-Authenticate, Cookie, and Cookie2 on a
	// cross-domain redirect and does NOT strip x-api-key, so a redirect would
	// forward this key verbatim. transport refuses redirects outright for
	// exactly that reason.
	KeyHeader = "x-api-key"

	// APIVersion is the anthropic-version header value this adapter is written
	// against. Pinned rather than tracking latest: the header is how Anthropic
	// keeps a response shape stable, and a run whose parsing changes underneath
	// it is a run whose numbers changed for no recorded reason.
	APIVersion = "2023-06-01"
)

// messagesPath is the endpoint, appended to the configured base URL.
const messagesPath = "/v1/messages"

// defaultWorstCasePromptBytes bounds the prompt term of WorstCase.
//
// Anthropic's current models carry a 1M-token context window; this is the
// 200k-token floor of the family expressed in the estimator's byte units at a
// deliberately low 2 bytes per token. It bounds PLANNING only — the consent
// prompt and the feasibility check — never a per-Case reservation, which
// Estimate computes from the Case itself.
//
// Over-stating it makes planning conservative: the consent prompt quotes a
// larger number and the feasibility check reduces concurrency. Under-stating it
// makes planning optimistic, which is how a run quotes $0.06 for $12.00 of
// exposure. A bound takes the recoverable direction, and MaxPromptBytes exists
// for a caller whose Cases are known to be small.
const defaultWorstCasePromptBytes = 400_000

// maxWorstCasePromptBytes clamps MaxPromptBytes. Far above any published
// context window, far below where building the placeholder would matter.
const maxWorstCasePromptBytes = 8 << 20 // 8 MiB

// samplingRemoved lists models that reject temperature, top_p, and top_k with a
// 400.
//
// Prefix-matched, because a provider appends dated suffixes and an exact match
// would silently permit temperature on every pinned version of a model that
// rejects it.
//
// This is why Capabilities.generation_params is answered per model rather than
// per adapter. A blanket temperature default would 400 EVERY Case, trip the
// error-rate threshold, and tell the user "too many cases errored for this to
// be a usable baseline" — a message naming nothing about the actual cause.
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
// Deliberately plain Go types. The transport is internal to adapters/agent, so
// a Destination or a KeyBindings in this struct would make the package
// unusable from cli, api, and tui — the shells that are supposed to be thin
// wrappers over one engine call.
type Options struct {
	// Model is the Anthropic model ID. Required.
	Model string

	// MaxOutputTokens is the request's max_tokens. Required, because the
	// Messages API requires it and because the estimate's output term is
	// unbounded without it — an unbounded output term is a cost cap that
	// cannot be enforced.
	MaxOutputTokens int64

	// BaseURL is the endpoint root. Empty uses DefaultBaseURL.
	BaseURL string

	// System is the system prompt sent with every Case, as a TOP-LEVEL field.
	System string

	// Temperature is the sampling temperature, or nil to send none.
	//
	// A pointer because 0 is the value a measurement run wants and "absent" is
	// what several current models require. Refused at construction for a model
	// that rejects it, rather than 400ing every Case.
	Temperature *float64

	// KeyEnv binds a host to the NAME of the environment variable holding its
	// credential — "api.example.com": "MY_KEY_VAR". The name of a variable is
	// not a secret; the key itself still only ever comes from the environment.
	//
	// A binding is required for any host other than DefaultBaseURL's:
	// DefaultKeyEnv applies to Anthropic's host and to nothing else, because
	// falling back to it would mail the user's Anthropic key to whatever host
	// they pointed --base-url at.
	KeyEnv map[string]string

	// AllowInsecureBaseURL permits a plain-HTTP base URL.
	AllowInsecureBaseURL bool

	// AllowPrivateAddress permits loopback and RFC1918 destinations, which is
	// what a local proxy is. Link-local is refused regardless.
	AllowPrivateAddress bool

	// MaxPromptBytes overrides the prompt term of WorstCase. Zero uses
	// defaultWorstCasePromptBytes.
	MaxPromptBytes int64

	// Timeout bounds a single request. Zero uses the transport's default.
	Timeout time.Duration

	// UserAgent identifies Kno to the provider.
	UserAgent string

	// HTTPClient supplies the underlying client's TRANSPORT and TLS settings
	// only. Its redirect policy and timeout are overwritten by the transport,
	// always — supplying a client must not be a way to disable cross-host
	// redirect refusal.
	HTTPClient *http.Client
}

// Agent invokes Cases against the Anthropic Messages API.
//
// Safe for concurrent use: everything it holds is read-only after New, and the
// transport underneath is an *http.Client.
type Agent struct {
	opts   Options
	client *transport.Client
	base   string

	// worst is WorstCase, computed once. Nothing about it depends on a Case,
	// and rebuilding the several-hundred-kilobyte placeholder prompt on every
	// planning call would be work for an answer that cannot change.
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
			WithFix("name a model, for example anthropic:claude-sonnet-4-6").
			Wrap(fmt.Errorf("anthropic: no model"))
	}
	if opts.MaxOutputTokens <= 0 {
		return nil, errs.ErrInvalidInput.
			WithFix("set --max-output-tokens; the Messages API requires max_tokens and a cost cap cannot bound an unbounded output term").
			Wrap(fmt.Errorf("anthropic: no output ceiling"))
	}
	if err := checkTemperature(opts.Model, opts.Temperature); err != nil {
		return nil, err
	}

	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	host, err := hostOf(opts.BaseURL)
	if err != nil {
		return nil, err
	}

	key, err := resolveKey(host, opts.KeyEnv)
	if err != nil {
		return nil, err
	}

	dest, err := transport.NewDestination(opts.BaseURL, key, KeyHeader, transport.Policy{
		AllowInsecureHTTP:   opts.AllowInsecureBaseURL,
		AllowPrivateAddress: opts.AllowPrivateAddress,
	})
	if err != nil {
		return nil, err
	}

	client, err := transport.New(transport.Options{
		Dest:       dest,
		Timeout:    opts.Timeout,
		UserAgent:  opts.UserAgent,
		HTTPClient: opts.HTTPClient,
		Headers:    http.Header{"Anthropic-Version": []string{APIVersion}},
	})
	if err != nil {
		return nil, err
	}

	a := &Agent{opts: opts, client: client, base: opts.BaseURL}
	a.worst = a.computeWorstCase()
	return a, nil
}

// checkTemperature refuses a sampling parameter a model will reject.
//
// Refused rather than dropped. Silently omitting it would run the measurement
// at the provider's default sampling and record temperature on the Run, so the
// report would claim a determinism the run did not have.
func checkTemperature(model string, t *float64) error {
	if t == nil {
		return nil
	}
	if *t < 0 || *t > 1 {
		return errs.ErrInvalidInput.
			WithFix("use a temperature between 0 and 1").
			Wrap(fmt.Errorf("anthropic: temperature %v is outside the accepted range", *t))
	}
	if !generationParamsSupported(model) {
		return errs.ErrInvalidInput.
			WithFix("drop --temperature for this model; it controls reasoning depth through effort instead, and sending a sampling parameter would fail every Case with a 400").
			Wrap(fmt.Errorf("anthropic: %s does not accept temperature", model))
	}
	return nil
}

// generationParamsSupported reports whether a model accepts sampling
// parameters.
func generationParamsSupported(model string) bool {
	for _, m := range samplingRemoved {
		if strings.HasPrefix(model, m) {
			return false
		}
	}
	return true
}

// hostOf extracts the host a base URL binds a credential to.
func hostOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Neither the URL nor the parse error is echoed: a malformed URL can
		// still carry a credential in its userinfo, and url.Parse quotes its
		// input back. transport.NewDestination refuses that shape with the same
		// reasoning; this runs first and must not undo it.
		return "", errs.ErrInvalidInput.
			WithFix("write --base-url as a full URL, for example https://api.anthropic.com").
			Wrap(fmt.Errorf("anthropic: the base URL has no host"))
	}
	return u.Host, nil
}

// resolveKey finds the credential bound to host.
//
// DefaultKeyEnv applies to Anthropic's own host and to no other. A user who
// points --base-url at a proxy binds a key for that host explicitly, because
// the alternative — falling back to ANTHROPIC_API_KEY — is a documented recipe
// that mails the user's key to a third party.
func resolveKey(host string, keyEnv map[string]string) (string, error) {
	// Sorted, so a refusal naming two bindings that normalize to one host reads
	// the same on every run rather than depending on map iteration order.
	pairs := make([]string, 0, len(keyEnv))
	for h, v := range keyEnv {
		pairs = append(pairs, h+"="+v)
	}
	sort.Strings(pairs)

	bindings, err := transport.ParseKeyBindings(pairs)
	if err != nil {
		return "", errs.ErrInvalidInput.
			WithFix("write each binding as --key-env host=ENV_VAR, naming the VARIABLE rather than the key").
			Wrap(err)
	}

	defaultHost, err := hostOf(DefaultBaseURL)
	if err != nil {
		return "", err
	}

	key, envVar := bindings.Resolve(host, defaultHost, DefaultKeyEnv)
	if key != "" {
		return key, nil
	}

	if envVar != "" {
		// A binding exists and the variable is empty. Distinguished from no
		// binding at all because the fixes differ: one is "export it", the
		// other is "bind it".
		return "", ErrAuthentication.
			WithFix(fmt.Sprintf("export %s; it is bound to %s but is empty", envVar, host)).
			Wrap(fmt.Errorf("anthropic: no credential for %s", host))
	}

	// A host that is not Anthropic's may legitimately need no credential — a
	// local proxy in front of the API is the documented case — so the absence
	// is only fatal for the host that certainly requires one.
	if host == defaultHost {
		return "", ErrAuthentication.Wrap(
			fmt.Errorf("anthropic: no credential for %s", host),
		)
	}
	return "", nil
}

// Capabilities reports what this adapter supports.
//
// Static, never probed. A probe costs money to learn something that changes
// monthly, and a probe failure is indistinguishable from an outage.
func (a *Agent) Capabilities() *core.Capabilities {
	return &knov1.Capabilities{
		// M2 does not inject. WithContext and WithKnowledge are the valuation
		// stage's surface and land with it; declaring them here would let a run
		// report a measurement mode it never used.
		ContextInject:  false,
		KnowledgeWrite: false,

		// Streaming is accepted debt, not an oversight — the plan's §11 records
		// it with a trigger. Declaring it would promise incremental output this
		// adapter cannot produce.
		Stream: false,

		// The Messages API reports usage on every successful call. Where it does
		// not, the individual Response carries usage_estimated rather than this
		// flag going false for the whole adapter: one representation, at the
		// granularity where the fact actually varies.
		TokenCounts: true,

		// Per model, not per adapter. See samplingRemoved.
		GenerationParams: generationParamsSupported(a.opts.Model),
	}
}

// Model reports the model this Agent invokes, for the record a Run keeps.
func (a *Agent) Model() string { return a.opts.Model }

// BaseURL reports the endpoint root this Agent was built against.
func (a *Agent) BaseURL() string { return a.base }

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

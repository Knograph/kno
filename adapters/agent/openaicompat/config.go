package openaicompat

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/adapters/agent/internal/transport"
	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// This file is everything that happens ONCE, at construction: what the adapter
// is pointed at, what credential may travel there, and which knobs the endpoint
// will accept. It is separate from the per-Case path in openaicompat.go because
// the two are read for different reasons — a reviewer checking where a key can
// go should not have to scroll past the response mapping to find out.

// Defaults. Every one is overridable; the values are what `openai:<model>` with
// no further configuration means.
const (
	// DefaultBaseURL is OpenAI's own endpoint.
	DefaultBaseURL = "https://api.openai.com/v1"

	// DefaultKeyEnv holds the credential for DefaultBaseURL's host, and ONLY
	// for that host. A model reached through another base URL needs its own
	// binding — see transport.KeyBindings — because falling back here would
	// mail the user's OpenAI key to a third party by following the documented
	// recipe.
	DefaultKeyEnv = "OPENAI_API_KEY"

	// DefaultMaxOutputTokens is the output ceiling when none is configured.
	//
	// It is not a neutral default. It is the output term of every reservation,
	// so raising it inflates what the guard holds; and it is the point at which
	// a long answer is truncated, so lowering it depresses the baseline through
	// STOP_REASON_LENGTH rather than through anything the agent did. Recorded
	// on the Run for exactly that reason.
	DefaultMaxOutputTokens = 1024

	// DefaultMaxPromptBytes bounds the prompt this adapter will send.
	//
	// It exists so WorstCase can be an upper bound rather than a guess: with a
	// ceiling on the prompt, "the most any Case could cost" is arithmetic
	// instead of speculation, and core can plan concurrency against a number
	// the guard will actually reserve. Without it, WorstCase would have to
	// invent a prompt size, and the failure it exists to prevent — a run that
	// denies its way to a halt with money unspent — would come back.
	//
	// 128 KiB is roughly 98k estimated tokens: a large Case by any measure. A
	// larger Asset is a legitimate thing to measure, so this is configurable —
	// with the trade stated, because raising it raises WorstCase, and a higher
	// WorstCase means the feasibility check plans fewer concurrent Cases.
	DefaultMaxPromptBytes = 128 << 10
)

// defaultHost is the host DefaultKeyEnv is bound to. Derived from
// DefaultBaseURL at init so the two cannot drift apart.
var defaultHost = mustHost(DefaultBaseURL)

// Options configure an Agent.
type Options struct {
	// Ref is the parsed agent reference. Required, and its scheme must be
	// `openai`.
	//
	// A parsed ref rather than a model string plus a URL string: agentref is
	// where the credential-in-a-URL and control-character refusals live, and a
	// second entry point that skipped them would be a second place for a key
	// to reach the Run record.
	Ref *core.AgentRef

	// KeyEnv maps a host to the NAME of the environment variable holding its
	// credential. The default host resolves through DefaultKeyEnv without a
	// binding; every other host needs one.
	//
	// It is map[string]string rather than transport.KeyBindings so that a
	// caller OUTSIDE adapters/agent can construct these Options at all. The
	// transport is an internal package by design — it is the security
	// boundary, and nothing above should be able to reach past it — which
	// meant the exported field typed by it made this whole struct
	// unconstructible from cli. anthropic.Options.KeyEnv has always had the
	// plain type for the same reason; this brings the two adapters into line.
	KeyEnv map[string]string

	// Policy is what the caller has opted into: plain HTTP, private addresses.
	// The zero value is the strictest one.
	AllowInsecureBaseURL bool

	// AllowPrivateAddress permits loopback and RFC1918 destinations, which is
	// what a local vLLM or Ollama endpoint is.
	//
	// Link-local is NOT covered and cannot be opted into: 169.254.169.254 is
	// the cloud instance-metadata endpoint, and a tool that fetches a URL and
	// persists the response body has no legitimate reason to reach it.
	AllowPrivateAddress bool

	// MaxOutputTokens is the output ceiling. Zero uses DefaultMaxOutputTokens.
	MaxOutputTokens int64

	// MaxPromptBytes bounds the assembled prompt. Zero uses the default. See
	// DefaultMaxPromptBytes for why a ceiling exists at all.
	MaxPromptBytes int

	// System is the system prompt sent with every Case, ahead of its history.
	System string

	// Temperature pins sampling. Nil sends none.
	//
	// Nil rather than 0 as "unset", because 0 is the value determinism wants
	// and a zero-valued float64 could not express the difference. Sending it to
	// a model that does not accept generation parameters is refused at
	// construction rather than discovered as a 400 on every Case.
	Temperature *float64

	// Seed pins the provider's sampler where it honors one. Nil sends none.
	Seed *int64

	// GenerationParams overrides the static capability matrix.
	//
	// Nil consults acceptsGenerationParams. Set it when a compatible endpoint
	// answers differently than the matrix assumes — the matrix is static by
	// design (a probe costs money to learn something that changes monthly, and
	// a probe failure is indistinguishable from an outage), so an override is
	// how a user corrects it without waiting for a release.
	GenerationParams *bool

	// UseLegacyMaxTokens sends `max_tokens` instead of `max_completion_tokens`.
	//
	// llama.cpp, LM Studio, and older self-hosted servers know only the legacy
	// spelling; OpenAI's reasoning models reject it. Neither default is right
	// for both, so the endpoint decides.
	UseLegacyMaxTokens bool

	// Price overrides the table for this model. Nil looks the model up.
	Price *knov1.Price

	// Timeout bounds a single request. Zero uses the transport's default.
	Timeout time.Duration

	// Limiter paces requests per host. Nil creates one. Share it across
	// adapters aimed at the same provider so one 429 slows all of them.
	Limiter *transport.Limiter

	// HTTPClient supplies transport and TLS settings only; its redirect policy
	// and timeout are always overwritten.
	HTTPClient *http.Client

	// UserAgent identifies Kno to the provider.
	UserAgent string

	// Now reads the clock, for measuring latency. Nil uses time.Now.
	//
	// A seam rather than a convenience. Latency is wall-clock by nature, so
	// asserting that it is measured at all — rather than reported as a constant
	// — needs a clock a test can drive. Without this the claim was untestable
	// and untested: replacing the measurement with zero broke nothing.
	Now func() time.Time
}

// New builds an Agent.
//
// Everything that can be refused is refused here, at configuration time, where
// a mistake is one readable message — rather than per Case, where the same
// mistake is a run of mysterious failures and a bill for the attempts.
func New(opts Options) (*Agent, error) {
	if opts.Ref == nil {
		return nil, fmt.Errorf("%w: openaicompat needs a parsed agent reference",
			errs.ErrInvalidInput)
	}
	if opts.Ref.GetScheme() != agentref.SchemeOpenAI {
		return nil, errs.ErrInvalidInput.WithFix(
			"write the reference as openai:<model>, adding @<base-url> for a " +
				"compatible provider",
		).
			Wrap(fmt.Errorf("openaicompat serves the %q scheme, not %q",
				agentref.SchemeOpenAI, opts.Ref.GetScheme()))
	}
	model := opts.Ref.GetTarget()
	if model == "" {
		return nil, fmt.Errorf("%w: the agent reference names no model", errs.ErrInvalidInput)
	}

	client, host, keyEnv, err := connect(opts)
	if err != nil {
		return nil, err
	}

	a := &Agent{
		client:    client,
		model:     model,
		host:      host,
		scheme:    opts.Ref.GetScheme(),
		keyEnv:    keyEnv,
		system:    opts.System,
		maxOutput: opts.MaxOutputTokens,
		maxPrompt: opts.MaxPromptBytes,
		legacyMax: opts.UseLegacyMaxTokens,
		now:       opts.Now,
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.maxOutput <= 0 {
		a.maxOutput = DefaultMaxOutputTokens
	}
	if a.maxPrompt <= 0 {
		a.maxPrompt = DefaultMaxPromptBytes
	}
	if err := a.checkCeilings(); err != nil {
		return nil, err
	}

	if err := a.applyGenerationParams(opts); err != nil {
		return nil, err
	}
	if err := a.applyPrice(opts); err != nil {
		return nil, err
	}
	return a, nil
}

// connect builds the transport, and is where the destination and the credential
// are decided.
//
// Split out of New because it is the security-relevant half: which host a
// request may reach and which key may travel there. It returns the host and the
// variable NAME the credential came from — never the credential — so a later
// error can say what was consulted without holding the secret to say it.
func connect(opts Options) (client *transport.Client, host, keyEnv string, err error) {
	baseURL := opts.Ref.GetBaseUrl()
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	host, err = hostOf(baseURL)
	if err != nil {
		return nil, "", "", err
	}

	// Resolved per host, never per scheme. `openai:llama-3.3-70b@groq` needs
	// GROQ_API_KEY; falling back to OPENAI_API_KEY would send the user's OpenAI
	// key to a third party, which is the threat the binding exists for.
	key, keyEnv := transport.KeyBindings(opts.KeyEnv).Resolve(host, defaultHost, DefaultKeyEnv)

	// An absent credential for the DEFAULT host is refused here, before any
	// request. It used to proceed: connect simply omitted the Authorization
	// header, so every Case made a real request and collected a 401 — which is
	// now run-fatal and therefore bounded, but still one paid round trip and a
	// message about the provider rejecting a credential that was never sent.
	//
	// Only for the default host. A self-hosted OpenAI-compatible server —
	// vLLM, Ollama, llama.cpp — legitimately needs no credential, and refusing
	// there would make the local-model path unreachable. anthropic draws the
	// same line for the same reason (anthropic.go:333). Closes docs/debt.md#57.
	if key == "" && host == defaultHost {
		fix := fmt.Sprintf("export %s", DefaultKeyEnv)
		if keyEnv != "" && keyEnv != DefaultKeyEnv {
			// A binding exists and the variable is empty. Distinguished from
			// no binding at all because the fixes differ: one is "export it",
			// the other is "bind it".
			fix = fmt.Sprintf("export %s; it is bound to %s but is empty", keyEnv, host)
		}
		return nil, "", "", errs.ErrInvalidInput.WithFix(fix).
			Wrap(fmt.Errorf("no credential for %s", host))
	}

	if key != "" {
		// Bearer is part of the header VALUE for this shape. The transport
		// attaches whatever it is given to the bound host and nothing else.
		key = "Bearer " + key
	}

	dest, err := transport.NewDestination(baseURL, key, "Authorization", transport.Policy{
		AllowInsecureHTTP:   opts.AllowInsecureBaseURL,
		AllowPrivateAddress: opts.AllowPrivateAddress,
	})
	if err != nil {
		return nil, "", "", err
	}
	client, err = transport.New(transport.Options{
		Dest:       dest,
		Limiter:    opts.Limiter,
		Timeout:    opts.Timeout,
		UserAgent:  opts.UserAgent,
		HTTPClient: opts.HTTPClient,
	})
	if err != nil {
		return nil, "", "", err
	}
	return client, host, keyEnv, nil
}

// applyPrice resolves the model's price, or leaves it absent.
//
// An unknown model is NOT an error here. With no cost cap a run against an
// unpriced model is legitimate — the call cap still applies — so the refusal
// belongs at Estimate, where core knows whether a dollar cap is set. Refusing
// at construction would make --max-cost-usd absent and present behave
// identically, which is the opposite of the rule.
//
// A price that cannot price IS an error here, and the distinction matters. A
// caller-supplied Price missing one of the two rates does not settle at
// nothing — it settles at the OTHER term alone, which is worse than a zero
// because it looks like a real number. costOf sums the input, cached, and
// output terms independently, so a nil input rate drops the input term and
// charges output only, and a nil output rate charges input only. Either way
// usage_estimated stays UNSET, so nothing downstream says the figure is partial
// and a report adds it into a total as if it were the invoice. That is the M1
// cap failure reached through the override flag rather than through the table.
func (a *Agent) applyPrice(opts Options) error {
	p := opts.Price
	if p == nil {
		if found, ok := pricing.Lookup(a.scheme, a.model); ok {
			p = found
		}
	}
	if p == nil {
		return nil // unpriced, and that is Estimate's refusal to make
	}
	if p.InputPerMtokUsdMicros == nil || p.OutputPerMtokUsdMicros == nil {
		return errs.ErrInvalidInput.WithFix(
			"give the price both an input and an output rate, or drop the override " +
				"and let the dated table answer",
		).
			Wrap(fmt.Errorf("the price supplied for %s is missing one of its two "+
				"rates, so a reply that reported usage would settle at the other "+
				"term alone rather than at its real cost", a.model))
	}
	a.price = p
	return nil
}

// applyGenerationParams settles whether temperature and seed may be sent.
//
// Refusing at construction rather than sending and hoping: OpenAI's reasoning
// models answer any non-default temperature with a 400, so a blanket default
// means every Case errors, ErrorRateExceeded fires, and the user is told "too
// many cases errored for this to be a usable baseline" — a message naming
// nothing about the cause. One refusal that names the model and the flag is
// strictly better than N identical 400s the user paid for.
func (a *Agent) applyGenerationParams(opts Options) error {
	accepts := acceptsGenerationParams(a.model)
	if opts.GenerationParams != nil {
		accepts = *opts.GenerationParams
	}
	a.genParams = accepts
	if accepts {
		a.temperature = opts.Temperature
		a.seed = opts.Seed
		return nil
	}
	if opts.Temperature != nil || opts.Seed != nil {
		return errs.ErrCapabilityUnsupported.WithFix(fmt.Sprintf(
			"drop --temperature and --seed for %s, or pass "+
				"--generation-params if this endpoint accepts them", a.model,
		)).
			Wrap(fmt.Errorf("%s rejects sampling parameters, so sending one would "+
				"fail every Case with a 400 rather than pin determinism", a.model))
	}
	return nil
}

// reasoningModelPrefixes name the models that reject sampling parameters.
//
// A static matrix, not a probe: a probe costs money to learn something that
// changes monthly, and a probe failure is indistinguishable from an outage.
// Prefix-matched because providers append dated suffixes, and an exact match
// would silently treat every pinned version as if it accepted parameters it
// does not.
//
// The matrix is keyed on the MODEL NAME ALONE and never on the base URL. That
// is a limitation, not a design: a self-hosted server behind a base URL will
// happily honour temperature on a model it has named `gpt-5-whatever`, and this
// refuses it. The user's remedy is Options.GenerationParams, which is why the
// override exists.
//
// Keying on the base URL was considered and rejected. "Not the default host, so
// assume it accepts" is a guess in the direction that costs money — every Case
// 400s and the user is told the error rate is too high — and "not the default
// host, so assume it refuses" would break every gateway that proxies OpenAI
// models faithfully. Neither guess is better than a name-based matrix the user
// can override, and a comment claiming a base-URL rule that the code does not
// implement is worse than both.
var reasoningModelPrefixes = []string{"o1", "o3", "o4", "gpt-5"}

func acceptsGenerationParams(model string) bool {
	for _, p := range reasoningModelPrefixes {
		if strings.HasPrefix(model, p) {
			return false
		}
	}
	return true
}

// hostOf extracts the host[:port] a base URL names.
func hostOf(raw string) (string, error) {
	u, err := parseBase(raw)
	if err != nil {
		// The URL is not echoed: a malformed one can still carry a credential,
		// and url.Parse quotes its input back.
		return "", fmt.Errorf("%w: the base URL could not be parsed", errs.ErrInvalidInput)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: the base URL has no host", errs.ErrInvalidInput)
	}
	return u.Host, nil
}

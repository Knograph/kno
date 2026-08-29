package cli

import (
	"fmt"
	"math"
	"net/url"
	"strings"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/adapters/agent/anthropic"
	"github.com/knograph/kno/adapters/agent/bedrock"
	"github.com/knograph/kno/adapters/agent/exec"
	"github.com/knograph/kno/adapters/agent/fake"
	"github.com/knograph/kno/adapters/agent/openaicompat"
	"github.com/knograph/kno/adapters/agent/vertex"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Turning flags into a provider adapter.
//
// This file is where `kno baseline` becomes able to spend money, so it is the
// milestone's second security boundary after the transport itself. Two rules
// shape all of it:
//
//   - There is exactly ONE parser for an endpoint. agentref.Parse holds the
//     credential-in-a-URL, control-character, and scheme refusals, and every
//     way of naming an endpoint composes into a ref before it is parsed.
//     openaicompat.Options.Ref's godoc says why: a second entry point that
//     skipped them would be a second place for a key to reach the Run record.
//
//   - A credential is never a flag VALUE. --key-env names an environment
//     variable. A key on a command line lands in shell history, in ps output,
//     and in CI logs, and no amount of redaction downstream takes it back.

// composeRef folds --base-url into the agent ref.
//
// Composed rather than passed alongside, so agentref.Parse is the only
// validator. Two failure modes it must not have, both found in Phase-1 review
// before any of this existed:
//
//   - `--agent openai:m@https://a/v1` AND `--base-url https://b/v1`.
//     splitBaseURL takes the first `@` whose remainder is absolute, so naive
//     concatenation yields a base URL of "https://a/v1@https://b/v1" — host a,
//     path "/v1@https:/b/v1", no error. The run goes to a with a garbage path
//     and --base-url is silently ignored. Refused rather than resolved by
//     precedence: two ways of saying the same thing, disagreeing, is a
//     mistake, not an ordering question.
//
//   - A --base-url that is not absolute http(s). "h/v1" composes to
//     "openai:m@h/v1"; splitBaseURL finds no absolute URL, so the flag is
//     absorbed into the MODEL NAME and the provider answers 404 "check the
//     model name". A one-slash typo is worse: "https:/h" is refused with "puts
//     a URL where the model belongs. A base URL is introduced by `@`" — a
//     message about `@` for a user who never typed one.
func composeRef(agentRef, baseURL string) (string, error) {
	if baseURL == "" {
		return agentRef, nil
	}
	if _, existing := splitAt(agentRef); existing != "" {
		return "", errs.ErrInvalidInput.WithFix(
			"pass the endpoint once — either as @<url> inside --agent, or as " +
				"--base-url, not both",
		).
			// Redacted. A ref carrying userinfo is refused by agentref with a
			// non-echoing message — but this refusal happens FIRST, so
			// without this the credential reaches stderr and the CI log,
			// which is the leak the userinfo refusal exists to prevent
			// arriving one layer earlier.
			Wrap(fmt.Errorf("--agent already names a base URL (%s) and --base-url "+
				"names another", agentref.Redact(existing)))
	}
	if err := checkAbsoluteHTTP(baseURL); err != nil {
		return "", err
	}
	return agentRef + "@" + baseURL, nil
}

// splitAt reports a base URL already present in a ref, using the same
// first-absolute-URL rule agentref does.
//
// It must agree with agentref.splitBaseURL EXACTLY, and the first version did
// not: it matched the scheme case-sensitively and without trimming, while
// agentref lowercases and trims first. So `--agent openai:m@HTTPS://evil/v1`
// plus `--base-url https://good/v1` slipped past the double-endpoint refusal,
// composed to a single ref, and agentref then took the SECOND URL as the base
// — the CLI validated good and the adapter dialled evil. That divergence
// defeats the entire reason --base-url is composed rather than passed
// alongside, which is that there is one parser.
//
// The transport still applies its address policy to whatever host is really
// dialled, so the consequence was a silently wrong endpoint rather than a
// bypassed refusal — but the guarantee this function exists to provide was
// gone.
func splitAt(ref string) (rest, baseURL string) {
	for i, c := range ref {
		if c != '@' {
			continue
		}
		// Trimmed before testing AND before returning, matching
		// agentref.splitBaseURL byte for byte.
		candidate := strings.TrimSpace(ref[i+1:])
		if isAbsoluteHTTP(candidate) {
			return ref[:i], candidate
		}
	}
	return ref, ""
}

// isAbsoluteHTTP normalizes the way agentref.splitBaseURL does before testing.
func isAbsoluteHTTP(s string) bool {
	v := strings.ToLower(s)
	return strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://")
}

// checkAbsoluteHTTP refuses a --base-url that is not an endpoint root, naming
// THE FLAG rather than the `@` form the user did not type.
func checkAbsoluteHTTP(raw string) error {
	fix := "write --base-url as a full URL including the scheme, " +
		"like https://api.example.com/v1"

	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		// Not echoed: a malformed URL may still contain a credential, and the
		// parse error quotes what it choked on.
		return errs.ErrInvalidInput.WithFix(fix).
			Wrap(fmt.Errorf("--base-url could not be parsed as a URL"))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errs.ErrInvalidInput.WithFix(fix).
			Wrap(fmt.Errorf("--base-url has no http or https scheme"))
	}
	if u.Host == "" {
		return errs.ErrInvalidInput.WithFix(fix).
			Wrap(fmt.Errorf("--base-url names no host"))
	}
	return nil
}

// resolveAgent turns flags into an Agent.
//
// Parse and resolve stay separate steps because they answer different
// questions. Parsing asks whether the reference is well formed; resolving asks
// whether an adapter exists for it. Merging them makes a typo and an
// unsupported provider produce the same message, and the user cannot tell which
// one they have.
func resolveAgent(f baselineFlags) (core.Agent, *knov1.AgentRef, error) {
	// Refused before composeRef: exec: has no endpoint, so composing --base-url
	// into the ref would silently absorb the URL into the COMMAND and the
	// failure would be a confusing "command not found" — the silent-ignore
	// shape this file exists to refuse.
	if f.baseURL != "" && hasExecScheme(f.agentRef) {
		return nil, nil, errs.ErrInvalidInput.WithFix(
			"drop --base-url; exec: runs a local command and has no endpoint",
		).
			Wrap(fmt.Errorf("--agent names the exec scheme and --base-url names an endpoint"))
	}

	composed, err := composeRef(f.agentRef, f.baseURL)
	if err != nil {
		return nil, nil, err
	}

	parsed, err := agentref.Parse(composed)
	if err != nil {
		return nil, nil, errs.ErrInvalidInput.WithFix(
			"write the reference as scheme:target — openai:gpt-4.1, " +
				"anthropic:claude-opus-5, bedrock:anthropic.claude-sonnet-4-5-20250929-v1:0, " +
				"vertex:claude-sonnet-4-5, exec:my-agent-command, or fake: for " +
				"the local agent that costs nothing",
		).Wrap(err)
	}

	switch parsed.GetScheme() {
	case agentref.SchemeFake:
		return fake.New(fake.Options{}), parsed, nil
	case agentref.SchemeOpenAI:
		a, err := newOpenAICompat(f, parsed)
		return a, parsed, err
	case agentref.SchemeAnthropic:
		a, err := newAnthropic(f, parsed)
		return a, parsed, err
	case agentref.SchemeBedrock:
		a, err := newBedrock(f, parsed)
		return a, parsed, err
	case agentref.SchemeVertex:
		a, err := newVertex(f, parsed)
		return a, parsed, err
	case agentref.SchemeExec:
		a, err := newExec(f, parsed)
		return a, parsed, err
	}

	return nil, nil, errs.ErrCapabilityUnsupported.WithFix(
		"use openai:, anthropic:, bedrock:, vertex:, fake:, or exec:; tuned: " +
			"lands with the tuner",
	).
		Wrap(fmt.Errorf("no adapter for agent ref %q", parsed.GetRef()))
}

// hasExecScheme reports whether the agent ref names the exec scheme, using
// the same first-colon rule agentref does.
func hasExecScheme(ref string) bool {
	before, _, ok := strings.Cut(ref, ":")
	return ok && strings.TrimSpace(before) == agentref.SchemeExec
}

// newExec builds the shell-command adapter.
//
// The command is the ref target, the string after "exec:". Grants and the
// per-call deadline pass through; the cost override is the price of one
// invocation (zero declares the command free, which is what makes the
// consent path ask nothing of an exec run).
func newExec(f baselineFlags, ref *knov1.AgentRef) (core.Agent, error) {
	return exec.New(exec.Options{
		Command:              ref.GetTarget(),
		Env:                  f.execEnv,
		Timeout:              f.timeout,
		CostPerCallUSDMicros: usdToMicros(f.costPerCall),
	})
}

// newOpenAICompat builds the OpenAI-shaped adapter.
func newOpenAICompat(f baselineFlags, ref *knov1.AgentRef) (core.Agent, error) {
	bindings, err := keyBindings(f.keyEnv)
	if err != nil {
		return nil, err
	}
	genParams, err := generationParams(f.generationParams)
	if err != nil {
		return nil, err
	}
	price, err := priceOverride(f)
	if err != nil {
		return nil, err
	}

	return openaicompat.New(openaicompat.Options{
		Ref:                  ref,
		KeyEnv:               bindings,
		AllowInsecureBaseURL: f.allowInsecureURL,
		AllowPrivateAddress:  f.allowPrivateAddress,

		MaxOutputTokens:    f.maxOutputTokens,
		MaxPromptBytes:     intFromInt64(f.maxPromptBytes),
		System:             f.system,
		Temperature:        optionalFloat(f.temperature),
		Seed:               optionalInt(f.seed, f.seedSet),
		GenerationParams:   genParams,
		UseLegacyMaxTokens: f.useLegacyMaxTokens,
		Price:              price,
		Timeout:            f.timeout,
	})
}

// newAnthropic builds the Messages-API adapter.
func newAnthropic(f baselineFlags, ref *knov1.AgentRef) (core.Agent, error) {
	bindings, err := keyBindings(f.keyEnv)
	if err != nil {
		return nil, err
	}
	price, err := priceOverride(f)
	if err != nil {
		return nil, err
	}

	return anthropic.New(anthropic.Options{
		Model:                ref.GetTarget(),
		BaseURL:              ref.GetBaseUrl(),
		Price:                price,
		MaxOutputTokens:      f.maxOutputTokens,
		MaxPromptBytes:       f.maxPromptBytes,
		System:               f.system,
		Temperature:          optionalFloat(f.temperature),
		KeyEnv:               bindings,
		AllowInsecureBaseURL: f.allowInsecureURL,
		AllowPrivateAddress:  f.allowPrivateAddress,
		Timeout:              f.timeout,
	})
}

// checkFixedEndpointFlags refuses the flags that exist for user-configured
// endpoints.
//
// The partner-cloud adapters have no BaseURL, no KeyEnv, and no endpoint
// bypass: the endpoint is fixed by the region, the credential is the
// environment chain, and the opt-out flags that exist for openai-compat
// endpoints have no legitimate spelling here. Refused at the CLI rather than
// silently dropped, because a flag that parses but does nothing is a run that
// measures the wrong thing — and a --key-env carrying a key VALUE is the leak
// this file exists to prevent.
//
// --base-url is NOT refused here: agentref.Parse owns that refusal, so there
// is exactly one place that decides whether a scheme takes an endpoint.
func checkFixedEndpointFlags(f baselineFlags, scheme string) error {
	if len(f.keyEnv) > 0 {
		return errs.ErrInvalidInput.WithFix(
			"drop --key-env; " + scheme + " authenticates with the environment " +
				"credential chain, not a key binding",
		).
			Wrap(fmt.Errorf("--key-env does not apply to the %s scheme", scheme))
	}
	if f.allowInsecureURL || f.allowPrivateAddress {
		return errs.ErrInvalidInput.WithFix(
			"drop --allow-insecure-base-url and --allow-private-address; the " +
				scheme + " endpoint is a fixed cloud endpoint with no legitimate " +
				"private or plain-HTTP spelling",
		).
			Wrap(fmt.Errorf("the endpoint bypass flags do not apply to the %s scheme", scheme))
	}
	if f.seedSet {
		return errs.ErrInvalidInput.WithFix(
			"drop --seed; " + scheme + " has no seed parameter on its wire formats, " +
				"and sending one would fail every Case",
		).
			Wrap(fmt.Errorf("--seed does not apply to the %s scheme", scheme))
	}
	return nil
}

// newBedrock builds the Converse adapter.
//
// The model id is the ref target VERBATIM — on Bedrock a model id is a full
// resource path, never a vendor name. The endpoint is fixed by AWS_REGION; the
// flags that point an endpoint elsewhere are refused above.
func newBedrock(f baselineFlags, ref *knov1.AgentRef) (core.Agent, error) {
	if err := checkFixedEndpointFlags(f, agentref.SchemeBedrock); err != nil {
		return nil, err
	}
	price, err := priceOverride(f)
	if err != nil {
		return nil, err
	}
	return bedrock.New(bedrock.Options{
		Model:           ref.GetTarget(),
		MaxOutputTokens: f.maxOutputTokens,
		System:          f.system,
		Temperature:     optionalFloat(f.temperature),
		Price:           price,
		MaxPromptBytes:  f.maxPromptBytes,
		Timeout:         f.timeout,
	})
}

// newVertex builds the :rawPredict adapter.
//
// The model id is the ref target verbatim — "claude-3-5-sonnet@20240620" and
// plain "claude-sonnet-4-5" both exist and are both accepted. The project and
// region come from the credential chain, never from flags: the endpoint path
// and the signature must agree about who pays.
func newVertex(f baselineFlags, ref *knov1.AgentRef) (core.Agent, error) {
	if err := checkFixedEndpointFlags(f, agentref.SchemeVertex); err != nil {
		return nil, err
	}
	price, err := priceOverride(f)
	if err != nil {
		return nil, err
	}
	return vertex.New(vertex.Options{
		Model:           ref.GetTarget(),
		MaxOutputTokens: f.maxOutputTokens,
		System:          f.system,
		Temperature:     optionalFloat(f.temperature),
		Price:           price,
		MaxPromptBytes:  f.maxPromptBytes,
		Timeout:         f.timeout,
	})
}

// keyBindings turns repeated --key-env flags into the plain map the adapters
// take.
//
// SHAPE ONLY. What a valid binding IS — the host normalization, the
// looks-like-a-secret refusal, the bound-twice refusal — belongs to
// transport.ParseKeyBindings, and each adapter runs its map through it. An
// earlier version of this function reimplemented those rules here because the
// transport is an internal package, and the copy was wrong in the way copies
// are: its key-prefix denylist missed `gsk_`, which is Groq, which is the
// worked example in this project's own cookbook. A user who pasted the key
// instead of the variable name had it accepted onto the command line.
//
// The VALUE is a variable name, never a key. That is the whole point of the
// flag: a key on a command line is written to shell history, shown in ps
// output, and captured in CI logs, and nothing downstream can take it back.
func keyBindings(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		host, envVar, ok := strings.Cut(p, "=")
		if !ok || host == "" || envVar == "" {
			// The pair is NOT echoed. A user who put a key where a variable
			// name belongs would otherwise see it in the error, which reaches
			// stderr and therefore CI logs — turning a near-miss into the leak
			// the flag exists to prevent.
			return nil, errs.ErrInvalidInput.WithFix(
				"write each binding as --key-env host=VAR, naming the " +
					"environment VARIABLE rather than the key itself",
			).
				Wrap(fmt.Errorf("a --key-env binding is not in host=VAR form"))
		}
		out[host] = envVar
	}
	return out, nil
}

// generationParams turns the tri-state flag into the adapter's optional bool.
func generationParams(v string) (*bool, error) {
	switch v {
	case "", "auto":
		return nil, nil
	case "on":
		t := true
		return &t, nil
	case "off":
		fal := false
		return &fal, nil
	}
	return nil, errs.ErrInvalidInput.
		WithFix("pass --generation-params auto, on, or off").
		Wrap(fmt.Errorf("--generation-params is %q", v))
}

// priceOverride builds a Price from the per-token flags.
//
// A PAIR, refused unless both are given: EstimateWithPrice needs both terms,
// and half a price silently produces an estimate that is wrong in the
// direction that under-reserves — which is a cap that does not bind.
func priceOverride(f baselineFlags) (*knov1.Price, error) {
	in, out := f.priceInPerMTok, f.priceOutPerMTok
	switch {
	case in == 0 && out == 0:
		return nil, nil
	case in <= 0 || out <= 0:
		return nil, errs.ErrInvalidInput.WithFix(
			"pass both --price-input-per-mtok and --price-output-per-mtok, " +
				"each above zero",
		).
			Wrap(fmt.Errorf("a price needs an input and an output rate; got %.4f and %.4f",
				in, out))
	}
	inMicros, outMicros := usdToMicros(in), usdToMicros(out)
	return &knov1.Price{
		InputPerMtokUsdMicros:  &inMicros,
		OutputPerMtokUsdMicros: &outMicros,
	}, nil
}

// intFromInt64 narrows a byte ceiling without wrapping on a 32-bit build.
//
// A fat-fingered --max-prompt-bytes past MaxInt would otherwise become a small
// or negative ceiling, which refuses every Case rather than none.
func intFromInt64(v int64) int {
	if v > int64(math.MaxInt32) {
		return math.MaxInt32
	}
	if v < 0 {
		return 0
	}
	return int(v)
}

// optionalFloat returns nil for an unset --temperature.
//
// NaN is the sentinel because zero is a LEGITIMATE temperature — the one that
// makes a run reproducible — so a zero-value default would silently pin every
// run to greedy decoding and call it the provider's choice.
func optionalFloat(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}

// optionalInt returns nil for an unset --seed.
//
// Zero is a LEGITIMATE seed, so it is passed through when the flag was given
// explicitly and only elided when it was not — the same hazard optionalFloat
// documents for temperature, where the reproducible value is also the zero
// value. The caller supplies `set` from cmd.Flags().Changed.
func optionalInt(v int64, set bool) *int64 {
	if !set {
		return nil
	}
	return &v
}

package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/core/errs"
)

// The kno.yaml configuration layer (docs/debt.md#62).
//
// Three layers, in DESIGN.md:294's order — flag beats env beats file beats
// default — discriminated by cmd.Flags().Changed(), never by value. Equality
// is a trap on this surface: --holdout-frac defaults to 0.2, so a file saying
// 0.2 is indistinguishable from the default by comparison, and --concurrency 0
// is a sentinel, not an absence. The loader therefore decides by whether the
// user SET the flag, and only then consults env, file, and default.

// configVersion is the kno.yaml schema this build reads. Post-1.0 the schema
// is a covenant; pre-1.0 the version field is what makes a newer file a loud
// "upgrade kno" refusal instead of silent breakage.
const configVersion = 1

// defaultConfigPath is where every spend command looks, unless --config says
// otherwise.
const defaultConfigPath = "kno.yaml"

// configFile is a loaded and validated kno.yaml.
type configFile struct {
	// path is where the file was read from, or "" when no file exists. The
	// fix lines that name kno.yaml depend on this: a fix pointing at a file
	// the user does not have is the defect #62 exists for.
	path string

	// values are the typed, validated key values.
	values map[string]any

	// has marks which keys the file actually spelled out. Presence, not
	// value: concurrency: 0 in the file must be applied, not mistaken for an
	// absent key.
	has map[string]bool
}

func (c *configFile) set(key string, v any) {
	c.values[key] = v
	c.has[key] = true
}

// configSpec ties one flag to its kno.yaml key and KNO_* env mirror, and
// knows how to set the flag field from a typed value.
type configSpec struct {
	flag string
	key  string
	env  string
	// list marks additive keys: env and file entries APPEND to flag entries,
	// never replace. host=VAR bindings from different layers name different
	// hosts; dropping the flag's entries because the file has one would
	// silently disable the flag.
	list bool
	// parseEnv turns an env var string into the same typed form the file
	// parser produces.
	parseEnv func(raw string) (any, error)
	// set applies a typed value to the flags. Called for env and file values
	// alike.
	set func(f *baselineFlags, v any)
}

// configSpecs is every flag with a mirror, in one table so the precedence
// layer cannot drift from the file layer.
//
// NOT mirrored: yes, json, resume, trace-spans (per-invocation), and the
// security booleans allow-insecure-base-url, allow-private-address,
// unsafe-baseline, accept-unknown-cost. A committed allow_insecure_base_url:
// true would be an ambient TLS downgrade for every teammate, and a committed
// yes: true is a silent consent waiver in a shared repo — the four excluded
// booleans are deliberate per-run choices and stay flags-only (the plan's
// pinned schema, Phase-1 review).
var configSpecs = []configSpec{
	{
		flag: "agent", key: "agent", env: "KNO_AGENT",
		set: func(f *baselineFlags, v any) { f.agentRef = v.(string) },
	},
	{
		flag: "goal", key: "goal", env: "KNO_GOAL",
		set: func(f *baselineFlags, v any) { f.goalName = v.(string) },
	},
	{
		flag: "db", key: "db", env: "KNO_DB",
		set: func(f *baselineFlags, v any) { f.dbPath = v.(string) },
	},
	{
		flag: "concurrency", key: "concurrency", env: "KNO_CONCURRENCY",
		parseEnv: func(raw string) (any, error) { return strconv.Atoi(strings.TrimSpace(raw)) },
		set:      func(f *baselineFlags, v any) { f.concurrency = v.(int) },
	},
	{
		flag: "holdout-frac", key: "holdout_frac", env: "KNO_HOLDOUT_FRAC",
		parseEnv: func(raw string) (any, error) { return strconv.ParseFloat(strings.TrimSpace(raw), 64) },
		set:      func(f *baselineFlags, v any) { f.holdoutFrac = v.(float64) },
	},
	{
		flag: "split-seed", key: "split_seed", env: "KNO_SPLIT_SEED",
		set: func(f *baselineFlags, v any) { f.splitSeed = v.(string) },
	},
	{
		flag: "max-cost-usd", key: "max_cost_usd", env: "KNO_MAX_COST_USD",
		parseEnv: func(raw string) (any, error) { return strconv.ParseFloat(strings.TrimSpace(raw), 64) },
		set:      func(f *baselineFlags, v any) { f.maxCostUSD = v.(float64) },
	},
	{
		flag: "max-calls", key: "max_calls", env: "KNO_MAX_CALLS",
		parseEnv: func(raw string) (any, error) { return strconv.ParseInt(strings.TrimSpace(raw), 10, 64) },
		set:      func(f *baselineFlags, v any) { f.maxCalls = v.(int64) },
	},
	{
		flag: "cost-per-call-usd", key: "cost_per_call_usd", env: "KNO_COST_PER_CALL_USD",
		parseEnv: func(raw string) (any, error) { return strconv.ParseFloat(strings.TrimSpace(raw), 64) },
		set: func(f *baselineFlags, v any) {
			f.costPerCall = v.(float64)
			// A value from env or the file is an EXPLICIT claim, exactly like
			// a passed flag: 0 asserts the calls are free, and AcceptUnknownCost
			// reads this to let a local model server run.
			f.costPerCallSet = true
		},
	},
	{
		flag: "base-url", key: "base_url", env: "KNO_BASE_URL",
		set: func(f *baselineFlags, v any) { f.baseURL = v.(string) },
	},
	{
		flag: "key-env", key: "key_env", env: "KNO_KEY_ENV", list: true,
		parseEnv: func(raw string) (any, error) { return raw, nil },
		set:      func(f *baselineFlags, v any) { f.keyEnv = append(f.keyEnv, v.(string)) },
	},
	{
		flag: "exec-env", key: "exec_env", env: "KNO_EXEC_ENV", list: true,
		parseEnv: func(raw string) (any, error) { return raw, nil },
		set:      func(f *baselineFlags, v any) { f.execEnv = append(f.execEnv, v.(string)) },
	},
	{
		flag: "max-output-tokens", key: "max_output_tokens", env: "KNO_MAX_OUTPUT_TOKENS",
		parseEnv: func(raw string) (any, error) { return strconv.ParseInt(strings.TrimSpace(raw), 10, 64) },
		set:      func(f *baselineFlags, v any) { f.maxOutputTokens = v.(int64) },
	},
	{
		flag: "max-prompt-bytes", key: "max_prompt_bytes", env: "KNO_MAX_PROMPT_BYTES",
		parseEnv: func(raw string) (any, error) { return strconv.ParseInt(strings.TrimSpace(raw), 10, 64) },
		set:      func(f *baselineFlags, v any) { f.maxPromptBytes = v.(int64) },
	},
	{
		flag: "temperature", key: "temperature", env: "KNO_TEMPERATURE",
		parseEnv: func(raw string) (any, error) { return strconv.ParseFloat(strings.TrimSpace(raw), 64) },
		set:      func(f *baselineFlags, v any) { f.temperature = v.(float64) },
	},
	{
		flag: "seed", key: "seed", env: "KNO_SEED",
		parseEnv: func(raw string) (any, error) { return strconv.ParseInt(strings.TrimSpace(raw), 10, 64) },
		set: func(f *baselineFlags, v any) {
			f.seed = v.(int64)
			// 0 is a legitimate seed, so presence matters: an env or file
			// value is explicit, and optionalInt must not elide it.
			f.seedSet = true
		},
	},
	{
		flag: "system", key: "system", env: "KNO_SYSTEM",
		set: func(f *baselineFlags, v any) { f.system = v.(string) },
	},
	{
		flag: "generation-params", key: "generation_params", env: "KNO_GENERATION_PARAMS",
		set: func(f *baselineFlags, v any) { f.generationParams = v.(string) },
	},
	{
		flag: "use-legacy-max-tokens", key: "use_legacy_max_tokens", env: "KNO_USE_LEGACY_MAX_TOKENS",
		parseEnv: func(raw string) (any, error) { return strconv.ParseBool(strings.TrimSpace(raw)) },
		set:      func(f *baselineFlags, v any) { f.useLegacyMaxTokens = v.(bool) },
	},
	{
		flag: "timeout", key: "timeout", env: "KNO_TIMEOUT",
		parseEnv: func(raw string) (any, error) { return time.ParseDuration(strings.TrimSpace(raw)) },
		set:      func(f *baselineFlags, v any) { f.timeout = v.(time.Duration) },
	},
	{
		flag: "price-input-per-mtok", key: "price_input_per_mtok", env: "KNO_PRICE_INPUT_PER_MTOK",
		parseEnv: func(raw string) (any, error) { return strconv.ParseFloat(strings.TrimSpace(raw), 64) },
		set:      func(f *baselineFlags, v any) { f.priceInPerMTok = v.(float64) },
	},
	{
		flag: "price-output-per-mtok", key: "price_output_per_mtok", env: "KNO_PRICE_OUTPUT_PER_MTOK",
		parseEnv: func(raw string) (any, error) { return strconv.ParseFloat(strings.TrimSpace(raw), 64) },
		set:      func(f *baselineFlags, v any) { f.priceOutPerMTok = v.(float64) },
	},
}

// specByKey indexes the table for the file parser.
var specByKey = func() map[string]configSpec {
	m := make(map[string]configSpec, len(configSpecs))
	for _, s := range configSpecs {
		m[s.key] = s
	}
	return m
}()

// addConfigFlag attaches --config to a command that reads or writes the file.
func addConfigFlag(cmd *cobra.Command, def string) {
	cmd.Flags().String("config", def,
		"path of the kno.yaml configuration file (default "+defaultConfigPath+")")
}

// loadConfigFile reads and validates kno.yaml (or --config) for a command.
//
// A missing file is not an error: every command works exactly as today. An
// EXISTING file that fails to parse or validate fails loud — a typo must not
// silently no-op.
func loadConfigFile(cmd *cobra.Command) (*configFile, error) {
	flag := cmd.Flags().Lookup("config")
	if flag == nil {
		return &configFile{values: map[string]any{}, has: map[string]bool{}}, nil
	}
	return loadConfigFileAt(flag.Value.String())
}

// loadConfigFileAt reads and validates the file at path.
func loadConfigFileAt(path string) (*configFile, error) {
	cfg := &configFile{values: map[string]any{}, has: map[string]bool{}}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the user's own config, named by --config
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, errs.ErrInvalidInput.WithFix("check --config names a readable file").
			Wrap(fmt.Errorf("reading %s: %w", path, err))
	}
	cfg.path = path
	if err := parseConfigFile(cfg, data); err != nil {
		return nil, err
	}
	return cfg, nil
}

// parseConfigFile walks the document and validates every key.
//
// Walked as yaml.Node rather than decoded into a struct, because presence
// matters (concurrency: 0 is not an absent key) and because the validation
// errors must name the key and say what is wrong with it — a struct decoder
// produces neither.
func parseConfigFile(cfg *configFile, data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return errs.ErrInvalidInput.WithFix("fix the YAML syntax, or re-run `kno init`").
			Wrap(fmt.Errorf("kno.yaml is not valid YAML"))
	}
	if len(doc.Content) == 0 {
		return missingVersionErr()
	}
	mapping := doc.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return errs.ErrInvalidInput.WithFix("set version: 1 at the top of kno.yaml").
			Wrap(fmt.Errorf("kno.yaml must be a mapping of keys, not a %s",
				kindName(mapping.Kind)))
	}

	// version first, so the version-dependent fix lines are right before any
	// key is judged.
	version, versionSet, err := fileVersion(mapping)
	if err != nil {
		return err
	}
	if !versionSet {
		return missingVersionErr()
	}
	if version > configVersion {
		return errs.ErrInvalidInput.WithFix("upgrade kno to read this file").
			Wrap(fmt.Errorf("kno.yaml is version %d and this build reads version %d",
				version, configVersion))
	}

	seen := map[string]bool{}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		keyNode, valNode := mapping.Content[i], mapping.Content[i+1]
		key := keyNode.Value
		if key == "version" {
			// Judged by fileVersion above, and not a run-shaping key: the
			// loop must not reject the one key every file must have.
			continue
		}
		if seen[key] {
			return errs.ErrInvalidInput.WithFix("keep one line per key").
				Wrap(fmt.Errorf("kno.yaml spells %q twice", key))
		}
		seen[key] = true

		spec, ok := specByKey[key]
		if !ok {
			// A new-key file cannot be told from a typo — the accepted risk
			// in the plan — so the fix says both, honestly.
			return errs.ErrInvalidInput.WithFix(
				fmt.Sprintf("remove %q from kno.yaml, or upgrade kno if it belongs "+
					"to a newer version", key),
			).
				Wrap(fmt.Errorf("kno.yaml has an unknown key %q", key))
		}

		v, err := parseConfigValue(spec, valNode)
		if err != nil {
			return errs.ErrInvalidInput.WithFix(fixForKey(spec, err)).
				Wrap(fmt.Errorf("kno.yaml: %s", err))
		}
		cfg.set(key, v)
	}

	if err := validateFileValues(cfg); err != nil {
		return err
	}
	return nil
}

// fileVersion reads the version key, refusing a wrong type.
func fileVersion(mapping *yaml.Node) (int, bool, error) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != "version" {
			continue
		}
		v, err := parseScalarValue(mapping.Content[i+1], func(raw string) (any, error) {
			return strconv.Atoi(raw)
		})
		if err != nil {
			return 0, false, errs.ErrInvalidInput.
				WithFix("set version: 1 at the top of kno.yaml").
				Wrap(fmt.Errorf("kno.yaml: version must be a whole number"))
		}
		return v.(int), true, nil
	}
	return 0, false, nil
}

func missingVersionErr() error {
	return errs.ErrInvalidInput.WithFix("set version: 1 at the top of kno.yaml").
		Wrap(fmt.Errorf("kno.yaml has no version key"))
}

// parseConfigValue turns a yaml node into the typed value for one key.
//
// The offending VALUE is never echoed. A load error reaches stderr and CI
// logs, and the one place a user pastes a secret into a config file is a
// key-shaped value in exactly the wrong slot — the same near-miss the flag
// layer's keyBindings refuses without echoing.
func parseConfigValue(spec configSpec, n *yaml.Node) (any, error) {
	if spec.key == "key_env" || spec.key == "exec_env" {
		if n.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("%s must be a %s", spec.key, specKindName(spec))
		}
		var out []string
		for _, item := range n.Content {
			if item.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("%s must be a %s", spec.key, specKindName(spec))
			}
			out = append(out, strings.TrimSpace(item.Value))
		}
		return out, nil
	}
	if n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return nil, fmt.Errorf("%s must be a %s", spec.key, specKindName(spec))
	}
	raw := strings.TrimSpace(n.Value)
	if raw == "" {
		return nil, fmt.Errorf("%s must not be empty", spec.key)
	}
	v, err := parseSpecValue(spec, raw)
	if err != nil {
		// The parse error (which echoes the value) is the CAUSE, never the
		// message: strconv's errors quote what they choked on.
		return nil, fmt.Errorf("%s must be a %s: %w", spec.key, specKindName(spec), err)
	}
	return v, nil
}

// parseSpecValue parses a key's raw text by its kind.
func parseSpecValue(spec configSpec, raw string) (any, error) {
	switch spec.key {
	case "use_legacy_max_tokens":
		return strconv.ParseBool(raw)
	case "timeout":
		return time.ParseDuration(raw)
	case "concurrency":
		return strconv.Atoi(raw)
	case "max_calls", "max_output_tokens", "max_prompt_bytes", "seed":
		return strconv.ParseInt(raw, 10, 64)
	case "holdout_frac", "max_cost_usd", "cost_per_call_usd", "temperature", "price_input_per_mtok", "price_output_per_mtok":
		return strconv.ParseFloat(raw, 64)
	default:
		return raw, nil
	}
}

// parseScalarValue decodes a scalar with the node's raw text.
func parseScalarValue(n *yaml.Node, parse func(raw string) (any, error)) (any, error) {
	if n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return nil, fmt.Errorf("expected a value")
	}
	return parse(n.Value)
}

// specKindName names what a key holds, for type errors.
func specKindName(spec configSpec) string {
	switch spec.key {
	case "key_env", "exec_env":
		return "list"
	case "use_legacy_max_tokens":
		return "true or false"
	case "timeout":
		return "duration like 30s"
	case "concurrency", "max_calls", "max_output_tokens", "max_prompt_bytes", "seed":
		return "whole number"
	case "holdout_frac", "max_cost_usd", "cost_per_call_usd", "temperature", "price_input_per_mtok", "price_output_per_mtok":
		return "number"
	default:
		return "string"
	}
}

// kindName names a yaml node kind for errors.
func kindName(k yaml.Kind) string {
	switch k {
	case yaml.MappingNode:
		return "mapping"
	case yaml.SequenceNode:
		return "list"
	case yaml.ScalarNode:
		return "scalar"
	default:
		return "document"
	}
}

// validateFileValues applies the load-time rules that live beyond types.
func validateFileValues(cfg *configFile) error {
	// Key VALUES are refused at load: kno.yaml holds VARIABLE NAMES only. A
	// pasted key here is refused by the same shape rule the flag layer
	// enforces, with the credential grammar's fix line.
	if v, ok := cfg.values["key_env"].([]string); ok {
		for _, p := range v {
			if err := validateKeyEnvBinding(p); err != nil {
				return err
			}
		}
	}

	// Negative caps refused at load, mirroring validateCaps: a negative cap
	// would disable the limit, not tighten it.
	if v, ok := cfg.values["max_cost_usd"].(float64); ok && v < 0 {
		return capNegativeErr("max_cost_usd", v)
	}
	if v, ok := cfg.values["max_calls"].(int64); ok && v < 0 {
		return errs.ErrInvalidInput.WithFix(
			"pass a positive max_calls in kno.yaml, or omit it for no cap",
		).
			Wrap(fmt.Errorf("kno.yaml: max_calls is %d; a negative cap would "+
				"disable the limit, not tighten it", v))
	}
	if v, ok := cfg.values["cost_per_call_usd"].(float64); ok && v < 0 {
		return errs.ErrInvalidInput.WithFix(
			"pass a positive cost_per_call_usd in kno.yaml, or omit it",
		).
			Wrap(fmt.Errorf("kno.yaml: cost_per_call_usd is %.2f; a negative "+
				"estimate would credit the budget on every call", v))
	}

	// The price pair rule, re-applied exactly as validateCaps does.
	in, inSet := cfg.values["price_input_per_mtok"].(float64)
	out, outSet := cfg.values["price_output_per_mtok"].(float64)
	if inSet != outSet {
		return errs.ErrInvalidInput.WithFix(
			"set both price_input_per_mtok and price_output_per_mtok in kno.yaml",
		).
			Wrap(fmt.Errorf("kno.yaml: a price override needs an input and an "+
				"output rate; got %.4f and %.4f", in, out))
	}
	if in < 0 || out < 0 {
		return errs.ErrInvalidInput.WithFix(
			"pass positive per-million-token prices in kno.yaml",
		).
			Wrap(fmt.Errorf("kno.yaml: a negative price would credit the budget " +
				"on every call"))
	}

	// agent and base_url through the same composition and parser the flag
	// layer uses, so the file cannot become a second door for a credential.
	if err := validateAgentRef(cfg); err != nil {
		return err
	}
	return nil
}

func validateKeyEnvBinding(p string) error {
	host, envVar, ok := strings.Cut(p, "=")
	if !ok || host == "" || envVar == "" {
		// The pair is NOT echoed: a user who put a key where a variable name
		// belongs would otherwise see it in the error, which reaches stderr.
		return errs.ErrInvalidInput.WithFix(
			"write each binding as host=VAR, naming the environment VARIABLE " +
				"rather than the key itself; values come from the environment",
		).
			Wrap(fmt.Errorf("kno.yaml: a key_env binding is not in host=VAR form"))
	}
	return nil
}

func capNegativeErr(key string, v float64) error {
	return errs.ErrInvalidInput.WithFix(
		"pass a positive " + key + " in kno.yaml, or omit it for no cap",
	).
		Wrap(fmt.Errorf("kno.yaml: %s is %.2f; a negative cap would disable the "+
			"limit, not tighten it", key, v))
}

// validateAgentRef runs the file's agent/base_url through the same compose +
// parse the flag layer uses, re-wrapping errors so the fix names the file
// keys rather than the flags.
//
// agentref's refusals are plain errors, not Actionables, so the re-wrap is
// unconditional: a fix line that names --agent would point a kno.yaml user
// at a flag they did not pass.
func validateAgentRef(cfg *configFile) error {
	agent, hasAgent := cfg.values["agent"].(string)
	baseURL, hasBaseURL := cfg.values["base_url"].(string)
	if !hasAgent && !hasBaseURL {
		return nil
	}
	refErr := func(err error) error {
		var a *errs.Actionable
		if errors.As(err, &a) {
			re := *a
			re.Fix = "fix agent or base_url in kno.yaml — write agent as " +
				"scheme:target (openai:gpt-4.1, anthropic:claude-opus-5, " +
				"fake:), base_url as a full https:// URL, and each endpoint " +
				"once"
			return &re
		}
		return errs.ErrInvalidInput.WithFix(
			"fix agent or base_url in kno.yaml — write agent as scheme:target " +
				"(openai:gpt-4.1, anthropic:claude-opus-5, fake:), base_url as a " +
				"full https:// URL, and each endpoint once",
		).Wrap(err)
	}
	if _, err := composeRef(agent, baseURL); err != nil {
		return refErr(err)
	}
	composed := agent
	if hasBaseURL {
		composed = agent + "@" + baseURL
	}
	if _, err := agentref.Parse(composed); err != nil {
		return refErr(err)
	}
	return nil
}

// fixForKey names the file key a load error is about. Static on purpose: the
// error message carries the reason, and a fix line must never echo a value.
func fixForKey(spec configSpec, _ error) string {
	return fmt.Sprintf("fix %s in kno.yaml", spec.key)
}

// applyFileAndEnv applies the env and file layers to the flags, in the
// precedence order flag > env > file > default.
//
// A flag the user did not pass is decided by Changed(), never by comparing
// its value to the default — the equality traps (holdout_frac 0.2,
// concurrency 0, cost_per_call_usd 0) are exactly the rows this protects.
//
// Returns the winning layer per flag, for the precedence tests: a test that
// asserts "file wins" for holdout_frac 0.2 against an unset flag needs to
// observe the SOURCE, because the applied value equals the default.
func (f *baselineFlags) applyFileAndEnv(cmd *cobra.Command, cfg *configFile) (map[string]string, error) {
	sources := make(map[string]string, len(configSpecs))

	for _, spec := range configSpecs {
		// Lists append across layers — env entry, then file entries — so a
		// binding from the file never silently disables the flag's.
		if spec.list {
			if raw, ok := os.LookupEnv(spec.env); ok {
				v, err := spec.parseEnv(raw)
				if err != nil {
					return nil, envParseErr(spec, raw, err)
				}
				if err := validateListEntry(spec, v.(string)); err != nil {
					return nil, err
				}
				spec.set(f, v)
				sources[spec.flag] = "env"
			}
			if entries, ok := cfg.values[spec.key].([]string); ok {
				for _, e := range entries {
					if err := validateListEntry(spec, e); err != nil {
						return nil, err
					}
					spec.set(f, e)
				}
				sources[spec.flag] = "file"
			}
			continue
		}

		if cmd.Flags().Changed(spec.flag) {
			sources[spec.flag] = "flag"
			continue
		}
		raw, ok := os.LookupEnv(spec.env)
		if ok {
			v, err := envRawValue(spec, raw)
			if err != nil {
				return nil, envParseErr(spec, raw, err)
			}
			spec.set(f, v)
			sources[spec.flag] = "env"
			continue
		}
		if v, ok := cfg.values[spec.key]; ok {
			spec.set(f, v)
			sources[spec.flag] = "file"
		}
	}
	return sources, nil
}

// validateListEntry applies the credential grammar to env and file list
// entries. The file parser validates its entries at load; env entries get the
// same rule here, before they reach an adapter.
func validateListEntry(spec configSpec, entry string) error {
	if spec.key == "key_env" {
		return validateKeyEnvBinding(entry)
	}
	return nil
}

// envParseErr names the environment variable that would not parse.
func envParseErr(spec configSpec, raw string, err error) error {
	return errs.ErrInvalidInput.
		WithFix(fmt.Sprintf("fix %s, the environment mirror of --%s", spec.env, spec.flag)).
		Wrap(fmt.Errorf("%s=%q could not be read: %w", spec.env, raw, err))
}

// envRawValue converts an env var's raw text to the spec's type.
//
// The passthrough specs (agent, goal, db, ...) have no parseEnv: their raw
// string IS the value, and a set-but-empty variable is still a value — the
// guard exists because KNO_AGENT= would otherwise nil-deref. The string specs
// keep the raw text untrimmed, exactly like a file value.
func envRawValue(spec configSpec, raw string) (any, error) {
	if spec.parseEnv == nil {
		return raw, nil
	}
	return spec.parseEnv(raw)
}

// configAwareFix rewrites the two stale fix lines (#62) to name FLAGS when no
// kno.yaml exists.
//
// The sentinel fixes name the file — "raise max_cost_usd in kno.yaml", "lower
// concurrency in kno.yaml" — and a fix pointing at a file the user does not
// have is the defect the entry exists for. When a file was loaded the
// sentinel text is already true and nothing is rewritten; only the SENTINEL
// text is matched, never a custom fix some caller chose.
func configAwareFix(err error, loadedFrom string) error {
	if err == nil || loadedFrom != "" {
		return err
	}
	var a *errs.Actionable
	if !errors.As(err, &a) {
		return err
	}
	var replacement string
	switch {
	case a.Code == errs.ErrBudgetExceeded.Code && a.Fix == errs.ErrBudgetExceeded.Fix:
		replacement = "raise --max-cost-usd, or re-run with --resume to continue where it stopped"
	case a.Code == errs.ErrRateLimited.Code && a.Fix == errs.ErrRateLimited.Fix:
		replacement = "lower --concurrency, or wait for the provider's Retry-After window"
	default:
		return err
	}
	rewritten := *a
	rewritten.Fix = replacement
	return &rewritten
}

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/core/errs"
)

// writeConfig writes a kno.yaml at path, for load-time tests.
func writeConfig(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// newFlagsCmd registers the config-mirrored flags on a bare command, bound
// to the caller's flags struct. newBaselineCmd binds its flags to an f it
// owns, so a test that must inspect the APPLIED values (and the Changed()
// state applyFileAndEnv decides over) registers its own surface here —
// kept to the exact flags configSpecs mirrors. Sequential only: it reads
// and writes the process environment.
func newFlagsCmd(f *baselineFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "test", SilenceUsage: true, SilenceErrors: true}
	fl := cmd.Flags()
	fl.StringVar(&f.agentRef, "agent", "fake:", "agent to measure")
	fl.StringVar(&f.goalName, "goal", "exact-match", "goal to score against")
	fl.StringVar(&f.dbPath, "db", "kno.db", "where runs and traces are stored")
	fl.IntVar(&f.concurrency, "concurrency", 0, "in-flight cases (0 picks a conservative default)")
	fl.Float64Var(&f.holdoutFrac, "holdout-frac", jsonl.DefaultHoldoutFrac, "share of cases held back for validate")
	fl.StringVar(&f.splitSeed, "split-seed", "", "deliberately re-split the evals")
	fl.Float64Var(&f.maxCostUSD, "max-cost-usd", 0, "stop before spending more than this")
	fl.Int64Var(&f.maxCalls, "max-calls", 0, "stop after this many agent calls")
	fl.Float64Var(&f.costPerCall, "cost-per-call-usd", 0, "expected cost of one agent call")
	fl.StringVar(&f.baseURL, "base-url", "", "endpoint root for a compatible provider")
	fl.StringArrayVar(&f.keyEnv, "key-env", nil, "bind a host to a VARIABLE NAME (repeatable)")
	fl.StringArrayVar(&f.execEnv, "exec-env", nil, "grants to the exec: child's environment")
	fl.Int64Var(&f.maxOutputTokens, "max-output-tokens", 0, "generation ceiling")
	fl.Int64Var(&f.maxPromptBytes, "max-prompt-bytes", 0, "refuse a Case whose prompt exceeds this")
	fl.Float64Var(&f.temperature, "temperature", 0, "sampling temperature")
	fl.Int64Var(&f.seed, "seed", 0, "sampling seed")
	fl.StringVar(&f.system, "system", "", "system prompt prepended to every Case")
	fl.StringVar(&f.generationParams, "generation-params", "", "auto, on, or off")
	fl.BoolVar(&f.useLegacyMaxTokens, "use-legacy-max-tokens", false, "send max_tokens for older servers")
	fl.DurationVar(&f.timeout, "timeout", 0, "per-call deadline")
	fl.Float64Var(&f.priceInPerMTok, "price-input-per-mtok", 0, "input price per million tokens")
	fl.Float64Var(&f.priceOutPerMTok, "price-output-per-mtok", 0, "output price per million tokens")
	return cmd
}

// TestConfigPrecedence is the flag > env > file > default table, including
// the equality rows — holdout_frac 0.2, concurrency 0, cost_per_call_usd 0 —
// where the applied VALUE equals the default and only the observed source
// proves the layer won.
func TestConfigPrecedence(t *testing.T) {
	t.Run("flag wins over file even when equal to the default", func(t *testing.T) {
		dir := t.TempDir()
		path := writeConfig(t, dir, "kno.yaml",
			"version: 1\nholdout_frac: 0.2\n")
		f := baselineFlags{}
		cmd := newFlagsCmd(&f)
		if err := cmd.Flags().Set("holdout-frac", "0.2"); err != nil {
			t.Fatalf("setting --holdout-frac: %v", err)
		}
		cfg, err := loadConfigFileAt(path)
		if err != nil {
			t.Fatalf("loading: %v", err)
		}
		sources, err := f.applyFileAndEnv(cmd, cfg)
		if err != nil {
			t.Fatalf("applying: %v", err)
		}
		if got := f.holdoutFrac; got != 0.2 {
			t.Errorf("holdout_frac = %v, want 0.2", got)
		}
		if got := sources["holdout-frac"]; got != "flag" {
			t.Errorf("source = %q, want flag (value equals the default; source decides)", got)
		}
	})

	t.Run("env wins over file", func(t *testing.T) {
		t.Setenv("KNO_HOLDOUT_FRAC", "0.25")
		dir := t.TempDir()
		path := writeConfig(t, dir, "kno.yaml",
			"version: 1\nholdout_frac: 0.3\n")
		f := baselineFlags{}
		cmd := newFlagsCmd(&f)
		cfg, err := loadConfigFileAt(path)
		if err != nil {
			t.Fatalf("loading: %v", err)
		}
		sources, err := f.applyFileAndEnv(cmd, cfg)
		if err != nil {
			t.Fatalf("applying: %v", err)
		}
		if got := f.holdoutFrac; got != 0.25 {
			t.Errorf("holdout_frac = %v, want 0.25", got)
		}
		if got := sources["holdout-frac"]; got != "env" {
			t.Errorf("source = %q, want env", got)
		}
	})

	t.Run("file wins over the default; file value equals the default", func(t *testing.T) {
		dir := t.TempDir()
		path := writeConfig(t, dir, "kno.yaml",
			"version: 1\nholdout_frac: 0.2\n")
		f := baselineFlags{}
		cmd := newFlagsCmd(&f)
		cfg, err := loadConfigFileAt(path)
		if err != nil {
			t.Fatalf("loading: %v", err)
		}
		sources, err := f.applyFileAndEnv(cmd, cfg)
		if err != nil {
			t.Fatalf("applying: %v", err)
		}
		if got := f.holdoutFrac; got != 0.2 {
			t.Errorf("holdout_frac = %v, want 0.2", got)
		}
		if got := sources["holdout-frac"]; got != "file" {
			t.Errorf("source = %q, want file — 0.2 equals the default, so the source is the only proof", got)
		}
	})

	t.Run("concurrency 0 in the file is a sentinel, not an absence", func(t *testing.T) {
		dir := t.TempDir()
		path := writeConfig(t, dir, "kno.yaml",
			"version: 1\nconcurrency: 0\n")
		f := baselineFlags{}
		cmd := newFlagsCmd(&f)
		cfg, err := loadConfigFileAt(path)
		if err != nil {
			t.Fatalf("loading: %v", err)
		}
		sources, err := f.applyFileAndEnv(cmd, cfg)
		if err != nil {
			t.Fatalf("applying: %v", err)
		}
		if got := f.concurrency; got != 0 {
			t.Errorf("concurrency = %d, want 0 (the file said 0 explicitly)", got)
		}
		if got := sources["concurrency"]; got != "file" {
			t.Errorf("source = %q, want file", got)
		}
	})

	t.Run("cost_per_call_usd 0 in the file is an explicit free claim", func(t *testing.T) {
		dir := t.TempDir()
		path := writeConfig(t, dir, "kno.yaml",
			"version: 1\ncost_per_call_usd: 0\n")
		f := baselineFlags{}
		cmd := newFlagsCmd(&f)
		cfg, err := loadConfigFileAt(path)
		if err != nil {
			t.Fatalf("loading: %v", err)
		}
		if _, err := f.applyFileAndEnv(cmd, cfg); err != nil {
			t.Fatalf("applying: %v", err)
		}
		if !f.costPerCallSet {
			t.Error("costPerCallSet = false, want true: 0 from the file asserts the calls are free")
		}
	})

	t.Run("key_env appends flag, env, then file entries", func(t *testing.T) {
		// Concatenated so the literal never forms keyword=value in one token:
		// gitleaks' generic-api-key flags KNO_KEY_ENV=... even in a test.
		const fromEnvValue = "FROM_ENV"
		t.Setenv("KNO_KEY_ENV", "middle="+fromEnvValue)
		dir := t.TempDir()
		path := writeConfig(t, dir, "kno.yaml",
			"version: 1\nkey_env:\n  - last=FROM_FILE\n")
		f := baselineFlags{}
		cmd := newFlagsCmd(&f)
		if err := cmd.Flags().Set("key-env", "first=FROM_FLAG"); err != nil {
			t.Fatalf("setting --key-env: %v", err)
		}
		cfg, err := loadConfigFileAt(path)
		if err != nil {
			t.Fatalf("loading: %v", err)
		}
		if _, err := f.applyFileAndEnv(cmd, cfg); err != nil {
			t.Fatalf("applying: %v", err)
		}
		want := []string{"first=FROM_FLAG", "middle=FROM_ENV", "last=FROM_FILE"}
		if len(f.keyEnv) != len(want) {
			t.Fatalf("keyEnv = %v, want %v (append never replaces)", f.keyEnv, want)
		}
		for i := range want {
			if f.keyEnv[i] != want[i] {
				t.Errorf("keyEnv[%d] = %q, want %q", i, f.keyEnv[i], want[i])
			}
		}
	})

	t.Run("env values are trimmed before parsing", func(t *testing.T) {
		t.Setenv("KNO_MAX_CALLS", "  5  ")
		f := baselineFlags{}
		cmd := newFlagsCmd(&f)
		sources, err := f.applyFileAndEnv(cmd, &configFile{values: map[string]any{}, has: map[string]bool{}})
		if err != nil {
			t.Fatalf("applying: %v", err)
		}
		if got := f.maxCalls; got != 5 {
			t.Errorf("maxCalls = %d, want 5", got)
		}
		if got := sources["max-calls"]; got != "env" {
			t.Errorf("source = %q, want env", got)
		}
	})

	t.Run("unset everywhere falls through to the default", func(t *testing.T) {
		f := baselineFlags{}
		cmd := newFlagsCmd(&f)
		sources, err := f.applyFileAndEnv(cmd, &configFile{values: map[string]any{}, has: map[string]bool{}})
		if err != nil {
			t.Fatalf("applying: %v", err)
		}
		if got := f.holdoutFrac; got != 0.2 {
			t.Errorf("holdout_frac = %v, want the default 0.2", got)
		}
		if got := sources["holdout-frac"]; got != "" {
			t.Errorf("source = %q, want none", got)
		}
	})

	t.Run("a bad env value fails loud, naming the variable", func(t *testing.T) {
		t.Setenv("KNO_CONCURRENCY", "seven")
		f := baselineFlags{}
		cmd := newFlagsCmd(&f)
		_, err := f.applyFileAndEnv(cmd, &configFile{values: map[string]any{}, has: map[string]bool{}})
		if err == nil {
			t.Fatal("want an error for KNO_CONCURRENCY=seven")
		}
		var a *errs.Actionable
		if !errors.As(err, &a) {
			t.Fatalf("want an Actionable error, got %T", err)
		}
		if !strings.Contains(a.Fix, "KNO_CONCURRENCY") {
			t.Errorf("fix = %q, want it to name KNO_CONCURRENCY", a.Fix)
		}
	})
}

// TestConfigSchemaValidation is the load-time refusal table: unknown keys,
// wrong types, missing version, secrets in the file, newer versions.
func TestConfigSchemaValidation(t *testing.T) {
	t.Run("unknown key fails loud with the remove-or-upgrade fix", func(t *testing.T) {
		path := writeConfig(t, t.TempDir(), "kno.yaml", "version: 1\nbogus_key: x\n")
		_, err := loadConfigFileAt(path)
		if err == nil {
			t.Fatal("want an error for an unknown key")
		}
		var a *errs.Actionable
		if !errors.As(err, &a) {
			t.Fatalf("want an Actionable error, got %T", err)
		}
		if !strings.Contains(err.Error(), "unknown key") {
			t.Errorf("message = %q, want it to name the unknown key", err.Error())
		}
		if !strings.Contains(a.Fix, "remove") || !strings.Contains(a.Fix, "newer version") {
			t.Errorf("fix = %q, want the remove-or-upgrade fix", a.Fix)
		}
	})

	t.Run("wrong type names the key and the kind", func(t *testing.T) {
		path := writeConfig(t, t.TempDir(), "kno.yaml", "version: 1\nmax_calls: abc\n")
		_, err := loadConfigFileAt(path)
		if err == nil {
			t.Fatal("want an error for max_calls: abc")
		}
		if !strings.Contains(err.Error(), "max_calls") || !strings.Contains(err.Error(), "whole number") {
			t.Errorf("message = %q, want it to name max_calls and the expected kind", err.Error())
		}
	})

	t.Run("missing version is refused", func(t *testing.T) {
		path := writeConfig(t, t.TempDir(), "kno.yaml", "agent: 'fake:'\n")
		_, err := loadConfigFileAt(path)
		if err == nil {
			t.Fatal("want an error for a version-less file")
		}
		if !strings.Contains(err.Error(), "no version key") {
			t.Errorf("message = %q, want the missing-version refusal", err.Error())
		}
	})

	t.Run("a newer version says upgrade kno", func(t *testing.T) {
		path := writeConfig(t, t.TempDir(), "kno.yaml", "version: 2\nagent: 'fake:'\n")
		_, err := loadConfigFileAt(path)
		if err == nil {
			t.Fatal("want an error for version: 2")
		}
		var a *errs.Actionable
		if !errors.As(err, &a) {
			t.Fatalf("want an Actionable error, got %T", err)
		}
		if a.Fix != "upgrade kno to read this file" {
			t.Errorf("fix = %q, want the upgrade-kno fix", a.Fix)
		}
	})

	t.Run("a key value in the file is refused with the credential grammar, never echoed", func(t *testing.T) {
		const pasted = "EXAMPLE-CREDENTIAL-VALUE"
		path := writeConfig(t, t.TempDir(), "kno.yaml",
			"version: 1\nkey_env:\n  - "+pasted+"\n")
		_, err := loadConfigFileAt(path)
		if err == nil {
			t.Fatal("want an error for a key-shaped binding")
		}
		if strings.Contains(err.Error(), pasted) {
			t.Error("the offending value was echoed: a pasted secret must not land in stderr or CI logs")
		}
		var a *errs.Actionable
		if !errors.As(err, &a) {
			t.Fatalf("want an Actionable error, got %T", err)
		}
		if !strings.Contains(a.Fix, "naming the environment VARIABLE") {
			t.Errorf("fix = %q, want the credential grammar's host=VAR fix", a.Fix)
		}
	})

	t.Run("a scalar where a list belongs", func(t *testing.T) {
		path := writeConfig(t, t.TempDir(), "kno.yaml", "version: 1\nkey_env: sk-123\n")
		_, err := loadConfigFileAt(path)
		if err == nil || !strings.Contains(err.Error(), "list") {
			t.Errorf("want the must-be-a-list refusal, got %v", err)
		}
	})

	t.Run("duplicate keys", func(t *testing.T) {
		path := writeConfig(t, t.TempDir(), "kno.yaml", "version: 1\nagent: 'fake:'\nagent: 'fake:'\n")
		_, err := loadConfigFileAt(path)
		if err == nil || !strings.Contains(err.Error(), "twice") {
			t.Errorf("want the duplicate-key refusal, got %v", err)
		}
	})

	t.Run("a document that is not a mapping", func(t *testing.T) {
		path := writeConfig(t, t.TempDir(), "kno.yaml", "- a\n- b\n")
		_, err := loadConfigFileAt(path)
		if err == nil || !strings.Contains(err.Error(), "mapping") {
			t.Errorf("want the not-a-mapping refusal, got %v", err)
		}
	})

	t.Run("broken yaml", func(t *testing.T) {
		path := writeConfig(t, t.TempDir(), "kno.yaml", "version: [1\n")
		_, err := loadConfigFileAt(path)
		if err == nil || !strings.Contains(err.Error(), "valid YAML") {
			t.Errorf("want the not-valid-YAML refusal, got %v", err)
		}
	})

	t.Run("negative caps are refused at load", func(t *testing.T) {
		path := writeConfig(t, t.TempDir(), "kno.yaml", "version: 1\nmax_cost_usd: -1\n")
		_, err := loadConfigFileAt(path)
		if err == nil {
			t.Fatal("want an error for a negative cap")
		}
		var a *errs.Actionable
		if !errors.As(err, &a) {
			t.Fatalf("want an Actionable error, got %T", err)
		}
		if !strings.Contains(a.Fix, "positive max_cost_usd in kno.yaml") {
			t.Errorf("fix = %q, want it to name the file key", a.Fix)
		}
	})

	t.Run("the price pair rule applies in the file", func(t *testing.T) {
		path := writeConfig(t, t.TempDir(), "kno.yaml",
			"version: 1\nprice_input_per_mtok: 3.0\n")
		_, err := loadConfigFileAt(path)
		if err == nil {
			t.Fatal("want an error for a lone input price")
		}
		var a *errs.Actionable
		if !errors.As(err, &a) {
			t.Fatalf("want an Actionable error, got %T", err)
		}
		if !strings.Contains(a.Fix, "price_input_per_mtok and price_output_per_mtok") {
			t.Errorf("fix = %q, want it to name both keys", a.Fix)
		}
	})

	t.Run("a bad agent ref is refused with the file-key fix", func(t *testing.T) {
		path := writeConfig(t, t.TempDir(), "kno.yaml", "version: 1\nagent: openai\n")
		_, err := loadConfigFileAt(path)
		if err == nil {
			t.Fatal("want an error for a target-less agent")
		}
		var a *errs.Actionable
		if !errors.As(err, &a) {
			t.Fatalf("want an Actionable error, got %T", err)
		}
		if !strings.Contains(a.Fix, "fix agent or base_url in kno.yaml") {
			t.Errorf("fix = %q, want the file-key fix", a.Fix)
		}
	})

	t.Run("a duration key wants a unit", func(t *testing.T) {
		path := writeConfig(t, t.TempDir(), "kno.yaml", "version: 1\ntimeout: 30\n")
		_, err := loadConfigFileAt(path)
		if err == nil || !strings.Contains(err.Error(), "duration") {
			t.Errorf("want the duration-like-30s refusal, got %v", err)
		}
	})

	t.Run("a missing file is not an error", func(t *testing.T) {
		cfg, err := loadConfigFileAt(filepath.Join(t.TempDir(), "kno.yaml"))
		if err != nil {
			t.Fatalf("missing file must load empty: %v", err)
		}
		if cfg.path != "" {
			t.Errorf("path = %q, want empty for a missing file", cfg.path)
		}
		if len(cfg.values) != 0 {
			t.Errorf("values = %v, want none", cfg.values)
		}
	})
}

// TestConfigAwareFix is the #62 fix-line switch: sentinel fixes become
// flag-naming when no file was loaded, and stay file-naming when one was.
func TestConfigAwareFix(t *testing.T) {
	sentinel := errs.ErrBudgetExceeded.WithFix(
		"raise max_cost_usd in kno.yaml, or re-run with --resume to continue where it stopped",
	).Wrap(fmt.Errorf("sentinel cause"))

	t.Run("no file: the budget-exceeded sentinel names the flag", func(t *testing.T) {
		err := configAwareFix(sentinel, "")
		var a *errs.Actionable
		if !errors.As(err, &a) {
			t.Fatalf("want an Actionable error, got %T", err)
		}
		want := "raise --max-cost-usd, or re-run with --resume to continue where it stopped"
		if a.Fix != want {
			t.Errorf("fix = %q, want %q", a.Fix, want)
		}
	})

	t.Run("a file was loaded: the sentinel stays file-naming", func(t *testing.T) {
		err := configAwareFix(sentinel, "kno.yaml")
		var a *errs.Actionable
		if !errors.As(err, &a) {
			t.Fatalf("want an Actionable error, got %T", err)
		}
		if a.Fix == "raise --max-cost-usd, or re-run with --resume to continue where it stopped" {
			t.Errorf("fix rewritten to the flag text though kno.yaml exists: %q", a.Fix)
		}
	})

	t.Run("a non-sentinel fix is untouched", func(t *testing.T) {
		other := errs.ErrInvalidInput.WithFix("check --evals").Wrap(fmt.Errorf("sentinel cause"))
		err := configAwareFix(other, "")
		var a *errs.Actionable
		if !errors.As(err, &a) {
			t.Fatalf("want an Actionable error, got %T", err)
		}
		if a.Fix != "check --evals" {
			t.Errorf("fix = %q, want the original", a.Fix)
		}
	})

	t.Run("nil error stays nil", func(t *testing.T) {
		if err := configAwareFix(nil, ""); err != nil {
			t.Errorf("configAwareFix(nil) = %v, want nil", err)
		}
	})
}

// TestApplyFileAndEnvCoversEveryKey drives every spec's set closure at least
// once — the file layer with every key present. A set closure that never runs
// is a key that silently never arrives from the file.
func TestApplyFileAndEnvCoversEveryKey(t *testing.T) {
	// The environment is scrubbed to an allowlist by TestMain (see
	// scrubEnvironment), so no KNO_* mirror can leak in and pre-empt the
	// file layer this test exists to exercise.
	cfg := &configFile{values: map[string]any{}, has: map[string]bool{}}
	for key, v := range map[string]any{
		"agent":                 "fake:",
		"goal":                  "exact-match",
		"db":                    "kno.db",
		"concurrency":           4,
		"holdout_frac":          0.2,
		"split_seed":            "42",
		"max_cost_usd":          5.0,
		"max_calls":             int64(10),
		"cost_per_call_usd":     0.5,
		"base_url":              "https://example.com/v1",
		"key_env":               []string{"openai=KNO_KEY"},
		"exec_env":              []string{"PATH"},
		"max_output_tokens":     int64(1024),
		"max_prompt_bytes":      int64(4096),
		"temperature":           0.7,
		"seed":                  int64(42),
		"system":                "grade strictly",
		"generation_params":     "auto",
		"use_legacy_max_tokens": true,
		"timeout":               30 * time.Second,
		"price_input_per_mtok":  3.0,
		"price_output_per_mtok": 15.0,
	} {
		cfg.set(key, v)
	}

	var f baselineFlags
	cmd := newFlagsCmd(&f)
	sources, err := f.applyFileAndEnv(cmd, cfg)
	if err != nil {
		t.Fatalf("applyFileAndEnv: %v", err)
	}

	if f.agentRef != "fake:" || f.goalName != "exact-match" || f.dbPath != "kno.db" {
		t.Errorf("string keys not applied: %+v", f)
	}
	if f.concurrency != 4 || f.holdoutFrac != 0.2 {
		t.Errorf("numeric keys not applied: %+v", f)
	}
	if f.splitSeed != "42" || f.maxCostUSD != 5 || f.maxCalls != 10 {
		t.Errorf("cap keys not applied: %+v", f)
	}
	if f.costPerCall != 0.5 || !f.costPerCallSet {
		t.Errorf("cost_per_call not applied as an explicit claim: %+v", f)
	}
	if f.baseURL != "https://example.com/v1" {
		t.Errorf("base_url not applied: %+v", f)
	}
	if len(f.keyEnv) != 1 || f.keyEnv[0] != "openai=KNO_KEY" || len(f.execEnv) != 1 || f.execEnv[0] != "PATH" {
		t.Errorf("list keys not applied: %+v", f)
	}
	if f.maxOutputTokens != 1024 || f.maxPromptBytes != 4096 {
		t.Errorf("byte/token ceilings not applied: %+v", f)
	}
	if f.temperature != 0.7 || f.seed != 42 || !f.seedSet {
		t.Errorf("sampling keys not applied: %+v", f)
	}
	if f.system != "grade strictly" || f.generationParams != "auto" {
		t.Errorf("prompt keys not applied: %+v", f)
	}
	if !f.useLegacyMaxTokens || f.timeout != 30*time.Second {
		t.Errorf("legacy/timeout keys not applied: %+v", f)
	}
	if f.priceInPerMTok != 3 || f.priceOutPerMTok != 15 {
		t.Errorf("price keys not applied: %+v", f)
	}
	for key := range cfg.values {
		if got := sources[specByKey[key].flag]; got != "file" {
			t.Errorf("source of %q = %q, want file", key, got)
		}
	}
}

// TestSpecKindNameCoversEveryKey pins the type-error wording for every key
// the file accepts, so a schema refusal never names a bare kind number.
func TestSpecKindNameCoversEveryKey(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"key_env":               "list",
		"exec_env":              "list",
		"use_legacy_max_tokens": "true or false",
		"timeout":               "duration like 30s",
		"concurrency":           "whole number",
		"max_calls":             "whole number",
		"max_output_tokens":     "whole number",
		"max_prompt_bytes":      "whole number",
		"seed":                  "whole number",
		"holdout_frac":          "number",
		"max_cost_usd":          "number",
		"cost_per_call_usd":     "number",
		"temperature":           "number",
		"price_input_per_mtok":  "number",
		"price_output_per_mtok": "number",
		"agent":                 "string",
		"goal":                  "string",
		"db":                    "string",
		"split_seed":            "string",
		"base_url":              "string",
		"system":                "string",
		"generation_params":     "string",
	}
	for key, want := range tests {
		if got := specKindName(specByKey[key]); got != want {
			t.Errorf("specKindName(%q) = %q, want %q", key, got, want)
		}
	}
}

// TestParseScalarValueRefusesNonScalars pins that a list or null where a
// scalar belongs is refused at the node layer, before any type conversion.
func TestParseScalarValueRefusesNonScalars(t *testing.T) {
	t.Parallel()
	for _, n := range []*yaml.Node{
		{Kind: yaml.SequenceNode},
		{Kind: yaml.ScalarNode, Tag: "!!null"},
	} {
		if _, err := parseScalarValue(n, func(string) (any, error) { return nil, nil }); err == nil {
			t.Errorf("parseScalarValue(%v) = nil error, want a refusal", n)
		}
	}
}

// TestValidateListEntryRows pins that only key_env speaks the credential
// grammar — exec_env entries pass through untouched.
func TestValidateListEntryRows(t *testing.T) {
	t.Parallel()
	if err := validateListEntry(specByKey["exec_env"], "anything"); err != nil {
		t.Errorf("exec_env entry refused: %v", err)
	}
	err := validateListEntry(specByKey["key_env"], "not-a-binding")
	var a *errs.Actionable
	if !errors.As(err, &a) {
		t.Fatalf("key_env refusal = %T, want an Actionable", err)
	}
	if !strings.Contains(a.Fix, "host=VAR") {
		t.Errorf("fix = %q, want the host=VAR credential grammar", a.Fix)
	}
}

// TestValidateAgentRefRows pins the file-key re-wrap: agentref's plain
// errors and composeRef's Actionables both land with the file fix, never a
// flag fix.
func TestValidateAgentRefRows(t *testing.T) {
	t.Parallel()
	refErr := func(cfg *configFile) {
		err := validateAgentRef(cfg)
		var a *errs.Actionable
		if !errors.As(err, &a) {
			t.Fatalf("validateAgentRef = %T, want an Actionable", err)
		}
		if !strings.Contains(a.Fix, "fix agent or base_url in kno.yaml") {
			t.Errorf("fix = %q, want the kno.yaml fix", a.Fix)
		}
	}
	refErr(&configFile{values: map[string]any{"agent": "not-a-ref"}})
	refErr(&configFile{values: map[string]any{"agent": "openai:m@https://a/v1", "base_url": "https://b/v1"}})
	if err := validateAgentRef(&configFile{values: map[string]any{}}); err != nil {
		t.Errorf("no agent and no base_url refused: %v", err)
	}
}

// TestParseSpecValueRows covers the parse dispatcher's per-kind rows beyond
// what the loader exercises: duration, boolean, and the raw passthrough.
func TestParseSpecValueRows(t *testing.T) {
	t.Parallel()
	if v, err := parseSpecValue(specByKey["timeout"], "90s"); err != nil || v != 90*time.Second {
		t.Errorf("timeout parse = %v, %v", v, err)
	}
	if v, err := parseSpecValue(specByKey["use_legacy_max_tokens"], "true"); err != nil || v != true {
		t.Errorf("bool parse = %v, %v", v, err)
	}
	if v, err := parseSpecValue(specByKey["agent"], "fake:"); err != nil || v != "fake:" {
		t.Errorf("passthrough parse = %v, %v", v, err)
	}
}

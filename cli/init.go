package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/core/errs"
)

// `kno init` writes the one file every command reads (docs/debt.md#62's
// wizard half).
//
// A huh form, in DESIGN's order: agent, key binding, goal, holdout fraction,
// cost caps, concurrency, output directory. Every answer mirrors a flag and a
// KNO_* env var. Re-running MERGES: the form pre-fills from the existing file
// — never from flags or defaults — enter-through means "unchanged", and keys
// the wizard does not cover are round-tripped, so a hand-edited temperature:
// survives re-init. The write is atomic: a crashed write must not corrupt the
// one file every command reads.

func newInitCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a kno.yaml configuration file",
		Long: `Ask the questions every run needs — agent, key binding, goal, caps,
concurrency, output directory — and write the answers to kno.yaml, heavily
commented so a future you can edit it by hand.

Re-running merges: the answers already in the file pre-fill the form, keys
the wizard does not ask about are preserved, and nothing is overwritten until
the file is completely written.

A key VALUE never goes in the file: bind a host to the NAME of an environment
variable, and the value stays where only you can see it.`,
		Example: `  # First run: answer the questions, get a commented kno.yaml
  kno init

  # Re-run after an edit, to see what changed without losing the rest
  kno init`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := cmd.Flags().Lookup("config").Value.String()
			return runInit(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), path, force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false,
		"overwrite an existing kno.yaml without pre-filling or merging")
	addConfigFlag(cmd, defaultConfigPath)
	return cmd
}

// runInit is the wizard's driver.
//
// The cobra run's context is intentionally unused: the form has no
// cancellation surface of its own yet, so it waits — the write is atomic, so
// an interrupt cannot corrupt the file.
func runInit(_ context.Context, in io.Reader, out io.Writer, path string, force bool) error {
	// Load first, so a file this build cannot read is never clobbered. A
	// newer-versioned file is refused EVEN WITH --force: overwriting the
	// future is how data dies.
	existing, loadErr := loadConfigFileAt(path)
	if loadErr != nil {
		if isNewerVersionErr(loadErr) {
			return loadErr
		}
		if fileExists(path) {
			return loadErr
		}
	}

	if !shouldPrompt(in, out) {
		return errs.ErrInvalidInput.WithFix(
			"run `kno init` from a terminal; the wizard is interactive",
		).
			Wrap(fmt.Errorf("kno init needs a terminal for stdin and stdout"))
	}

	// Pre-fill from the file ONLY. Never from flags or defaults: the merge
	// must decide by what the file says, not by what this binary's defaults
	// happen to be.
	prefill := wizardPrefill{}
	if !force && loadErr == nil {
		prefill = prefillFrom(existing)
	}

	answers, err := runWizard(in, out, prefill)
	if err != nil {
		return errs.ErrInvalidInput.WithFix(
			"re-run `kno init` from a terminal",
		).
			Wrap(fmt.Errorf("the init form did not finish: %w", err))
	}

	// Merge: start from the existing file, overlay the answers. Keys the
	// wizard does not ask about survive because they were never touched.
	merged := &configFile{values: map[string]any{}, has: map[string]bool{}}
	if !force && loadErr == nil {
		for k, v := range existing.values {
			merged.set(k, v)
		}
	}
	answers.applyTo(merged)
	merged.set("version", configVersion)

	data, err := renderConfigFile(merged)
	if err != nil {
		return err
	}
	if err := writeConfigFile(path, data); err != nil {
		return errs.ErrInvalidInput.WithFix("check the directory is writable").
			Wrap(fmt.Errorf("writing %s: %w", path, err))
	}
	if _, err := io.WriteString(out,
		fmt.Sprintf("Wrote %s — edit it by hand any time; the answers are commented.\n", path)); err != nil {
		return err
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// isNewerVersionErr reports whether an error is the load-time refusal of a
// file written by a newer build, matched by the sentinel fix the loader
// uses.
func isNewerVersionErr(err error) bool {
	var a *errs.Actionable
	if !errors.As(err, &a) {
		return false
	}
	return a.Fix == "upgrade kno to read this file"
}

// wizardPrefill is the existing file's answers, for the form's initial values.
type wizardPrefill struct {
	agent       string
	keyEnv      string
	goal        string
	holdoutFrac string
	maxCostUSD  string
	maxCalls    string
	concurrency string
	dbPath      string
}

func prefillFrom(cfg *configFile) wizardPrefill {
	p := wizardPrefill{}
	if v, ok := cfg.values["agent"].(string); ok {
		p.agent = v
	}
	if v, ok := cfg.values["key_env"].([]string); ok && len(v) > 0 {
		p.keyEnv = v[0]
	}
	if v, ok := cfg.values["goal"].(string); ok {
		p.goal = v
	}
	if v, ok := cfg.values["holdout_frac"].(float64); ok {
		p.holdoutFrac = strconv.FormatFloat(v, 'f', -1, 64)
	}
	if v, ok := cfg.values["max_cost_usd"].(float64); ok {
		p.maxCostUSD = strconv.FormatFloat(v, 'f', -1, 64)
	}
	if v, ok := cfg.values["max_calls"].(int64); ok {
		p.maxCalls = strconv.FormatInt(v, 10)
	}
	if v, ok := cfg.values["concurrency"].(int); ok {
		p.concurrency = strconv.Itoa(v)
	}
	if v, ok := cfg.values["db"].(string); ok {
		p.dbPath = v
	}
	return p
}

// wizardAnswers is what the form returned. Empty means "don't write the key".
type wizardAnswers struct {
	agent       string
	keyEnv      string
	goal        string
	holdoutFrac string
	maxCostUSD  string
	maxCalls    string
	concurrency string
	dbPath      string
}

// applyTo overlays the answers onto a configFile, skipping empty ones —
// enter-through means "unchanged", and an empty answer keeps the key absent.
func (a wizardAnswers) applyTo(cfg *configFile) {
	setStr := func(key, v string) {
		if v == "" {
			return
		}
		cfg.set(key, v)
	}
	setStr("agent", a.agent)
	if a.keyEnv != "" {
		cfg.set("key_env", []string{a.keyEnv})
	}
	setStr("goal", a.goal)
	if a.holdoutFrac != "" {
		v, err := strconv.ParseFloat(a.holdoutFrac, 64)
		if err != nil { // validated by the form; a direct test can still hit this
			return
		}
		cfg.set("holdout_frac", v)
	}
	if a.maxCostUSD != "" {
		v, err := strconv.ParseFloat(a.maxCostUSD, 64)
		if err != nil {
			return
		}
		cfg.set("max_cost_usd", v)
	}
	if a.maxCalls != "" {
		v, err := strconv.ParseInt(a.maxCalls, 10, 64)
		if err != nil {
			return
		}
		cfg.set("max_calls", v)
	}
	if a.concurrency != "" {
		v, err := strconv.Atoi(a.concurrency)
		if err != nil {
			return
		}
		cfg.set("concurrency", v)
	}
	setStr("db", a.dbPath)
}

// runWizard runs the huh form. Pre-fill values are written into the answer
// variables BEFORE the fields bind to them — the bound value is the initial
// value.
func runWizard(in io.Reader, out io.Writer, prefill wizardPrefill) (wizardAnswers, error) {
	var a wizardAnswers
	a.agent = prefill.agent
	a.keyEnv = prefill.keyEnv
	a.goal = prefill.goal
	a.holdoutFrac = prefill.holdoutFrac
	a.maxCostUSD = prefill.maxCostUSD
	a.maxCalls = prefill.maxCalls
	a.concurrency = prefill.concurrency
	a.dbPath = prefill.dbPath

	agent := huh.NewInput().
		Title("Agent to measure").
		Description("scheme:model — openai:gpt-4.1, anthropic:claude-opus-5, fake:, exec:command").
		Prompt("agent: ").
		Value(&a.agent).
		Validate(func(s string) error {
			if s == "" {
				return fmt.Errorf("an agent is required — fake: costs nothing and needs no key")
			}
			if _, err := agentref.Parse(strings.TrimSpace(s)); err != nil {
				return fmt.Errorf("write the reference as scheme:target, e.g. openai:gpt-4.1")
			}
			return nil
		})

	keyBinding := huh.NewInput().
		Title("Key binding").
		Description("host=VARIABLE_NAME — the NAME of an environment variable, never the key itself").
		Prompt("binding: ").
		Value(&a.keyEnv).
		Validate(func(s string) error {
			if s == "" {
				return nil // optional
			}
			host, envVar, ok := strings.Cut(s, "=")
			if !ok || host == "" || envVar == "" {
				return fmt.Errorf("write host=VAR, naming the environment VARIABLE rather than the key itself")
			}
			return nil
		})

	goal := huh.NewInput().
		Title("Goal").
		Description("what an answer is scored against (exact-match in this build)").
		Prompt("goal: ").
		Value(&a.goal).
		Validate(func(s string) error {
			if s == "" {
				return nil
			}
			if _, err := resolveGoal(strings.TrimSpace(s)); err != nil {
				return fmt.Errorf("only `exact-match` is available in this build")
			}
			return nil
		})

	holdout := huh.NewInput().
		Title("Holdout fraction").
		Description("share of cases held back for validate; enter-through keeps the default 0.2").
		Prompt("holdout_frac: ").
		Value(&a.holdoutFrac).
		Validate(func(s string) error {
			if s == "" {
				return nil
			}
			v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil || v < 0 || v > 1 {
				return fmt.Errorf("a fraction between 0 and 1, like 0.2")
			}
			return nil
		})

	maxCost := huh.NewInput().
		Title("Cost cap, USD").
		Description("stop before spending more than this; empty means no cap").
		Prompt("max_cost_usd: ").
		Value(&a.maxCostUSD).
		Validate(func(s string) error {
			if s == "" {
				return nil
			}
			v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil || v < 0 {
				return fmt.Errorf("a dollar amount, or empty for no cap")
			}
			return nil
		})

	maxCalls := huh.NewInput().
		Title("Call cap").
		Description("stop after this many agent calls; empty means no cap").
		Prompt("max_calls: ").
		Value(&a.maxCalls).
		Validate(func(s string) error {
			if s == "" {
				return nil
			}
			v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if err != nil || v < 0 {
				return fmt.Errorf("a whole number, or empty for no cap")
			}
			return nil
		})

	concurrency := huh.NewInput().
		Title("Concurrency").
		Description("in-flight cases; 0 picks a conservative default").
		Prompt("concurrency: ").
		Value(&a.concurrency).
		Validate(func(s string) error {
			if s == "" {
				return nil
			}
			v, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil || v < 0 {
				return fmt.Errorf("a whole number, or empty for the default")
			}
			return nil
		})

	db := huh.NewInput().
		Title("Output directory").
		Description("where runs and traces are stored (the --db flag's path)").
		Prompt("db: ").
		Value(&a.dbPath).
		Validate(func(s string) error {
			if s == "" {
				return nil
			}
			if strings.ContainsAny(s, "\x00") {
				return fmt.Errorf("a filesystem path, like kno.db")
			}
			return nil
		})

	form := huh.NewForm(
		huh.NewGroup(agent, keyBinding, goal, holdout, maxCost, maxCalls, concurrency, db),
	).WithInput(in).WithOutput(out)

	if err := form.Run(); err != nil {
		return a, err
	}
	return a, nil
}

// renderConfigFile renders the validated file, one commented line per answer.
//
// The wizard's questions are the comments, so a future reader edits the file
// the same way the wizard asked about it. Keys the wizard does not cover are
// still written when present — they were round-tripped, and the comment says
// so honestly.
func renderConfigFile(cfg *configFile) ([]byte, error) {
	var b strings.Builder

	b.WriteString("# kno.yaml — configuration for kno, written by `kno init`.\n")
	b.WriteString("#\n")
	b.WriteString("# Precedence: a flag beats an environment variable beats this file\n")
	b.WriteString("# beats the default. Secrets never go here: bind a VARIABLE NAME.\n")
	b.WriteString("version: 1\n")

	wizardOrder := []struct {
		key     string
		comment string
	}{
		{"agent", "The agent to measure: scheme:model (fake: needs no key)"},
		{"key_env", "Bind a host to the NAME of an env var holding its key"},
		{"goal", "What an answer is scored against"},
		{"holdout_frac", "Share of cases held back for validate"},
		{"max_cost_usd", "Stop before spending more than this (0 is unlimited)"},
		{"max_calls", "Stop after this many agent calls (0 is unlimited)"},
		{"concurrency", "In-flight cases (0 picks a conservative default)"},
		{"db", "Where runs and traces are stored"},
	}
	roundTripped := []struct {
		key     string
		comment string
	}{
		{"base_url", "Endpoint root for a compatible provider"},
		{"split_seed", "Deliberately re-split the evals"},
		{"exec_env", "Grants to the exec: child's environment"},
		{"max_output_tokens", "Generation ceiling"},
		{"max_prompt_bytes", "Refuse a Case whose prompt exceeds this"},
		{"temperature", "Sampling temperature (unset leaves the provider default)"},
		{"seed", "Sampling seed, where the provider supports one"},
		{"system", "System prompt prepended to every Case"},
		{"generation_params", "auto, on, or off"},
		{"use_legacy_max_tokens", "Send max_tokens for older self-hosted servers"},
		{"timeout", "Per-call deadline"},
		{"price_input_per_mtok", "Input price per million tokens (pairs with the next)"},
		{"price_output_per_mtok", "Output price per million tokens"},
	}

	for _, spec := range wizardOrder {
		v, ok := cfg.values[spec.key]
		if !ok {
			continue
		}
		lines, err := renderConfigValue(spec.key, v)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "\n# %s\n%s", spec.comment, lines)
	}

	// Round-tripped keys, kept in the file with the reason made explicit.
	kept := false
	for _, spec := range roundTripped {
		v, ok := cfg.values[spec.key]
		if !ok {
			continue
		}
		if !kept {
			b.WriteString("\n# Preserved from the previous file (the wizard does not ask about these):\n")
			kept = true
		}
		lines, err := renderConfigValue(spec.key, v)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "# %s\n%s", spec.comment, lines)
	}

	return []byte(b.String()), nil
}

// renderConfigValue renders one key's value in yaml, per its type.
//
// String values go through yaml.Marshal, which quotes exactly when needed: a
// written kno.yaml must LOAD back, and `agent: fake:` — the answer every
// example starts with — is not valid YAML, because a plain scalar cannot end
// in a colon. Marshal turns it into `agent: 'fake:'` and leaves the plain
// spellings plain.
func renderConfigValue(key string, v any) (string, error) {
	switch val := v.(type) {
	case string:
		quoted, err := yaml.Marshal(val)
		if err != nil {
			return "", fmt.Errorf("rendering %s: %w", key, err)
		}
		return key + ": " + string(quoted), nil
	case int:
		return fmt.Sprintf("%s: %d\n", key, val), nil
	case int64:
		return fmt.Sprintf("%s: %d\n", key, val), nil
	case float64:
		return key + ": " + strconv.FormatFloat(val, 'f', -1, 64) + "\n", nil
	case bool:
		return fmt.Sprintf("%s: %t\n", key, val), nil
	case time.Duration:
		return key + ": " + val.String() + "\n", nil
	case []string:
		var b strings.Builder
		fmt.Fprintf(&b, "%s:\n", key)
		for _, e := range val {
			quoted, err := yaml.Marshal(e)
			if err != nil {
				return "", fmt.Errorf("rendering %s: %w", key, err)
			}
			fmt.Fprintf(&b, "  - %s", quoted)
		}
		return b.String(), nil
	default:
		return "", fmt.Errorf("rendering %s: unhandled type %T", key, v)
	}
}

// writeConfigFile writes atomically: temp file in the same directory, then
// rename. A crashed write must not corrupt the one file every command reads.
func writeConfigFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".kno-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

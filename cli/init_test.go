package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/knograph/kno/core/errs"
)

// goldenConfig renders to a fixed file: every wizard key plus a sample of
// the round-tripped ones, in the wizard's order and with the wizard's
// comments — a future `kno init` output change is a deliberate diff.
const goldenConfig = `# kno.yaml — configuration for kno, written by ` + "`kno init`" + `.
#
# Precedence: a flag beats an environment variable beats this file
# beats the default. Secrets never go here: bind a VARIABLE NAME.
version: 1

# The agent to measure: scheme:model (fake: needs no key)
agent: 'fake:'

# Bind a host to the NAME of an env var holding its key
key_env:
  - openai=OPENAI_API_KEY

# What an answer is scored against
goal: exact-match

# Share of cases held back for validate
holdout_frac: 0.2

# Stop before spending more than this (0 is unlimited)
max_cost_usd: 5

# Stop after this many agent calls (0 is unlimited)
max_calls: 100

# In-flight cases (0 picks a conservative default)
concurrency: 0

# Where runs and traces are stored
db: kno.db

# Preserved from the previous file (the wizard does not ask about these):
# Endpoint root for a compatible provider
base_url: https://example.com/v1
# Sampling temperature (unset leaves the provider default)
temperature: 0.7
# Send max_tokens for older self-hosted servers
use_legacy_max_tokens: true
# Per-call deadline
timeout: 30s
# Input price per million tokens (pairs with the next)
price_input_per_mtok: 3
# Output price per million tokens
price_output_per_mtok: 15
`

func goldenCfg() *configFile {
	cfg := &configFile{values: map[string]any{}, has: map[string]bool{}}
	for k, v := range map[string]any{
		"version":               1,
		"agent":                 "fake:",
		"key_env":               []string{"openai=OPENAI_API_KEY"},
		"goal":                  "exact-match",
		"holdout_frac":          0.2,
		"max_cost_usd":          5.0,
		"max_calls":             int64(100),
		"concurrency":           0,
		"db":                    "kno.db",
		"base_url":              "https://example.com/v1",
		"temperature":           0.7,
		"use_legacy_max_tokens": true,
		"timeout":               30 * time.Second,
		"price_input_per_mtok":  3.0,
		"price_output_per_mtok": 15.0,
	} {
		cfg.set(k, v)
	}
	return cfg
}

func TestRenderConfigFileGolden(t *testing.T) {
	data, err := renderConfigFile(goldenCfg())
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	if got := string(data); got != goldenConfig {
		t.Errorf("rendered kno.yaml differs from the golden.\n--- got ---\n%s\n--- want ---\n%s", got, goldenConfig)
	}
}

func TestRenderConfigFileOmitsAbsentKeys(t *testing.T) {
	data, err := renderConfigFile(&configFile{values: map[string]any{"version": 1}, has: map[string]bool{"version": true}})
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "agent:") {
		t.Errorf("an absent agent leaked into the file:\n%s", got)
	}
	if strings.Contains(got, "Preserved from the previous file") {
		t.Errorf("the preserved section rendered with nothing to preserve:\n%s", got)
	}
}

// TestPrefillFromAndMerge is the re-run contract: the file pre-fills the
// form, enter-through keeps a key absent, and keys the wizard does not ask
// about survive the merge untouched.
func TestPrefillFromAndMerge(t *testing.T) {
	cfg := &configFile{values: map[string]any{}, has: map[string]bool{}}
	cfg.set("version", 1)
	cfg.set("agent", "anthropic:claude-opus-5")
	cfg.set("key_env", []string{"anthropic=ANTHROPIC_API_KEY"})
	cfg.set("temperature", 0.7)

	prefill := prefillFrom(cfg)
	if prefill.agent != "anthropic:claude-opus-5" {
		t.Errorf("prefill.agent = %q, want the file's agent", prefill.agent)
	}
	if prefill.keyEnv != "anthropic=ANTHROPIC_API_KEY" {
		t.Errorf("prefill.keyEnv = %q, want the file's first binding", prefill.keyEnv)
	}

	// The human changed the agent and left everything else alone; an empty
	// holdout answer must not write a holdout_frac.
	answers := wizardAnswers{agent: "fake:"}
	merged := &configFile{values: map[string]any{}, has: map[string]bool{}}
	for k, v := range cfg.values {
		merged.set(k, v)
	}
	answers.applyTo(merged)
	merged.set("version", configVersion)

	if got := merged.values["agent"]; got != "fake:" {
		t.Errorf("agent = %v, want the new answer", got)
	}
	if got := merged.values["temperature"]; got != 0.7 {
		t.Errorf("temperature = %v, want 0.7 preserved (the wizard does not ask about it)", got)
	}
	if _, ok := merged.values["holdout_frac"]; ok {
		t.Error("holdout_frac was written from an empty answer; enter-through means unchanged")
	}
}

// initFeed is the keystrokes that answer the form: ctrl+u clears any
// prefilled agent, then "fake:", then every other field entered through
// empty. Eight fields, eight enters. The enters are CR: a real terminal sends
// 0x0D for Enter, and bubbletea parses a bare LF as ctrl+j, not enter.
const initFeed = "\x15fake:\r\r\r\r\r\r\r\r"

// runInitPTY drives `kno init` end-to-end over a pty and returns the
// rendered file's content.
func runInitPTY(t *testing.T, path string, force bool, feed string) string {
	t.Helper()
	t.Setenv("TERM", "xterm-256color")

	m, s := ptyPair(t)
	// The wizard renders to the slave; draining the master keeps the pty
	// buffer from filling and deadlocking the form.
	rendered := make(chan struct{})
	go func() {
		// Wait for the first frame before the test feeds: the form sets raw
		// mode with TCSAFLUSH, which DISCARDS whatever was queued before it —
		// bytes fed too early are echoed back by the canonical line
		// discipline and then flushed, and the form never sees them.
		var one [64]byte
		if _, err := m.Read(one[:]); err == nil {
			close(rendered)
		}
		_, _ = io.Copy(io.Discard, m)
	}()
	done := make(chan error, 1)
	go func() {
		done <- runInit(t.Context(), s, s, path, force)
	}()
	select {
	case <-rendered:
	case <-time.After(10 * time.Second):
		t.Fatal("the wizard never rendered its first frame")
	}
	if _, err := m.Write([]byte(feed)); err != nil {
		t.Fatalf("feeding the wizard: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runInit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wizard stuck; the form did not finish within 10s")
	}
	// bubbletea's cursor-blink timers outlive Program.Run by up to one
	// BlinkSpeed (530ms); their Send is canceled, so they die on the next
	// tick, but the package-level goleak check would catch the transient
	// goroutines. Wait them out.
	time.Sleep(700 * time.Millisecond)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func TestInitWritesACommentedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kno.yaml")
	got := runInitPTY(t, path, false, initFeed)

	if !strings.Contains(got, "version: 1") {
		t.Errorf("file lacks version: 1:\n%s", got)
	}
	if !strings.Contains(got, "agent: 'fake:'") {
		t.Errorf("file lacks the agent answer:\n%s", got)
	}
	// Enter-through answers stay absent: no goal, no caps, no db.
	for _, absent := range []string{"goal:", "max_cost_usd:", "max_calls:", "db:", "concurrency:"} {
		if strings.Contains(got, absent) {
			t.Errorf("file contains %q from an empty answer:\n%s", absent, got)
		}
	}
	if strings.Contains(got, "sk-") {
		t.Error("a key value landed in the file")
	}
}

func TestInitMergesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "kno.yaml",
		"version: 1\nagent: anthropic:claude-opus-5\ntemperature: 0.7\n"+
			"price_input_per_mtok: 3\nprice_output_per_mtok: 15\n")
	got := runInitPTY(t, path, false, initFeed)

	if !strings.Contains(got, "agent: 'fake:'") {
		t.Errorf("the new agent answer is missing:\n%s", got)
	}
	if !strings.Contains(got, "temperature: 0.7") {
		t.Errorf("temperature was not round-tripped:\n%s", got)
	}
	if !strings.Contains(got, "Preserved from the previous file") {
		t.Errorf("the round-trip section is missing:\n%s", got)
	}
	if !strings.Contains(got, "price_input_per_mtok: 3") {
		t.Errorf("the price pair was not round-tripped:\n%s", got)
	}
}

func TestInitAtomicWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kno.yaml")
	runInitPTY(t, path, false, initFeed)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".kno-") {
			t.Errorf("a temp file survived the write: %s", e.Name())
		}
	}
}

func TestInitRefusesANewerVersionEvenWithForce(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "kno.yaml", "version: 2\nagent: 'future:'\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}

	_, s := ptyPair(t)
	done := make(chan error, 1)
	go func() { done <- runInit(t.Context(), s, s, path, true) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a newer-versioned file must be refused even with --force")
		}
		if !strings.Contains(err.Error(), "upgrade kno") {
			t.Errorf("error = %q, want the upgrade-kno fix", err.Error())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("refusal did not return")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the newer-versioned file was overwritten; the refusal must not touch it")
	}
}

func TestInitRefusesAnUnparseableFile(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "kno.yaml", "version: [1\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}

	_, s := ptyPair(t)
	done := make(chan error, 1)
	go func() { done <- runInit(t.Context(), s, s, path, false) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an unparseable file must be refused, not clobbered")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("refusal did not return")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the unparseable file was overwritten; the refusal must not touch it")
	}
}

func TestInitRefusesANonTerminal(t *testing.T) {
	var in, out bytes.Buffer
	err := runInit(t.Context(), &in, &out, filepath.Join(t.TempDir(), "kno.yaml"), false)
	if err == nil {
		t.Fatal("kno init off a terminal must refuse")
	}
	if !strings.Contains(err.Error(), "from a terminal") {
		t.Errorf("error = %q, want the from-a-terminal refusal", err.Error())
	}
}

func TestWriteConfigFileReplacesAndIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kno.yaml")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if err := writeConfigFile(path, []byte("new\n")); err != nil {
		t.Fatalf("writeConfigFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(data) != "new\n" {
		t.Errorf("content = %q, want new", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".kno-") {
			t.Errorf("a temp file survived: %s", e.Name())
		}
	}
}

// TestApplyTo overlays answers onto a file: every key lands, empty answers
// skip, and a parse failure silently skips the key (the form validates first;
// the silent skip is the defensive backstop).
func TestApplyTo(t *testing.T) {
	t.Parallel()
	full := wizardAnswers{
		agent:       "fake:",
		keyEnv:      "openai=OPENAI_API_KEY",
		goal:        "exact-match",
		holdoutFrac: "0.2",
		maxCostUSD:  "5",
		maxCalls:    "100",
		concurrency: "4",
		dbPath:      "runs.db",
	}
	cfg := &configFile{values: map[string]any{}, has: map[string]bool{}}
	full.applyTo(cfg)
	if cfg.values["agent"] != "fake:" || cfg.values["goal"] != "exact-match" || cfg.values["db"] != "runs.db" {
		t.Errorf("string answers not written: %v", cfg.values)
	}
	if len(cfg.values["key_env"].([]string)) != 1 || cfg.values["key_env"].([]string)[0] != "openai=OPENAI_API_KEY" {
		t.Errorf("key_env not written: %v", cfg.values["key_env"])
	}
	if cfg.values["holdout_frac"] != 0.2 || cfg.values["max_cost_usd"] != 5.0 {
		t.Errorf("float answers not written: %v", cfg.values)
	}
	if cfg.values["max_calls"] != int64(100) || cfg.values["concurrency"] != 4 {
		t.Errorf("int answers not written: %v", cfg.values)
	}

	empty := wizardAnswers{}
	cfg = &configFile{values: map[string]any{}, has: map[string]bool{}}
	empty.applyTo(cfg)
	if len(cfg.values) != 0 {
		t.Errorf("empty answers wrote keys: %v", cfg.values)
	}

	for _, bad := range []wizardAnswers{
		{holdoutFrac: "oops"},
		{maxCostUSD: "oops"},
		{maxCalls: "oops"},
		{concurrency: "oops"},
	} {
		cfg = &configFile{values: map[string]any{}, has: map[string]bool{}}
		bad.applyTo(cfg)
		if len(cfg.values) != 0 {
			t.Errorf("parse failure for %+v wrote keys: %v", bad, cfg.values)
		}
	}
}

// TestPrefillFromEveryType drives the type assertions: every numeric kind
// renders to the string the form will re-validate, and an absent or empty
// key_env pre-fills nothing.
func TestPrefillFromEveryType(t *testing.T) {
	t.Parallel()
	cfg := &configFile{values: map[string]any{}, has: map[string]bool{}}
	cfg.set("agent", "anthropic:claude-opus-5")
	cfg.set("goal", "exact-match")
	cfg.set("db", "traces.db")
	cfg.set("holdout_frac", 0.2)
	cfg.set("max_cost_usd", 2.5)
	cfg.set("max_calls", int64(7))
	cfg.set("concurrency", 3)

	p := prefillFrom(cfg)
	if p.agent != "anthropic:claude-opus-5" || p.goal != "exact-match" || p.dbPath != "traces.db" {
		t.Errorf("string prefill wrong: %+v", p)
	}
	if p.holdoutFrac != "0.2" || p.maxCostUSD != "2.5" {
		t.Errorf("float prefill wrong: %+v", p)
	}
	if p.maxCalls != "7" || p.concurrency != "3" {
		t.Errorf("int prefill wrong: %+v", p)
	}
	if p.keyEnv != "" {
		t.Errorf("keyEnv = %q, want empty with no key_env in the file", p.keyEnv)
	}

	cfg.set("key_env", []string{})
	if p2 := prefillFrom(cfg); p2.keyEnv != "" {
		t.Errorf("keyEnv = %q, want empty when the list is empty", p2.keyEnv)
	}
}

// wizardFeedChunks answers every field with an invalid value first, then a
// valid one — except the last, which is enter-through empty. One write per
// chunk: bubbletea delivers each write as one key batch, and the form only
// moves focus after the batch is processed, so keys that depend on the
// previous field's outcome must arrive in their own write.
var wizardFeedChunks = []string{
	"nope\r", "\x15fake:\r",
	"nopair\r", "\x15host=KNO_KEY\r",
	"fuzzy\r", "\x15\r",
	"3\r", "\x150.2\r",
	"-5\r", "\x150.5\r",
	"-1\r", "\x15100\r",
	"-2\r", "\x154\r",
	"\r",
}

// runWizardPTY drives the form directly over a pty, same discipline as
// runInitPTY: wait for the first frame (raw-mode TCSAFLUSH discards early
// input), feed, drain, wait out the cursor-blink timer.
func runWizardPTY(t *testing.T, chunks []string) wizardAnswers {
	t.Helper()
	t.Setenv("TERM", "xterm-256color")

	m, s := ptyPair(t)
	rendered := make(chan struct{})
	frames := make(chan struct{}, 16)
	go func() {
		var one [64]byte
		if _, err := m.Read(one[:]); err == nil {
			close(rendered)
		}
		buf := make([]byte, 4096)
		for {
			n, err := m.Read(buf)
			if n > 0 {
				select {
				case frames <- struct{}{}:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()
	done := make(chan struct{})
	var answers wizardAnswers
	go func() {
		defer close(done)
		var err error
		answers, err = runWizard(s, s, wizardPrefill{})
		if err != nil {
			t.Errorf("runWizard: %v", err)
		}
	}()
	select {
	case <-rendered:
	case <-time.After(10 * time.Second):
		t.Fatal("the wizard never rendered its first frame")
	}
	for _, chunk := range chunks {
		if _, err := m.Write([]byte(chunk)); err != nil {
			t.Fatalf("feeding the wizard %q: %v", chunk, err)
		}
		select {
		case <-frames:
		case <-time.After(10 * time.Second):
			t.Fatalf("the wizard stopped rendering after %q", chunk)
		}
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the wizard did not finish within 10s")
	}
	time.Sleep(700 * time.Millisecond) // cursor-blink timers outlive Program.Run
	return answers
}

// TestRunWizardRepromptsOnInvalidInput proves each field's validator is
// actually consulted: an invalid value keeps the form on the field, and the
// corrected value is what lands in the answers.
func TestRunWizardRepromptsOnInvalidInput(t *testing.T) {
	a := runWizardPTY(t, wizardFeedChunks)
	if a.agent != "fake:" {
		t.Errorf("agent = %q, want fake:", a.agent)
	}
	if a.keyEnv != "host=KNO_KEY" {
		t.Errorf("keyEnv = %q, want host=KNO_KEY", a.keyEnv)
	}
	if a.goal != "" {
		t.Errorf("goal = %q, want empty (enter-through after an invalid value)", a.goal)
	}
	if a.holdoutFrac != "0.2" {
		t.Errorf("holdoutFrac = %q, want 0.2", a.holdoutFrac)
	}
	if a.maxCostUSD != "0.5" {
		t.Errorf("maxCostUSD = %q, want 0.5", a.maxCostUSD)
	}
	if a.maxCalls != "100" {
		t.Errorf("maxCalls = %q, want 100", a.maxCalls)
	}
	if a.concurrency != "4" {
		t.Errorf("concurrency = %q, want 4", a.concurrency)
	}
	if a.dbPath != "" {
		t.Errorf("dbPath = %q, want empty", a.dbPath)
	}
}

// TestNewInitCmdRunERefusesNonTerminal covers the cobra seam: a non-terminal
// stdin refuses before any file is touched.
func TestNewInitCmdRunERefusesNonTerminal(t *testing.T) {
	cmd := newInitCmd()
	cmd.SetArgs([]string{})
	cmd.SetIn(bytes.NewReader(nil))
	cmd.SetOut(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatalf("Execute() on a non-terminal = %v, want the terminal refusal", err)
	}
}

// TestIsNewerVersionErrRows pins the sentinel match: the loader's fix string
// is the marker, and a wrapped refusal of any other shape is not one.
func TestIsNewerVersionErrRows(t *testing.T) {
	t.Parallel()
	loader := errs.ErrInvalidInput.WithFix("upgrade kno to read this file").
		Wrap(fmt.Errorf("version 2 needs a newer build"))
	if !isNewerVersionErr(loader) {
		t.Error("the loader's refusal was not recognized as a newer-version error")
	}
	if isNewerVersionErr(errs.ErrInvalidInput.WithFix("fix agent or base_url in kno.yaml")) {
		t.Error("an unrelated Actionable was misrecognized as a newer-version error")
	}
	if isNewerVersionErr(errors.New("plain error")) {
		t.Error("a plain error was misrecognized as a newer-version error")
	}
	if isNewerVersionErr(nil) {
		t.Error("nil was misrecognized as a newer-version error")
	}
}

// TestWriteConfigFileRefusesMissingDir covers the CreateTemp failure: an
// unwritable directory refuses before any partial file exists.
func TestWriteConfigFileRefusesMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "kno.yaml")
	if err := writeConfigFile(path, []byte("version: 1\n")); err == nil {
		t.Fatal("writeConfigFile into a missing directory succeeded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a file appeared at %s: %v", path, err)
	}
}

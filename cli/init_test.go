package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	case <-time.After(30 * time.Second):
		t.Fatal("the wizard did not finish within 30s")
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

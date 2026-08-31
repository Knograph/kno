package cli

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The defaults-parity gate.
//
// `kno demo` builds its five flag structs as LITERALS. A keyed composite
// literal that omits a field takes Go's ZERO value — not the value the command
// registered with cobra — and nothing in the toolchain says so: adding a field
// to a struct does not break a keyed literal, and adding a flag registration
// does not touch the struct at all. So every non-zero registered default the
// demo does not hand-copy is a silent divergence, and the list of them grows
// every time a flag is added to any of the five commands.
//
// The worked example is --holdout-frac. `baseline` registers
// jsonl.DefaultHoldoutFrac (0.2); a demo literal that omits holdoutFrac passes
// 0. The BEHAVIOR survives — jsonl.Options.holdoutFrac maps <= 0 back to
// DefaultHoldoutFrac, so the split still happens and the too-small-holdout
// warning still prints — but core.BaselineOptions.HoldoutFrac is recorded on
// the Run verbatim, so the store would keep 0 for a run that split at 0.2.
// This test therefore guards a RECORDED-VALUE bug rather than a behavioral
// one, which is the kind that is found late and believed in the meantime.
//
// The rule below is deliberately narrow, and the narrowness is the point: a
// flag whose registered default is Go's zero value cannot diverge, so it needs
// no entry. Everything else must either be copied into the demo's literal
// (checked here, by value) or named in demoOverrides with a reason. A new flag
// with a non-zero default fails on the day it is added, which is the only day
// the fix is cheap.
//
// The stronger design — constructing the structs FROM the registered defaults
// — lost on blast radius: the defaults are written into a closure-local `f`
// inside each newXCmd(), so it would mean refactoring flag registration out of
// five command constructors, four of which are spend paths, to serve a free
// demo. If that extraction ever happens for another reason, the demo should
// switch to it and this test becomes the regression net for the extraction.
//
// Deleting or skipping this test puts the "the demo bypasses configuration"
// accepted risk back to being unbounded. Read that sentence twice.

// demoZeroDefaults are the DefValue strings pflag renders for a Go zero value,
// one per flag type the five commands use: string, numeric, bool,
// StringArray, and Duration.
var demoZeroDefaults = map[string]bool{
	"":      true, // StringVar
	"0":     true, // IntVar, Int32Var, Int64Var, Float64Var
	"false": true, // BoolVar
	"[]":    true, // StringArrayVar
	"0s":    true, // DurationVar
}

// demoParity is one command's registered flags against the demo's literal.
type demoParity struct {
	command string
	cmd     *cobra.Command

	// literal maps a flag name to the demo's literal value, READ FROM the
	// demo's own struct and rendered the way pflag renders a default. Read,
	// not restated: a hand-typed constant here would prove nothing.
	literal map[string]string

	// overrides names the flags the demo deliberately sets to something other
	// than the registered default, each with the reason.
	overrides map[string]string
}

// checkDemoParity returns one finding per divergence, in a stable order.
//
// Split out from the test so the mutation test below can drive it with a
// deliberately-broken literal and assert that it fails.
func checkDemoParity(p demoParity) []string {
	var findings []string
	registered := map[string]bool{}

	p.cmd.Flags().VisitAll(func(fl *pflag.Flag) {
		if fl.Name == "help" {
			return
		}
		registered[fl.Name] = true

		if why, ok := p.overrides[fl.Name]; ok {
			if strings.TrimSpace(why) == "" {
				findings = append(findings, fmt.Sprintf(
					"%s --%s is in demoOverrides with no reason; an override without one "+
						"is an omission wearing an allowlist's clothes", p.command, fl.Name))
			}
			return
		}
		if want, ok := p.literal[fl.Name]; ok {
			if want != fl.DefValue {
				findings = append(findings, fmt.Sprintf(
					"%s --%s: the demo's literal is %q but the command registers %q",
					p.command, fl.Name, want, fl.DefValue))
			}
			return
		}
		if !demoZeroDefaults[fl.DefValue] {
			findings = append(findings, fmt.Sprintf(
				"%s --%s registers the non-zero default %q, and the demo's literal flag "+
					"struct neither copies it nor names it in demoOverrides — so the demo "+
					"silently runs with Go's zero value instead",
				p.command, fl.Name, fl.DefValue))
		}
	})

	for name := range p.literal {
		if !registered[name] {
			findings = append(findings, fmt.Sprintf(
				"%s: the parity table names --%s, which the command no longer registers",
				p.command, name))
		}
	}
	for name := range p.overrides {
		if !registered[name] {
			findings = append(findings, fmt.Sprintf(
				"%s: demoOverrides names --%s, which the command no longer registers",
				p.command, name))
		}
	}

	sort.Strings(findings)
	return findings
}

// demoFloatDefault renders a float the way pflag's float64Value.String does.
func demoFloatDefault(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// demoParityTable builds the five subjects from the demo's own flag structs.
func demoParityTable() []demoParity {
	p := demoPathsFor(demoDefaultDir, false)
	b := demoBaselineFlags(p)
	v := demoValueFlags(p)

	// The reasons the demo diverges. Every stage-linking ID, every path, and
	// every budget cap is here: those ARE the demo, and they cannot be the
	// command's defaults.
	pathsAndIDs := func(extra map[string]string) map[string]string {
		m := map[string]string{
			"db":     "the DB lives inside --dir, never the user's ./kno.db",
			"run-id": "fixed IDs, so the epilogue and the docs can name them",
		}
		for k, why := range extra {
			m[k] = why
		}
		return m
	}

	return []demoParity{
		{
			command: "baseline",
			cmd:     newBaselineCmd(),
			literal: map[string]string{
				"agent":        b.agentRef,
				"goal":         b.goalName,
				"holdout-frac": demoFloatDefault(b.holdoutFrac),
				// baseline registers math.NaN() — "leave the provider default
				// alone". value registers 0. The two literals legitimately
				// differ here, and copying one into the other is exactly the
				// divergence this test exists to catch.
				"temperature": demoFloatDefault(b.temperature),
			},
			overrides: pathsAndIDs(map[string]string{
				"evals": "the demo's own embedded Cases",
				"config": "the demo calls neither loadConfigFile nor applyFileAndEnv, " +
					"so --config has no meaning here; it is not accepted on `kno demo` either",
			}),
		},
		{
			command: "value",
			cmd:     newValueCmd(),
			literal: map[string]string{
				"agent":        v.agentRef,
				"goal":         v.goalName,
				"routing-seed": strconv.FormatInt(v.routingSeed, 10),
				"temperature":  demoFloatDefault(v.temperature),
			},
			overrides: pathsAndIDs(map[string]string{
				"evals":           "the demo's own embedded Cases",
				"pool":            "the demo's own embedded Assets",
				"baseline-run-id": "the stage link: this run pairs against demo-baseline",
				"config":          "see baseline",
			}),
		},
		{
			command: "select",
			cmd:     newSelectCmd(),
			literal: map[string]string{},
			overrides: pathsAndIDs(map[string]string{
				"value-run-id":          "the stage link",
				"pool":                  "naming it enables the content rules",
				"max-context-tokens":    "select refuses a run with no cap at all",
				"max-training-examples": "select refuses a run with no cap at all",
				"max-cost-usd":          "select refuses a run with no cap at all",
			}),
		},
		{
			command: "export",
			cmd:     newExportCmd(),
			literal: map[string]string{},
			overrides: pathsAndIDs(map[string]string{
				"select-run-id": "the stage link",
				"destination":   "tuning_set, the grammar the tape shows",
				"pool":          "the demo's own embedded Assets",
				"out":           "the artifact lives inside --dir",
			}),
		},
		{
			command: "report",
			cmd:     newReportCmd(),
			literal: map[string]string{},
			overrides: map[string]string{
				"db":            "the DB lives inside --dir, never the user's ./kno.db",
				"value-run-id":  "the stage link",
				"select-run-id": "the stage link",
			},
		},
	}
}

// TestDemoFlagDefaultsMatchTheRegisteredOnes is acceptance criterion 19.
func TestDemoFlagDefaultsMatchTheRegisteredOnes(t *testing.T) {
	t.Parallel()

	for _, subject := range demoParityTable() {
		t.Run(subject.command, func(t *testing.T) {
			t.Parallel()
			for _, finding := range checkDemoParity(subject) {
				t.Error(finding)
			}
		})
	}
}

// TestDemoParityCatchesADroppedDefault is the mutation test: a parity gate
// that cannot fail is decoration.
//
// It drops --holdout-frac from baseline's literal, the way an author would by
// forgetting the field, and asserts the check names the flag and both values.
func TestDemoParityCatchesADroppedDefault(t *testing.T) {
	t.Parallel()

	var baseline demoParity
	for _, subject := range demoParityTable() {
		if subject.command == "baseline" {
			baseline = subject
		}
	}
	if baseline.cmd == nil {
		t.Fatal("the parity table no longer covers baseline")
	}

	// What a demoBaselineFlags literal without holdoutFrac would produce.
	broken := baseline
	broken.literal = map[string]string{}
	for k, v := range baseline.literal {
		broken.literal[k] = v
	}
	broken.literal["holdout-frac"] = demoFloatDefault(0)

	findings := checkDemoParity(broken)
	if len(findings) == 0 {
		t.Fatal("the parity check passed a demo literal with holdout-frac at Go's zero " +
			"value; it would not catch the bug it exists for")
	}
	joined := strings.Join(findings, "\n")
	for _, want := range []string{"holdout-frac", `"0"`, `"0.2"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("the finding does not name %s:\n%s", want, joined)
		}
	}
}

// TestDemoParityCatchesANewNonZeroDefault: the drift this gate exists for is
// a flag added TOMORROW, not one that exists today.
func TestDemoParityCatchesANewNonZeroDefault(t *testing.T) {
	t.Parallel()

	cmd := newSelectCmd()
	cmd.Flags().String("some-new-flag", "a-non-zero-default", "added after the demo shipped")

	findings := checkDemoParity(demoParity{
		command:   "select",
		cmd:       cmd,
		literal:   map[string]string{},
		overrides: map[string]string{},
	})
	if !strings.Contains(strings.Join(findings, "\n"), "some-new-flag") {
		t.Errorf("a newly registered flag with a non-zero default did not fail the gate:\n%v",
			findings)
	}
}

// TestDemoNeverWaivesConsent is acceptance criterion 10.
//
// Nothing in the demo prompts: fake.Agent implements no WorstCase(), so it is
// not a core.Estimator, so PlanningCostPerCall falls through to
// EstCostPerCallUSDMicros — zero for a literal struct — and consentDialog
// returns before the TTY check matters. The property that matters is that the
// demo never sets the bypass: if a spending agent were ever wired into this
// path, the guard must fire rather than find a --yes sitting in the code
// silently pre-approving it.
func TestDemoNeverWaivesConsent(t *testing.T) {
	t.Parallel()

	for _, jsonOut := range []bool{false, true} {
		p := demoPathsFor(demoDefaultDir, jsonOut)
		if demoBaselineFlags(p).yes {
			t.Errorf("the demo's baseline flags waive consent (jsonOut=%v)", jsonOut)
		}
		if demoValueFlags(p).yes {
			t.Errorf("the demo's value flags waive consent (jsonOut=%v)", jsonOut)
		}
		if demoBaselineFlags(p).jsonOut != jsonOut || demoValueFlags(p).jsonOut != jsonOut {
			t.Errorf("the demo's --json did not reach the stage flags (jsonOut=%v)", jsonOut)
		}
	}
}

// TestDemoPinsTheFreeAgent: the one field that makes the whole command free.
func TestDemoPinsTheFreeAgent(t *testing.T) {
	t.Parallel()

	p := demoPathsFor(demoDefaultDir, false)
	for name, ref := range map[string]string{
		"baseline": demoBaselineFlags(p).agentRef,
		"value":    demoValueFlags(p).agentRef,
	} {
		if ref != "fake:" {
			t.Errorf("the demo's %s stage runs %q, not fake:", name, ref)
		}
	}
	if !math.IsNaN(demoBaselineFlags(p).temperature) {
		t.Error("the demo's baseline temperature is set, so it would send one instead of " +
			"leaving the provider default alone")
	}
}

// TestDemoRefusesTheCurrentDirectory covers the paths refuseDemoCwd guards
// that a black-box run cannot reach portably.
func TestDemoRefusesTheCurrentDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	for _, arg := range []string{".", dir, filepath.Join(dir, "sub", "..")} {
		if err := refuseDemoCwd(filepath.Clean(arg)); err == nil {
			t.Errorf("--dir %q resolved to the current directory and was accepted", arg)
		}
	}
	if err := refuseDemoCwd(filepath.Join(dir, "kno-demo")); err != nil {
		t.Errorf("a subdirectory was refused: %v", err)
	}
}

// TestDemoProbeRefusesAnUnwritableDirectory: --force aborts before any stage
// runs rather than clearing half a directory.
func TestDemoProbeRefusesAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to a 0500 directory, so the probe cannot fail")
	}
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatalf("creating the locked directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := demoProbeWritable(dir)
	if err == nil {
		t.Fatal("an unwritable directory passed the pre-flight")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("the refusal does not name the directory: %v", err)
	}
}

// TestWrapIndentIsTheOnlyThingThatReshapesANote.
//
// The human/--json equivalence rests on the notes being one string in the
// source that only this function reflows, so the reflow must be lossless.
func TestWrapIndentIsTheOnlyThingThatReshapesANote(t *testing.T) {
	t.Parallel()

	for _, note := range demoNotes() {
		wrapped := wrapIndent(note, demoWrapWidth, "  ")
		if got := strings.Join(strings.Fields(wrapped), " "); got != note {
			t.Errorf("wrapping lost or added words.\ngot:  %s\nwant: %s", got, note)
		}
		for _, line := range strings.Split(strings.TrimRight(wrapped, "\n"), "\n") {
			if !strings.HasPrefix(line, "  ") {
				t.Errorf("a wrapped line lost its indent: %q", line)
			}
			if len(line) > demoWrapWidth {
				// A single word longer than the width is the only legal
				// overflow, and none of the notes contain one.
				t.Errorf("a wrapped line is %d columns wide: %q", len(line), line)
			}
		}
	}
	if got := wrapIndent("   ", demoWrapWidth, "  "); got != "" {
		t.Errorf("wrapping whitespace produced %q, not an empty block", got)
	}
}

// TestDemoIgnoresEveryConfigKnob is acceptance criterion 17, and it reads the
// KNO_* names from configSpecs itself so a new key cannot be forgotten.
//
// Not parallel: it sets process environment, and the CLI runs in-process.
func TestDemoIgnoresEveryConfigKnob(t *testing.T) {
	clean := t.TempDir()
	poisoned := t.TempDir()

	t.Chdir(clean)
	before := runDemoForTest(t)

	t.Chdir(poisoned)
	for _, spec := range configSpecs {
		t.Setenv(spec.env, "poison")
	}
	t.Setenv("KNO_AGENT", "openai:gpt-4.1")
	t.Setenv("KNO_DB", filepath.Join(poisoned, "elsewhere.db"))
	yaml := "config_version: 1\nagent: openai:gpt-4.1\ndb: elsewhere.db\n"
	if err := os.WriteFile(filepath.Join(poisoned, "kno.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing the poison kno.yaml: %v", err)
	}
	after := runDemoForTest(t)

	if before != after {
		t.Errorf("configuration changed the demo's transcript.\nclean:\n%s\npoisoned:\n%s",
			before, after)
	}
	if _, err := os.Stat(filepath.Join(poisoned, "elsewhere.db")); err == nil {
		t.Error("the demo wrote to the DB path KNO_DB named")
	}
}

// runDemoForTest runs `kno demo` in the current directory and returns stdout.
func runDemoForTest(t *testing.T) string {
	t.Helper()

	var out, errOut strings.Builder
	code := Execute(t.Context(), []string{"demo"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("demo exit = %d\nstderr: %s", code, errOut.String())
	}
	return out.String()
}

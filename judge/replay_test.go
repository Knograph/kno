package judge_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knograph/kno/judge"
)

// writeFixtures records a prompted goal's judgements to disk, the way
// `make record-calibration` will.
func writeFixtures(t *testing.T, root, goalName, promptSHA string, set *judge.Set, verdicts map[string]bool) {
	t.Helper()
	dir := filepath.Join(root, goalName, promptSHA)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Written by hand rather than with encoding/json: this package's exemption
	// is scoped to format.go, and a test that marshaled its own fixtures would
	// widen it.
	for _, r := range set.Records {
		v := verdicts[r.ID]
		b := fmt.Sprintf(
			`{"record_id":%q,"prompt_sha":%q,"judge_model":"test-judge-1",`+
				`"value":%g,"passed":%t,"rationale":"recorded"}`,
			r.ID, promptSHA, boolValue(v), v)
		if err := os.WriteFile(filepath.Join(dir, r.ID+".json"), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReplayMakesNoProviderCall is replay purity, asserted rather than
// inspected: the goal's provider calls t.Fatal, so a replay that reaches it
// fails here instead of quietly producing a number from a live call.
func TestReplayMakesNoProviderCall(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	g := &promptedGoal{t: t, prompt: "is this answer correct?"}
	sha := judge.PromptSHA(g)

	root := t.TempDir()
	writeFixtures(t, root, "prompted", sha, set, truthOf(set))

	res := calibrate(t, judge.Options{
		Goal: g, GoalName: "prompted", Set: set,
		Fixtures: judge.NewFixtureStore(root),
	})
	if res.NScored != len(set.Records) {
		t.Errorf("scored %d of %d from fixtures", res.NScored, len(set.Records))
	}
	if res.JudgeModel != "test-judge-1" {
		t.Errorf("judge_model = %q; the recorded model must survive the replay", res.JudgeModel)
	}
	if res.Verdict != judge.VerdictPass {
		t.Errorf("verdict = %s over a perfectly agreeing replay", res.Verdict)
	}
}

// TestChangingAnyByteOfAPromptChangesTheSHA is what makes prompt-change
// detection work by hash rather than by path.
func TestChangingAnyByteOfAPromptChangesTheSHA(t *testing.T) {
	t.Parallel()

	a := judge.PromptSHA(&promptedGoal{prompt: "is this answer correct?"})
	b := judge.PromptSHA(&promptedGoal{prompt: "is this answer correct? "})
	if a == b {
		t.Fatal("a whitespace edit did not change the prompt sha")
	}
	if a == judge.NoPromptSHA || b == judge.NoPromptSHA {
		t.Error("a goal with prompts reported the no-prompt sentinel")
	}
	if got := judge.PromptSHA(constantGoal{}); got != judge.NoPromptSHA {
		t.Errorf("a goal with no prompts reported %q, want %q", got, judge.NoPromptSHA)
	}
}

// TestAPromptEditWithoutRecordingFails is the gate: the fixture directory for
// the new sha does not exist, and the error names the sha and the remedy.
func TestAPromptEditWithoutRecordingFails(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	recorded := &promptedGoal{prompt: "is this answer correct?"}
	root := t.TempDir()
	writeFixtures(t, root, "prompted", judge.PromptSHA(recorded), set, truthOf(set))

	// The prompt is edited. Nothing else moves.
	edited := &promptedGoal{t: t, prompt: "is this answer correct? Be strict."}
	_, err := judge.Calibrate(t.Context(), judge.Options{
		Goal: edited, GoalName: "prompted", Set: set,
		Fixtures: judge.NewFixtureStore(root),
	})
	if err == nil {
		t.Fatal("an unrecorded prompt replayed successfully")
	}
	if !strings.Contains(err.Error(), "no recorded judge responses for prompt") {
		t.Errorf("the error does not name the missing recording:\n%v", err)
	}
	if !strings.Contains(err.Error(), "make record-calibration") {
		t.Errorf("the error does not name the remedy:\n%v", err)
	}
}

// TestAMissingFixtureForOneRecordFails. A partial replay would compute kappa
// over whichever records happened to have fixtures, and the number would look
// exactly like a real one.
func TestAMissingFixtureForOneRecordFails(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	g := &promptedGoal{t: t, prompt: "is this answer correct?"}
	sha := judge.PromptSHA(g)
	root := t.TempDir()
	writeFixtures(t, root, "prompted", sha, set, truthOf(set))

	missing := set.Records[3].ID
	if err := os.Remove(filepath.Join(root, "prompted", sha, missing+".json")); err != nil {
		t.Fatal(err)
	}

	_, err := judge.Calibrate(t.Context(), judge.Options{
		Goal: g, GoalName: "prompted", Set: set, Fixtures: judge.NewFixtureStore(root),
	})
	if err == nil {
		t.Fatal("a partial replay succeeded")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("the error does not name the missing record %s:\n%v", missing, err)
	}
}

// TestReplayingAPromptedGoalWithoutFixturesIsRefused: the alternative is
// calling the provider from the free, offline path.
func TestReplayingAPromptedGoalWithoutFixturesIsRefused(t *testing.T) {
	t.Parallel()

	set := starterSet(t)
	_, err := judge.Calibrate(t.Context(), judge.Options{
		Goal: &promptedGoal{t: t, prompt: "p"}, GoalName: "prompted", Set: set,
	})
	if err == nil {
		t.Fatal("a prompted goal replayed with no fixture store")
	}
}

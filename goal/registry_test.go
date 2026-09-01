package goal_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/goal"
	"github.com/knograph/kno/goal/exactmatch"
)

// stubProvider stands in for an LLM endpoint. Calling it fails the test, so a
// Goal that gets past the allowlist and then spends fails twice: once for
// registering, once for spending.
type stubProvider struct{ t *testing.T }

func (p *stubProvider) call() {
	p.t.Helper()
	p.t.Fatal("a provider was called from inside Goal.Score. " +
		"Score runs OUTSIDE the budget reservation: this is money the guard " +
		"never authorized and never settled.")
}

// llmBackedGoal calls a provider inside Score and declares NOTHING about
// itself. It is the Goal the containment exists for.
type llmBackedGoal struct{ provider *stubProvider }

func (g *llmBackedGoal) Score(context.Context, *core.Case, *core.Response) (*core.Score, error) {
	g.provider.call()
	return &knov1.Score{}, nil
}
func (g *llmBackedGoal) Domain() core.ScoreDomain  { return knov1.ScoreDomain_SCORE_DOMAIN_BINARY }
func (g *llmBackedGoal) Direction() core.Direction { return knov1.Direction_DIRECTION_MAXIMIZE }

// selfDeclaringGoal calls a provider inside Score AND claims it does not.
//
// It implements every marker an affirmative design might have asked for. That
// is the point: a must-affirm interface is a self-report, and a self-report
// admits this Goal. The allowlist does not, because the claim is not the
// Goal's to make.
type selfDeclaringGoal struct{ provider *stubProvider }

func (g *selfDeclaringGoal) Score(context.Context, *core.Case, *core.Response) (*core.Score, error) {
	g.provider.call()
	return &knov1.Score{}, nil
}

func (g *selfDeclaringGoal) Domain() core.ScoreDomain { return knov1.ScoreDomain_SCORE_DOMAIN_BINARY }

func (g *selfDeclaringGoal) Direction() core.Direction {
	return knov1.Direction_DIRECTION_MAXIMIZE
}
func (g *selfDeclaringGoal) SelfContained() bool { return true }
func (g *selfDeclaringGoal) LLMBacked() bool     { return false }

// TestUnmarkedLLMBackedGoalIsRefused is the acceptance criterion for the P0.
//
// The assertion is REFUSAL, not admission. A Goal that calls a provider inside
// Score and says nothing at all about itself must not register — the case an
// opt-in guard admits, and the reason this registry is default-deny.
func TestUnmarkedLLMBackedGoalIsRefused(t *testing.T) {
	t.Parallel()

	r := goal.NewRegistry()
	err := r.Register("mystery-judge", &llmBackedGoal{provider: &stubProvider{t: t}})
	if err == nil {
		t.Fatal("an unmarked provider-calling Goal registered. " +
			"Goal.Score runs outside the budget reservation, so this Goal would " +
			"spend the user's money with no guard in the path.")
	}
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("refusal is not an Actionable: %v", err)
	}
	for _, want := range []string{"allowlist", "exact-match", "docs/debt.md#150"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%s", want, err.Error())
		}
	}
	if names := r.Names(); len(names) != 0 {
		t.Errorf("the refused Goal is in the registry anyway: %v", names)
	}
	if _, err := r.Resolve("mystery-judge"); err == nil {
		t.Error("a refused Goal resolved")
	}
}

// TestSelfDeclaredGoalIsStillRefused is the finding the allowlist answers: a
// marker method is a self-report, and a self-report catches only the author who
// did not need catching.
func TestSelfDeclaredGoalIsStillRefused(t *testing.T) {
	t.Parallel()

	r := goal.NewRegistry()
	if err := r.Register("honest-judge", &selfDeclaringGoal{provider: &stubProvider{t: t}}); err == nil {
		t.Fatal("a Goal admitted itself by declaring itself self-contained. " +
			"Nothing a Goal says about itself may admit it.")
	}
}

// TestAllowlistedGoalRegisters is the other half: the guard must not refuse
// everything, or it is not a guard, it is a wall.
func TestAllowlistedGoalRegisters(t *testing.T) {
	t.Parallel()

	r := goal.NewRegistry()
	if err := r.Register("exact-match", &exactmatch.Goal{}); err != nil {
		t.Fatalf("exact-match is on the allowlist and did not register: %v", err)
	}
	g, err := r.Resolve("exact-match")
	if err != nil {
		t.Fatalf("resolving a registered Goal: %v", err)
	}
	if g.Domain() != knov1.ScoreDomain_SCORE_DOMAIN_BINARY {
		t.Errorf("resolved the wrong Goal: domain %v", g.Domain())
	}
}

// TestUnknownGoalNamesWhatIsAvailable pins resolveGoal's replacement error:
// the old hardcoded `if` could only name exact-match, whatever was registered.
func TestUnknownGoalNamesWhatIsAvailable(t *testing.T) {
	t.Parallel()

	r := goal.NewRegistry()
	if err := r.Register("exact-match", &exactmatch.Goal{}); err != nil {
		t.Fatal(err)
	}
	_, err := r.Resolve("rubric-judge")
	if err == nil {
		t.Fatal("an unregistered Goal resolved")
	}
	if !strings.Contains(err.Error(), "exact-match") {
		t.Errorf("the error does not list what is available:\n%s", err.Error())
	}
}

// TestRegisteringOneNameTwicePanics: two Goals answering to one name would
// score a run with whichever registered second, and nothing would say so.
func TestRegisteringOneNameTwicePanics(t *testing.T) {
	t.Parallel()

	r := goal.NewRegistry()
	if err := r.Register("exact-match", &exactmatch.Goal{}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Error("registering one name twice did not panic")
		}
	}()
	_ = r.Register("exact-match", &exactmatch.Goal{})
}

// TestRegisteringNilIsRefused: a nil Goal panics at the first Score, which is
// far from the wiring bug that put it there.
func TestRegisteringNilIsRefused(t *testing.T) {
	t.Parallel()

	if err := goal.NewRegistry().Register("exact-match", nil); err == nil {
		t.Error("a nil Goal registered")
	}
}

// TestSelfContainedIsNotEditableThroughItsAccessor guards the reason the
// allowlist is unexported: a caller that could append to it could admit the
// very Goal the default-deny polarity exists to refuse.
func TestSelfContainedIsNotEditableThroughItsAccessor(t *testing.T) {
	t.Parallel()

	got := goal.SelfContained()
	got = append(got, "mystery-judge")
	_ = got

	r := goal.NewRegistry()
	if err := r.Register("mystery-judge", &exactmatch.Goal{}); err == nil {
		t.Error("mutating the returned slice changed the allowlist")
	}
	if names := goal.SelfContained(); len(names) != 1 || names[0] != "exact-match" {
		t.Errorf("the allowlist drifted: %v", names)
	}
}

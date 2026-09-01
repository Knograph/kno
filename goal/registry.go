// Package goal names the Goals a build can score against.
//
// It exists because `--goal` was matched against a hardcoded `if`, and
// calibrating a Goal requires naming it. What it adds beyond a map is the
// containment described below.
//
// # The allowlist is a budget guard
//
// core.Goal.Score runs OUTSIDE the budget reservation. core/invoke.go brackets
// the agent call — Authorize, Invoke, Settle — and Score runs after the
// reservation has already settled, in both callers. That is harmless today
// only because the one Goal in the tree is arithmetic over two strings. The
// moment a Goal calls a provider inside Score, it spends money the guard never
// authorized and never settles, which is prime directive 4 verbatim.
//
// So registration is DEFAULT-DENY: a Goal registers only if its name appears
// in this package's compile-time allowlist of Goals that provably make no
// provider call. Absence is unsafe.
//
// The alternative — an affirmative marker method every Goal must implement —
// has the better ergonomics and is the weaker guard, because it is still a
// self-report. It catches the author who FORGOT and does nothing about the one
// who writes a marker returning true on a Goal that calls a provider: it
// compiles, and the tests go green. The allowlist takes the decision away from
// the Goal entirely and puts it in a one-line diff to this file, where a
// reviewer already knows what the line means. It is also mechanically coupled
// to the debt entry: adding a judged Goal REQUIRES editing the allowlist, and
// the trigger is written on the line being edited, so it cannot be missed by
// failing to read docs/debt.md.
//
// The cost, stated: an out-of-tree Goal cannot register at all. Accepted for
// v0.2 — Goals are in-tree today, the plugin boundary is Ring-2 adapters, and
// the plan that lands an out-of-tree Goal is the plan that fixes the budget
// seam, which is what removes this allowlist's reason to exist.
package goal

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
)

// selfContained is the allowlist: the names of Goals that provably make no
// provider call inside Score.
//
// UNEXPORTED, and deliberately not the `map[string]struct{}` variable the plan
// sketched. An exported map is an allowlist any caller can append to at run
// time — including the out-of-tree judged Goal this guard exists to refuse —
// which reintroduces the self-report the default-deny polarity was chosen to
// eliminate. Callers that need to read it get SelfContained() below, which
// returns a copy.
//
// DEBT(docs/debt.md#150): adding a name here asserts that the Goal makes NO
// provider call inside Score. Before adding a judged Goal, fix the seam:
// core.Goal.Score runs outside the budget reservation.
var selfContained = map[string]struct{}{
	"exact-match": {},
}

// SelfContained returns the allowlisted Goal names, sorted.
//
// A copy, so that reading the allowlist cannot become a way to edit it.
func SelfContained() []string {
	out := make([]string, 0, len(selfContained))
	for name := range selfContained {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Registry maps a name to the Goal it resolves to.
//
// Populated by explicit registration from the shell that owns the build — not
// by init() side effects. A Goal registered by init() is a Goal whose presence
// depends on an import nobody reads, and whose absence is therefore a silent
// behavior change rather than a compile error.
type Registry struct {
	mu     sync.RWMutex
	byName map[string]core.Goal
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]core.Goal{}}
}

// Register adds a Goal under name.
//
// It REFUSES any name absent from the allowlist, whatever the Goal does or
// does not declare about itself. There is no marker method, no declaration,
// and nothing a Goal's author can say to admit their own Goal — see the
// package comment for why that polarity is the point.
//
// A duplicate name PANICS rather than returning an error. Two Goals answering
// to one name is programmer error in the wiring, per CLAUDE.md's panic rule,
// and the failure it would otherwise produce is a run scored by whichever Goal
// happened to register second.
func (r *Registry) Register(name string, g core.Goal) error {
	if g == nil {
		return errs.ErrInvalidInput.
			WithFix("pass a constructed Goal; a nil Goal would panic at the first Score").
			Wrap(fmt.Errorf("registering %q: nil goal", name))
	}
	if _, ok := selfContained[name]; !ok {
		return errs.ErrInvalidInput.
			WithFix("a Goal registers only if it is on goal/registry.go's self-contained " +
				"allowlist (" + strings.Join(SelfContained(), ", ") + "). " +
				"core.Goal.Score runs OUTSIDE the budget reservation, so a Goal that " +
				"calls a provider inside Score spends money the guard never authorized. " +
				"Fix that seam first: docs/debt.md#150").
			Wrap(fmt.Errorf("goal %q is not on the self-contained allowlist", name))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[name]; dup {
		panic(fmt.Sprintf("goal: %q is registered twice; one name must mean one Goal", name))
	}
	r.byName[name] = g
	return nil
}

// Resolve returns the Goal registered under name.
//
// The error names what IS available rather than naming only the one Goal the
// hardcoded `if` used to know about, so an unknown --goal is a readable
// failure rather than a guess.
func (r *Registry) Resolve(name string) (core.Goal, error) {
	r.mu.RLock()
	g, ok := r.byName[name]
	r.mu.RUnlock()
	if ok {
		return g, nil
	}
	return nil, errs.ErrInvalidInput.
		WithFix("available goals: " + strings.Join(r.Names(), ", ")).
		Wrap(fmt.Errorf("no goal named %q", name))
}

// Names returns the registered names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byName))
	for name := range r.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

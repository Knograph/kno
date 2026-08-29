package cli

import (
	"testing"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/adapters/agent/exec"
)

// TestExecSchemeResolvesToTheAdapter pins the wiring branch: an exec: ref
// resolves to the exec adapter, and the cost override becomes a declared
// cost — the Spends() flip that makes the consent path engage.
func TestExecSchemeResolvesToTheAdapter(t *testing.T) {
	t.Parallel()
	const cmd = "sh ../adapters/agent/exec/testdata/good.sh"

	f := baselineFlags{agentRef: "exec:" + cmd}
	agent, ref, err := resolveAgent(f)
	if err != nil {
		t.Fatalf("resolveAgent: %v", err)
	}
	if _, ok := agent.(*exec.Agent); !ok {
		t.Errorf("agent is %T, want *exec.Agent", agent)
	}
	if ref.GetScheme() != agentref.SchemeExec {
		t.Errorf("scheme = %q, want %q", ref.GetScheme(), agentref.SchemeExec)
	}
	if ref.GetTarget() != cmd {
		t.Errorf("target = %q, want %q", ref.GetTarget(), cmd)
	}

	// A zero override leaves the command free: Spends() stays false, which is
	// what keeps the consent path quiet for an exec run.
	if spends := agent.(interface{ Spends() bool }).Spends(); spends {
		t.Error("Spends() = true without a cost override; exec must default to free")
	}

	// An explicit override flips it.
	costed := f
	costed.costPerCall = 0.01
	costed.costPerCallSet = true
	agent2, _, err := resolveAgent(costed)
	if err != nil {
		t.Fatalf("resolveAgent with a cost override: %v", err)
	}
	if !agent2.(interface{ Spends() bool }).Spends() {
		t.Error("Spends() = false with a cost override; the consent path must engage")
	}
}

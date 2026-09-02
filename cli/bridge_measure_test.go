package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/knograph/kno/bridge"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestBridgeHostOf covers the trivial URL-to-host extraction
// bridgeAgentFactory uses to bind Together's default credential.
func TestBridgeHostOf(t *testing.T) {
	if got := bridgeHostOf("https://api.together.xyz/v1"); got != "api.together.xyz" {
		t.Errorf("bridgeHostOf = %q, want api.together.xyz", got)
	}
	if got := bridgeHostOf("not a url with spaces and \x00 control chars"); got != "" {
		t.Errorf("bridgeHostOf(garbage) = %q, want empty", got)
	}
}

// TestNewBridgeTunerRefusesAnUnsupportedScheme and
// TestNewBridgeTunerConstructsATogetherTuner cover newBridgeTuner's
// dispatch: only "together" ships in this build, and construction itself
// makes no network call, so it is exercisable without a live server.
func TestNewBridgeTunerRefusesAnUnsupportedScheme(t *testing.T) {
	_, err := newBridgeTuner(bridgeFlags{}, "fireworks")
	if err == nil {
		t.Fatal("want a refusal for a scheme with no shipped Tuner adapter")
	}
	if !strings.Contains(err.Error(), "together") {
		t.Errorf("error does not name the supported scheme: %q", err.Error())
	}
}

func TestNewBridgeTunerConstructsATogetherTuner(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-test")
	tuner, err := newBridgeTuner(bridgeFlags{}, "together")
	if err != nil {
		t.Fatalf("newBridgeTuner: %v", err)
	}
	if tuner == nil {
		t.Fatal("want a non-nil Tuner")
	}
}

// TestBridgeAgentFactoryRefusesAnUnsupportedScheme and
// TestBridgeAgentFactoryBuildsAnAgentForTogether cover
// bridgeAgentFactory's construction path the same way — the factory
// itself and the Agent it builds are both pure construction, no network.
func TestBridgeAgentFactoryRefusesAnUnsupportedScheme(t *testing.T) {
	_, err := bridgeAgentFactory(bridgeFlags{}, "fireworks", nil)
	if err == nil {
		t.Fatal("want a refusal for a scheme with no shipped inference wiring")
	}
}

func TestBridgeAgentFactoryBuildsAnAgentForTogether(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-test")
	factory, err := bridgeAgentFactory(bridgeFlags{}, "together", nil)
	if err != nil {
		t.Fatalf("bridgeAgentFactory: %v", err)
	}
	agent, err := factory(context.Background(), &knov1.AgentRef{Target: "meta-llama/Llama-3-8b-kno-run-1-all-in"})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if agent == nil {
		t.Fatal("want a non-nil Agent")
	}
}

// TestRenderBridgeResultHumanAndJSON covers both renderings of an armed
// run's result — the piece confirmAndRun's own test coverage cannot reach
// without a live Tuner and Agent (see this PR's report).
func TestRenderBridgeResultHumanAndJSON(t *testing.T) {
	result := &bridge.RunResult{
		Measured: []*knov1.BridgeGroupMeasured{
			{AblationGroup: "refunds", Verdict: knov1.BridgeGroupVerdict_BRIDGE_GROUP_VERDICT_CONFIRMED, DeltaGroup: 0.1234},
		},
		Skipped: []string{"billing"},
	}

	var human bytes.Buffer
	if err := renderBridgeResult(&human, bridgeFlags{}, result); err != nil {
		t.Fatalf("renderBridgeResult (human): %v", err)
	}
	if !strings.Contains(human.String(), "refunds") || !strings.Contains(human.String(), "0.1234") {
		t.Errorf("human output missing group or delta: %q", human.String())
	}
	if !strings.Contains(human.String(), "billing") {
		t.Errorf("human output missing the skipped group: %q", human.String())
	}

	var jsonOut bytes.Buffer
	if err := renderBridgeResult(&jsonOut, bridgeFlags{jsonOut: true}, result); err != nil {
		t.Fatalf("renderBridgeResult (json): %v", err)
	}
	doc := bridgeResultJSONOf(result)
	if len(doc.Measured) != 1 || doc.Measured[0].Group != "refunds" {
		t.Errorf("bridgeResultJSONOf = %+v", doc)
	}
	if !strings.Contains(jsonOut.String(), `"refunds"`) {
		t.Errorf("json output does not name the group: %q", jsonOut.String())
	}
}

package core

import (
	"context"
	"errors"
	"testing"

	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// contentDemandingAgent behaves the way every real ContextInjector does: it
// refuses an Asset with no content.
//
// The existing stubAgent accepts anything (`WithContext(*Asset)` ignores its
// argument), which is exactly why nothing caught the defect this file exists
// for. A test double more permissive than every adapter it stands in for
// cannot fail where they would.
// got holds the Asset by POINTER rather than by value: an Asset is a protobuf
// message carrying a sync.Mutex in its MessageState, so copying one trips
// govet's copylocks. The test only needs to observe what was handed over.
type contentDemandingAgent struct{ got **Asset }

func (contentDemandingAgent) Invoke(context.Context, *Case) (*Response, error) {
	return &Response{}, nil
}

func (a contentDemandingAgent) WithContext(asset *Asset) (Agent, error) {
	if len(asset.GetContent()) == 0 {
		return nil, errors.New("asset has no content to measure")
	}
	if a.got != nil {
		*a.got = asset
	}
	return contentDemandingAgent{}, nil
}

// TestTheTreatmentArmCarriesTheAssetsContent is the regression test for a
// defect that made `kno value` unusable against every real provider.
//
// measureAsset built its treatment arm from a freshly constructed
// `&Asset{Id: routing.AssetID}` — the ID and no Content — while the Pool's
// real Asset was already in scope as its own `asset` parameter.
//
// Two failure modes, depending on the adapter. One that validates what it is
// handed (openaicompat's assetContent, which refuses an empty Asset precisely
// to prevent the other mode) refused every measurement, so the stage could not
// run at all. One that does not validate produced a treatment request
// byte-identical to the control's: every paired difference exactly zero, with
// a tight interval around it, which reads in the report as "measured, and
// inert" — the one conclusion this stage exists to reach honestly.
//
// It survived because the pipeline had only ever been driven end to end
// against `fake:`, whose WithContext ignores its argument.
func TestTheTreatmentArmCarriesTheAssetsContent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := openTestStore(t)
	createBaselineRun(t, st, "base-1", []string{"m"})
	writeBaselineOutcome(t, st, "base-1", "c1", 1)
	writeBaselineOutcome(t, st, "base-1", "c2", 1)

	var seen *Asset
	cases := &caseSource{list: []*Case{
		{Id: "c1", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
		{Id: "c2", Input: "q", Expected: "a", Split: knov1.Split_SPLIT_DEV},
	}}
	opts := ValueOptions{
		RunID: "run-1", BaselineRunID: "base-1",
		Agent:    contentDemandingAgent{got: &seen},
		AgentRef: &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal:     fixedDirectionGoal{dir: knov1.Direction_DIRECTION_MAXIMIZE, domain: knov1.ScoreDomain_SCORE_DOMAIN_BINARY},
		GoalName: "g",
		Guard:    budget.New(budget.Limits{}, nil, 0),
		Store:    st, Evals: Seal(cases),
		Concurrency: 1,
		Routing:     value.Options{Seed: 1},
	}

	want := []byte("the Asset's actual content")
	if _, err := opts.Value(ctx, stubPool{assets: []*Asset{{Id: "a1", Content: want}}}); err != nil {
		t.Fatalf("Value: %v", err)
	}

	if got := seen.GetContent(); string(got) != string(want) {
		t.Errorf("the treatment arm was built with content %q, want %q. "+
			"An Asset injected without its content makes the treatment request "+
			"identical to the control's, so every delta is exactly zero and the "+
			"stage reports 'measured, and inert' for an Asset it never measured",
			got, want)
	}
	if seen.GetId() != "a1" {
		t.Errorf("the treatment arm carried Asset ID %q, want a1", seen.GetId())
	}
}

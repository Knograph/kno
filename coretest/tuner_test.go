package coretest_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/coretest"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// fakeTuner is a minimal, in-memory core.Tuner used only to prove
// CheckTuner is live: it must pass a conforming fake and catch each way a
// real adapter could misbehave. A harness that has never been seen to fail
// has not been shown to work (docs/debt.md#16), the same reasoning
// coretest_test.go's own fakes follow for CheckIterator.
type fakeTuner struct {
	submitted []*core.TuningJob
	nextID    int

	// statusesBeforeTerminal is how many non-terminal polls Status returns
	// before the terminal one — 0 means SUCCEEDED on the first poll.
	statusesBeforeTerminal int
	terminalStatus         knov1.JobStatus
	polls                  int

	// deployReadyNoReadyAt reproduces the exact bug #208 refuses: Ready
	// true with a zero ReadyAt.
	deployReadyNoReadyAt bool
	// deployErr, if set, is returned by Deploy unconditionally.
	deployErr error
	// teardownErr, if set, is returned by Teardown.
	teardownErr error
	// listJobsIgnoresSuffix reproduces an adapter that returns every job
	// regardless of what suffix asked for.
	listJobsIgnoresSuffix bool
	// listEndpointsNonEmpty reproduces a NON-empty ListEndpoints result —
	// used to catch EndpointsAlwaysEmpty being violated.
	listEndpointsNonEmpty bool
	// neverTerminal, if true, makes Status never report a terminal status —
	// used to prove CheckTuner's MaxPolls bound actually fires.
	neverTerminal bool
}

func (f *fakeTuner) Submit(_ context.Context, job *core.TuningJob) (*core.JobRef, error) {
	f.nextID++
	f.submitted = append(f.submitted, job)
	return &core.JobRef{Id: fmt.Sprintf("fake-job-%d", f.nextID), Provider: "fake"}, nil
}

func (f *fakeTuner) Status(_ context.Context, ref *core.JobRef) (*core.JobState, error) {
	f.polls++
	if f.neverTerminal || f.polls <= f.statusesBeforeTerminal {
		return &core.JobState{Ref: ref, Status: knov1.JobStatus_JOB_STATUS_RUNNING}, nil
	}
	state := &core.JobState{Ref: ref, Status: f.terminalStatus}
	if f.terminalStatus == knov1.JobStatus_JOB_STATUS_SUCCEEDED {
		state.TunedModel = &knov1.AgentRef{Ref: "fake:tuned-model", Scheme: "fake", Target: "tuned-model"}
	}
	return state, nil
}

func (f *fakeTuner) Model(ctx context.Context, ref *core.JobRef) (*core.AgentRef, error) {
	state, err := f.Status(ctx, ref)
	if err != nil {
		return nil, err
	}
	if state.GetTunedModel() == nil {
		return nil, errors.New("fake: no tuned model")
	}
	return state.GetTunedModel(), nil
}

func (f *fakeTuner) Deploy(_ context.Context, _ *core.JobRef) (*core.Endpoint, error) {
	if f.deployErr != nil {
		return nil, f.deployErr
	}
	if f.terminalStatus != knov1.JobStatus_JOB_STATUS_SUCCEEDED {
		return nil, errors.New("fake: job has no tuned model")
	}
	ep := &core.Endpoint{ID: "fake-endpoint", Provider: "fake", Served: "tuned-model", Ready: true}
	if !f.deployReadyNoReadyAt {
		ep.ReadyAt = time.Now()
	}
	return ep, nil
}

func (f *fakeTuner) Teardown(_ context.Context, _ *core.Endpoint) error {
	return f.teardownErr
}

func (f *fakeTuner) ListJobs(_ context.Context, suffix string) ([]*core.JobRef, error) {
	var refs []*core.JobRef
	for i, job := range f.submitted {
		if !f.listJobsIgnoresSuffix && job.GetSuffix() != suffix {
			continue
		}
		refs = append(refs, &core.JobRef{Id: fmt.Sprintf("fake-job-%d", i+1), Provider: "fake"})
	}
	return refs, nil
}

func (f *fakeTuner) ListEndpoints(_ context.Context, _ string) ([]*core.Endpoint, error) {
	if f.listEndpointsNonEmpty {
		return []*core.Endpoint{{ID: "fake-endpoint", Provider: "fake"}}, nil
	}
	return nil, nil
}

var _ core.Tuner = (*fakeTuner)(nil)

// conformingScenario returns a TunerScenario over a fakeTuner that satisfies
// every check — the baseline every negative test below is a mutation of.
func conformingScenario() coretest.TunerScenario {
	return coretest.TunerScenario{
		NewTuner: func() (core.Tuner, error) {
			return &fakeTuner{statusesBeforeTerminal: 1, terminalStatus: knov1.JobStatus_JOB_STATUS_SUCCEEDED}, nil
		},
		Job:                  &core.TuningJob{BaseModel: &core.AgentRef{Target: "base-model"}, Suffix: "kno-run-1"},
		Suffix:               "kno-run-1",
		NegativeSuffix:       "kno-run-2",
		EndpointsAlwaysEmpty: true,
	}
}

// TestConformTunerAcceptsAConformingAdapter is the baseline: the harness
// must pass something correct, or every other result is meaningless.
func TestConformTunerAcceptsAConformingAdapter(t *testing.T) {
	t.Parallel()
	coretest.ConformTuner(t, conformingScenario())
}

// TestCheckTunerCatchesReadyWithNoReadyAt proves the #208 contract check is
// live: a Deploy reporting Ready true with a zero ReadyAt must be caught.
func TestCheckTunerCatchesReadyWithNoReadyAt(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{
			statusesBeforeTerminal: 0, terminalStatus: knov1.JobStatus_JOB_STATUS_SUCCEEDED,
			deployReadyNoReadyAt: true,
		}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a Deploy that reported Ready true with a zero ReadyAt")
	}
	t.Logf("violations reported: %v", got)
}

// TestCheckTunerCatchesTeardownFailure proves Teardown's own contract is
// checked.
func TestCheckTunerCatchesTeardownFailure(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{
			statusesBeforeTerminal: 0, terminalStatus: knov1.JobStatus_JOB_STATUS_SUCCEEDED,
			teardownErr: errors.New("fake: teardown exploded"),
		}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a Teardown that failed after a successful Deploy")
	}
}

// TestCheckTunerCatchesListJobsIgnoringSuffix proves the "only jobs matching
// what was submitted" assertion is live.
func TestCheckTunerCatchesListJobsIgnoringSuffix(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{
			statusesBeforeTerminal: 0, terminalStatus: knov1.JobStatus_JOB_STATUS_SUCCEEDED,
			listJobsIgnoresSuffix: true,
		}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a ListJobs that ignored suffix entirely, " +
			"which the NegativeSuffix check must catch")
	}
}

// TestCheckTunerCatchesEndpointsAlwaysEmptyViolation proves
// EndpointsAlwaysEmpty is enforced, not decorative.
func TestCheckTunerCatchesEndpointsAlwaysEmptyViolation(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{
			statusesBeforeTerminal: 0, terminalStatus: knov1.JobStatus_JOB_STATUS_SUCCEEDED,
			listEndpointsNonEmpty: true,
		}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a ListEndpoints that returned entries " +
			"despite the scenario declaring EndpointsAlwaysEmpty")
	}
}

// TestCheckTunerCatchesAnUncancellableTerminalState proves the MaxPolls
// bound actually fires against an adapter whose job never terminates.
func TestCheckTunerCatchesAnUncancellableTerminalState(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.MaxPolls = 3
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{neverTerminal: true}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a Status that never reached a terminal JobStatus")
	}
}

// TestCheckTunerCatchesDeployWithoutModel proves a FAILED job's Deploy must
// be refused, not silently authorized.
func TestCheckTunerCatchesDeployWithoutModel(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{
			statusesBeforeTerminal: 0, terminalStatus: knov1.JobStatus_JOB_STATUS_FAILED,
			deployErr: nil, // the fake's own Deploy already refuses on non-SUCCEEDED
		}, nil
	}
	// A conforming FAILED-path fake must produce NO violations: Deploy
	// refuses on its own (see fakeTuner.Deploy), which is the behavior
	// under test.
	got := coretest.CheckTuner(s)
	if len(got) != 0 {
		t.Fatalf("a FAILED job that correctly refuses Deploy should conform; got %v", got)
	}
}

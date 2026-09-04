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
	// used to catch EndpointsAlwaysEmpty being violated, AND (when the
	// scenario declares EndpointsAlwaysEmpty false) to prove a deploy-
	// required adapter's real, listable resource is let through.
	listEndpointsNonEmpty bool
	// neverTerminal, if true, makes Status never report a terminal status —
	// used to prove CheckTuner's MaxPolls bound actually fires.
	neverTerminal bool

	// submitErr, if set, is returned by Submit unconditionally.
	submitErr error
	// submitEmptyID reproduces a Submit that reports success but returns a
	// JobRef with no Id — nothing downstream could ever look this job up.
	submitEmptyID bool
	// statusErr, if set, is returned by every Status call.
	statusErr error
	// deploySucceedsAnyway reproduces the mirror bug to deployReadyNoReadyAt:
	// an adapter whose Deploy fabricates an Endpoint even for a job that
	// never reached SUCCEEDED, rather than refusing it.
	deploySucceedsAnyway bool
	// deployNilEndpoint reproduces an adapter whose Deploy reports success
	// (a nil error) but returns a nil Endpoint — nothing downstream could
	// read Ready/ReadyAt off it.
	deployNilEndpoint bool
	// listJobsErr, if set, is returned by ListJobs when its suffix argument
	// equals listJobsErrOnSuffix (or unconditionally when that field is
	// empty) — lets a test target EITHER the positive-suffix call or the
	// NegativeSuffix call specifically, since checkTunerListJobs makes both.
	listJobsErr         error
	listJobsErrOnSuffix string
	// listJobsReturnsNothing reproduces an adapter whose ListJobs never
	// finds the job it was just told to tag, for ANY suffix — the mirror
	// bug to listJobsIgnoresSuffix.
	listJobsReturnsNothing bool
	// listEndpointsErr, if set, is returned by ListEndpoints.
	listEndpointsErr error
}

func (f *fakeTuner) Submit(_ context.Context, job *core.TuningJob) (*core.JobRef, error) {
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	f.nextID++
	f.submitted = append(f.submitted, job)
	if f.submitEmptyID {
		return &core.JobRef{Provider: "fake"}, nil
	}
	return &core.JobRef{Id: fmt.Sprintf("fake-job-%d", f.nextID), Provider: "fake"}, nil
}

func (f *fakeTuner) Status(_ context.Context, ref *core.JobRef) (*core.JobState, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
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
	if f.terminalStatus != knov1.JobStatus_JOB_STATUS_SUCCEEDED && !f.deploySucceedsAnyway {
		return nil, errors.New("fake: job has no tuned model")
	}
	if f.deployNilEndpoint {
		return nil, nil
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
	if f.listJobsErr != nil && (f.listJobsErrOnSuffix == "" || f.listJobsErrOnSuffix == suffix) {
		return nil, f.listJobsErr
	}
	if f.listJobsReturnsNothing {
		return nil, nil
	}
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
	if f.listEndpointsErr != nil {
		return nil, f.listEndpointsErr
	}
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

// TestCheckTunerCatchesDeploySucceedingWithoutAModel is
// TestCheckTunerCatchesDeployWithoutModel's mirror: an adapter whose Deploy
// fabricates an Endpoint for a job that never reached SUCCEEDED must be
// caught, not silently accepted.
func TestCheckTunerCatchesDeploySucceedingWithoutAModel(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{
			statusesBeforeTerminal: 0, terminalStatus: knov1.JobStatus_JOB_STATUS_FAILED,
			deploySucceedsAnyway: true,
		}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a Deploy that fabricated an Endpoint for a job with no tuned model")
	}
}

// TestCheckTunerCatchesNilNewTuner, TestCheckTunerCatchesNewTunerError and
// TestCheckTunerCatchesNewTunerReturningNilTuner cover CheckTuner's own
// construction-time refusals — the harness must fail loudly on a scenario
// it cannot even build a Tuner from, rather than reporting a clean pass on
// nothing.
func TestCheckTunerCatchesNilNewTuner(t *testing.T) {
	t.Parallel()
	got := coretest.CheckTuner(coretest.TunerScenario{})
	if len(got) == 0 {
		t.Fatal("the harness passed a TunerScenario with a nil NewTuner")
	}
}

func TestCheckTunerCatchesNewTunerError(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) { return nil, errors.New("fake: construction failed") }
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a NewTuner that returned an error")
	}
}

func TestCheckTunerCatchesNewTunerReturningNilTuner(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) { return nil, nil }
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a NewTuner that returned a nil Tuner with no error")
	}
}

// TestCheckTunerCatchesSubmitError and TestCheckTunerCatchesEmptyJobRefID
// cover Submit's own two refusals.
func TestCheckTunerCatchesSubmitError(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{submitErr: errors.New("fake: submit failed")}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a Submit that failed")
	}
}

func TestCheckTunerCatchesEmptyJobRefID(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{
			submitEmptyID: true, statusesBeforeTerminal: 0,
			terminalStatus: knov1.JobStatus_JOB_STATUS_SUCCEEDED,
		}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a Submit that returned a JobRef with an empty Id")
	}
}

// TestCheckTunerListJobsSkipsWhenSuffixEmpty proves a scenario that
// declares no Suffix at all is not checked — ListJobs and ListEndpoints
// have nothing to assert without one, so neither call is made.
func TestCheckTunerListJobsSkipsWhenSuffixEmpty(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.Suffix = ""
	s.NegativeSuffix = ""
	got := coretest.CheckTuner(s)
	if len(got) != 0 {
		t.Fatalf("a scenario with no Suffix should skip the ListJobs/ListEndpoints checks entirely; got %v", got)
	}
}

// TestCheckTunerListJobsSkipsNegativeCheckWhenEmpty proves the exclusivity
// half of ListJobs's contract is opt-in: a scenario with a Suffix but no
// NegativeSuffix still checks the positive match, but never calls ListJobs
// a second time.
func TestCheckTunerListJobsSkipsNegativeCheckWhenEmpty(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NegativeSuffix = ""
	got := coretest.CheckTuner(s)
	if len(got) != 0 {
		t.Fatalf("a scenario with no NegativeSuffix should still conform on the positive match alone; got %v", got)
	}
}

// TestCheckTunerCatchesListJobsError proves a ListJobs failure on the
// POSITIVE suffix call is reported, not swallowed.
func TestCheckTunerCatchesListJobsError(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{
			statusesBeforeTerminal: 0, terminalStatus: knov1.JobStatus_JOB_STATUS_SUCCEEDED,
			listJobsErr: errors.New("fake: ListJobs failed"),
		}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a ListJobs that failed on the positive suffix")
	}
}

// TestCheckTunerCatchesListJobsErrorOnNegativeSuffix is
// TestCheckTunerCatchesListJobsError's mirror: a ListJobs that succeeds for
// the submitted job's own suffix but fails for the negative one must still
// be reported.
func TestCheckTunerCatchesListJobsErrorOnNegativeSuffix(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{
			statusesBeforeTerminal: 0, terminalStatus: knov1.JobStatus_JOB_STATUS_SUCCEEDED,
			listJobsErr: errors.New("fake: ListJobs failed"), listJobsErrOnSuffix: "kno-run-2",
		}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a ListJobs that failed on the negative suffix")
	}
}

// TestCheckTunerCatchesListJobsMissingTheSubmittedJob proves the positive
// half of ListJobs's contract — finding the job it was just told to tag —
// is actually checked, not just its exclusivity half.
func TestCheckTunerCatchesListJobsMissingTheSubmittedJob(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{
			statusesBeforeTerminal: 0, terminalStatus: knov1.JobStatus_JOB_STATUS_SUCCEEDED,
			listJobsReturnsNothing: true,
		}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a ListJobs that never found the job it was just told to tag")
	}
}

// TestCheckTunerCatchesListEndpointsError proves a ListEndpoints failure is
// reported.
func TestCheckTunerCatchesListEndpointsError(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{
			statusesBeforeTerminal: 0, terminalStatus: knov1.JobStatus_JOB_STATUS_SUCCEEDED,
			listEndpointsErr: errors.New("fake: ListEndpoints failed"),
		}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a ListEndpoints that failed")
	}
}

// TestCheckTunerAllowsListEndpointsResultsWhenNotAlwaysEmpty is Together's
// own shape: a deploy-required adapter's ListEndpoints finding a real,
// listable resource once Deploy has succeeded must NOT be flagged — only
// an auto-serving adapter (EndpointsAlwaysEmpty true) is held to "always
// empty". See coretest.TunerScenario.EndpointsAlwaysEmpty's doc.
func TestCheckTunerAllowsListEndpointsResultsWhenNotAlwaysEmpty(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.EndpointsAlwaysEmpty = false
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{
			statusesBeforeTerminal: 0, terminalStatus: knov1.JobStatus_JOB_STATUS_SUCCEEDED,
			listEndpointsNonEmpty: true,
		}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) != 0 {
		t.Fatalf("a deploy-required adapter's non-empty ListEndpoints should conform when "+
			"EndpointsAlwaysEmpty is false; got %v", got)
	}
}

// TestCheckTunerCatchesStatusError proves a Status failure mid-poll is
// reported rather than treated as a non-terminal state to keep polling
// past.
func TestCheckTunerCatchesStatusError(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{statusErr: errors.New("fake: Status failed")}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a Status that failed")
	}
}

// TestCheckTunerCatchesDeployErrorAfterSucceeded proves a Deploy that fails
// against a job that DID reach SUCCEEDED is reported like any other Deploy
// failure — the mirror of checkTunerDeployRefusesWithoutModel, which
// expects Deploy to fail on a job that did NOT succeed.
func TestCheckTunerCatchesDeployErrorAfterSucceeded(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{
			statusesBeforeTerminal: 0, terminalStatus: knov1.JobStatus_JOB_STATUS_SUCCEEDED,
			deployErr: errors.New("fake: deploy failed"),
		}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a Deploy that failed after a SUCCEEDED job")
	}
}

// TestCheckTunerCatchesNilEndpointFromDeploy proves a Deploy that reports
// success but returns a nil Endpoint is caught, not dereferenced or
// silently accepted.
func TestCheckTunerCatchesNilEndpointFromDeploy(t *testing.T) {
	t.Parallel()
	s := conformingScenario()
	s.NewTuner = func() (core.Tuner, error) {
		return &fakeTuner{
			statusesBeforeTerminal: 0, terminalStatus: knov1.JobStatus_JOB_STATUS_SUCCEEDED,
			deployNilEndpoint: true,
		}, nil
	}
	got := coretest.CheckTuner(s)
	if len(got) == 0 {
		t.Fatal("the harness passed a Deploy that returned a nil Endpoint with no error")
	}
}

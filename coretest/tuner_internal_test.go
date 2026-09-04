package coretest

import (
	"context"
	"testing"

	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// This file exercises pollTunerToTerminal's context-cancellation branch
// directly. CheckTuner's exported signature deliberately takes no context
// (mirroring CheckIterator's own "build one internally" convention) so
// there is no way to drive a pre-cancelled context through the public API
// — the branch is real, load-bearing code (a producer that ignores ctx.Err()
// burns budget after the user hit Ctrl-C, the same failure
// TestConformIteratorCatchesAnUncancellableProducer polices for iterators),
// just not reachable from coretest_test's black-box fakes.

// stubTuner implements core.Tuner with the minimum needed to reach
// pollTunerToTerminal — every other method is an unused stub, since this
// file's tests call pollTunerToTerminal directly rather than CheckTuner.
type stubTuner struct{ statusCalls int }

func (s *stubTuner) Submit(context.Context, *core.TuningJob) (*core.JobRef, error) {
	return &core.JobRef{Id: "stub-job"}, nil
}

func (s *stubTuner) Status(context.Context, *core.JobRef) (*core.JobState, error) {
	s.statusCalls++
	return &core.JobState{Status: knov1.JobStatus_JOB_STATUS_RUNNING}, nil
}

func (s *stubTuner) Model(context.Context, *core.JobRef) (*core.AgentRef, error) { return nil, nil }
func (s *stubTuner) Deploy(context.Context, *core.JobRef) (*core.Endpoint, error) {
	return nil, nil
}
func (s *stubTuner) Teardown(context.Context, *core.Endpoint) error { return nil }
func (s *stubTuner) ListJobs(context.Context, string) ([]*core.JobRef, error) {
	return nil, nil
}

func (s *stubTuner) ListEndpoints(context.Context, string) ([]*core.Endpoint, error) {
	return nil, nil
}

var _ core.Tuner = (*stubTuner)(nil)

// TestPollTunerToTerminalStopsOnCancelledContext proves pollTunerToTerminal
// checks ctx.Err() before ever calling Status, rather than polling a
// cancelled run to exhaustion.
func TestPollTunerToTerminalStopsOnCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tuner := &stubTuner{}
	_, terminated, violations := pollTunerToTerminal(ctx, tuner, &core.JobRef{Id: "stub-job"}, 5)
	if terminated {
		t.Fatal("a cancelled context must not report a terminal state")
	}
	if len(violations) == 0 {
		t.Fatal("pollTunerToTerminal did not report the cancellation")
	}
	if tuner.statusCalls != 0 {
		t.Errorf("Status was called %d times against a cancelled context, want 0", tuner.statusCalls)
	}
}

package coretest

import (
	"context"
	"fmt"
	"testing"

	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// This file is the Tuner conformance suite docs/plans/2026-09-02-openai-
// tuner.md's test plan calls for: "coretest exports only ConformIterator
// today — there is nothing for Tuner, so this is new work, not reuse."
//
// It is built to span two structurally different adapters on purpose —
// deploy-required versus auto-serve, per-minute versus per-token, suffix
// versus metadata — and is exercised against BOTH
// adapters/tuner/together and adapters/tuner/openai. Where the two cannot
// satisfy the same assertion, TunerScenario carries a field naming the
// difference (EndpointsAlwaysEmpty) rather than the harness silently
// weakening the check to fit — see that field's own doc and this PR's
// report for the finding it records about core.Tuner.ListEndpoints.

// defaultMaxTunerPolls bounds CheckTuner's Status polling loop so a
// misbehaving adapter or fixture cannot hang the suite.
const defaultMaxTunerPolls = 20

// TunerScenario bundles what one adapter's Tuner conformance run needs.
//
// Building a Tuner and pointing it at that adapter's own fixture or test
// server is the caller's job — CheckTuner drives the SAME lifecycle
// (Submit, poll Status to a terminal state, Deploy, Teardown, ListJobs,
// ListEndpoints) against whatever core.Tuner NewTuner returns.
type TunerScenario struct {
	// NewTuner returns a fresh core.Tuner, already pointed at whatever
	// fixture or test server this scenario's Job/Suffix were recorded
	// against. Called once per CheckTuner run.
	NewTuner func() (core.Tuner, error)

	// Job is what CheckTuner submits. Its Suffix field should equal Suffix
	// below — the adopt-by-suffix mechanism core.Tuner.ListJobs exists for
	// tags a submitted job by ITS OWN Suffix, however the adapter chooses
	// to carry that tag on the wire.
	Job *core.TuningJob

	// Suffix is what ListJobs/ListEndpoints should find the submitted job
	// under. core.Tuner.ListJobs's own doc leaves HOW an adapter matches
	// suffix to the adapter — together matches a wire "suffix" field
	// directly, openai matches a namespaced metadata tag — so CheckTuner
	// only asserts the interface-level contract: the same string in, the
	// submitted job back out. Skipped when empty.
	Suffix string

	// NegativeSuffix, if set, must NOT match the submitted job — proves
	// ListJobs filters rather than returning every job on the account.
	NegativeSuffix string

	// MaxPolls bounds how many times CheckTuner calls Status waiting for a
	// terminal JobStatus. Zero uses defaultMaxTunerPolls.
	MaxPolls int

	// EndpointsAlwaysEmpty reports whether this adapter's ListEndpoints
	// never finds a deployed endpoint — true for an auto-serving provider
	// with no dedicated-endpoint resource to list (openai), false for one
	// that creates a real, listable resource once Deploy succeeds
	// (together). This is the one assertion the two reference adapters
	// cannot share uniformly; parameterizing over it, rather than dropping
	// it, is what keeps ONE harness meaningful for both — see this file's
	// package doc.
	EndpointsAlwaysEmpty bool
}

// CheckTuner runs one adapter's Tuner through a real job lifecycle and
// returns one message per violation, empty when the adapter conforms.
//
// It returns findings rather than calling t.Error so the harness itself can
// be tested — CheckIterator's own doc gives the reasoning (docs/debt.md#16):
// a harness that has never been seen to fail has not been shown to work.
// ConformTuner is the thin reporting wrapper.
//
// What it checks, at minimum, per docs/plans/2026-09-02-openai-tuner.md's
// test plan:
//
//   - Submit succeeds and returns a JobRef with a non-empty Id.
//   - Status reaches a terminal JobStatus within MaxPolls.
//   - A Deploy returning Ready: true returns a non-zero ReadyAt (#208's
//     contract, checked at the adapter's own unit level).
//   - Teardown is safe to call after a successful Deploy.
//   - ListJobs(Suffix) returns the submitted job, and — when NegativeSuffix
//     is set — ListJobs(NegativeSuffix) does not: "returns only jobs
//     matching what was submitted."
//   - ListEndpoints(Suffix) matches the scenario's declared
//     EndpointsAlwaysEmpty contract.
func CheckTuner(s TunerScenario) []string {
	ctx := context.Background()
	var violations []string

	if s.NewTuner == nil {
		return []string{"TunerScenario.NewTuner is nil"}
	}
	tuner, err := s.NewTuner()
	if err != nil {
		return []string{fmt.Sprintf("NewTuner failed: %v", err)}
	}
	if tuner == nil {
		return []string{"NewTuner returned a nil Tuner with no error"}
	}

	ref, err := tuner.Submit(ctx, s.Job)
	if err != nil {
		return append(violations, fmt.Sprintf("Submit failed: %v", err))
	}
	if ref.GetId() == "" {
		violations = append(violations, "Submit returned a JobRef with an empty Id")
	}

	state, terminated, pollViolations := pollTunerToTerminal(ctx, tuner, ref, s.MaxPolls)
	violations = append(violations, pollViolations...)
	if !terminated {
		// Nothing downstream (Deploy, Teardown) can be checked meaningfully
		// without a terminal state to react to.
		return violations
	}

	if state.GetStatus() == knov1.JobStatus_JOB_STATUS_SUCCEEDED {
		violations = append(violations, checkTunerDeployTeardown(ctx, tuner, ref)...)
	} else {
		violations = append(violations, checkTunerDeployRefusesWithoutModel(ctx, tuner, ref)...)
	}

	violations = append(violations, checkTunerListJobs(ctx, tuner, ref, s)...)
	violations = append(violations, checkTunerListEndpoints(ctx, tuner, s)...)

	return violations
}

func pollTunerToTerminal(ctx context.Context, tuner core.Tuner, ref *core.JobRef, maxPolls int) (*core.JobState, bool, []string) {
	if maxPolls <= 0 {
		maxPolls = defaultMaxTunerPolls
	}
	var last *core.JobState
	for i := 0; i < maxPolls; i++ {
		if err := ctx.Err(); err != nil {
			return last, false, []string{fmt.Sprintf("context cancelled while polling Status: %v", err)}
		}
		state, err := tuner.Status(ctx, ref)
		if err != nil {
			return nil, false, []string{fmt.Sprintf("Status poll %d failed: %v", i+1, err)}
		}
		last = state
		if isTerminalJobStatus(state.GetStatus()) {
			return last, true, nil
		}
	}
	return last, false, []string{fmt.Sprintf(
		"Status never reached a terminal JobStatus within %d polls; last status was %v",
		maxPolls, last.GetStatus(),
	)}
}

func isTerminalJobStatus(s knov1.JobStatus) bool {
	switch s {
	case knov1.JobStatus_JOB_STATUS_SUCCEEDED,
		knov1.JobStatus_JOB_STATUS_FAILED,
		knov1.JobStatus_JOB_STATUS_CANCELLED:
		return true
	default:
		return false
	}
}

// checkTunerDeployTeardown exercises Deploy after a SUCCEEDED job and
// Teardown after a successful Deploy — the two per-lifecycle assertions
// the test plan names explicitly.
func checkTunerDeployTeardown(ctx context.Context, tuner core.Tuner, ref *core.JobRef) []string {
	ep, err := tuner.Deploy(ctx, ref)
	if err != nil {
		return []string{fmt.Sprintf("Deploy failed after a SUCCEEDED job: %v", err)}
	}
	if ep == nil {
		return []string{"Deploy returned a nil Endpoint with no error"}
	}

	var violations []string
	// #208's own contract, checked at the adapter's own unit level rather
	// than only through bridge.DeployGroup's integration path — this is
	// what documents the contract for the next adapter author before they
	// write Deploy, per docs/plans/2026-09-02-openai-tuner.md §3.
	if ep.Ready && ep.ReadyAt.IsZero() {
		violations = append(violations, "Deploy returned Ready true with a zero ReadyAt — "+
			"bridge.DeployGroup (#208) refuses this; every Tuner must satisfy it at the unit level")
	}
	if err := tuner.Teardown(ctx, ep); err != nil {
		violations = append(violations, fmt.Sprintf(
			"Teardown failed after a successful Deploy: %v — Teardown must be safe to call "+
				"on every exit path once Deploy has returned successfully", err,
		))
	}
	return violations
}

// checkTunerDeployRefusesWithoutModel asserts the mirror case: a job that
// terminated WITHOUT succeeding (FAILED or CANCELLED) has no model to
// deploy, and Deploy must say so rather than fabricate an Endpoint.
func checkTunerDeployRefusesWithoutModel(ctx context.Context, tuner core.Tuner, ref *core.JobRef) []string {
	if _, err := tuner.Deploy(ctx, ref); err == nil {
		return []string{"Deploy succeeded against a job that did not reach SUCCEEDED; " +
			"it must refuse a job with no tuned model"}
	}
	return nil
}

func checkTunerListJobs(ctx context.Context, tuner core.Tuner, ref *core.JobRef, s TunerScenario) []string {
	if s.Suffix == "" {
		return nil
	}
	var violations []string

	refs, err := tuner.ListJobs(ctx, s.Suffix)
	if err != nil {
		return []string{fmt.Sprintf("ListJobs(%q) failed: %v", s.Suffix, err)}
	}
	if !tunerRefsContain(refs, ref.GetId()) {
		violations = append(violations, fmt.Sprintf(
			"ListJobs(%q) did not return the submitted job %q — the adopt-by-suffix mechanism "+
				"(core.Tuner.ListJobs's doc) must find a job it was just told to tag", s.Suffix, ref.GetId(),
		))
	}

	if s.NegativeSuffix != "" {
		negRefs, err := tuner.ListJobs(ctx, s.NegativeSuffix)
		if err != nil {
			return append(violations, fmt.Sprintf("ListJobs(%q) failed: %v", s.NegativeSuffix, err))
		}
		if tunerRefsContain(negRefs, ref.GetId()) {
			violations = append(violations, fmt.Sprintf(
				"ListJobs(%q) returned the job tagged %q — it must return ONLY jobs matching "+
					"what was submitted", s.NegativeSuffix, s.Suffix,
			))
		}
	}
	return violations
}

func tunerRefsContain(refs []*core.JobRef, id string) bool {
	for _, r := range refs {
		if r.GetId() == id {
			return true
		}
	}
	return false
}

func checkTunerListEndpoints(ctx context.Context, tuner core.Tuner, s TunerScenario) []string {
	if s.Suffix == "" {
		return nil
	}
	eps, err := tuner.ListEndpoints(ctx, s.Suffix)
	if err != nil {
		return []string{fmt.Sprintf("ListEndpoints(%q) failed: %v", s.Suffix, err)}
	}
	if s.EndpointsAlwaysEmpty && len(eps) != 0 {
		return []string{fmt.Sprintf(
			"ListEndpoints(%q) returned %d entries, want 0 — this scenario declared "+
				"EndpointsAlwaysEmpty (an auto-serving provider with no dedicated-endpoint resource)",
			s.Suffix, len(eps),
		)}
	}
	// A deploy-required adapter (EndpointsAlwaysEmpty false) is not
	// asserted to have a NON-empty result here: whether a real endpoint is
	// listable depends on adapter-specific timing this harness does not
	// control (a resume-time sweep concern, not a fresh-Deploy one). The
	// asymmetry is the finding: see this file's package doc.
	return nil
}

// ConformTuner asserts that an adapter's core.Tuner honors the cross-
// adapter contract CheckTuner checks. Adapters call this from their own
// tests, against BOTH together and openai per
// docs/plans/2026-09-02-openai-tuner.md's plan — see each adapter's own
// conformance_test.go.
func ConformTuner(t *testing.T, s TunerScenario) {
	t.Helper()
	for _, v := range CheckTuner(s) {
		t.Error(v)
	}
}

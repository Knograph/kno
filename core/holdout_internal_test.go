package core

import (
	"context"
	"errors"
	"testing"

	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestSealedEvalsCannotBeUnsealed drives errs.ErrHoldoutSealed's first
// non-test caller.
//
// The distinction being asserted is the whole reason openHoldout type-asserts
// rather than just iterating: a *SealedEvals filters to SPLIT_DEV, so opening
// one as a holdout would yield ZERO Cases — indistinguishable in every
// downstream surface from "your eval set has no holdout". That is a silent,
// plausible, wrong answer, and the test pins that the iterator is NOT returned
// at all rather than returned empty.
func TestSealedEvalsCannotBeUnsealed(t *testing.T) {
	t.Parallel()

	src := &splitCases{cases: []*Case{
		{Id: "d1", Split: knov1.Split_SPLIT_DEV},
		{Id: "h1", Split: knov1.Split_SPLIT_HOLDOUT},
	}}

	h, err := openHoldout(Seal(src))
	if h != nil {
		t.Error("openHoldout returned a reader for a sealed source; an empty iterator here " +
			"reads downstream as an eval set with no holdout")
	}
	if !errors.Is(err, errs.ErrHoldoutSealed) {
		t.Errorf("openHoldout(Seal(...)) = %v, want ErrHoldoutSealed", err)
	}
}

// TestOpenHoldoutRefusesANilSource pins that the opener names the flag rather
// than panicking on a nil interface.
func TestOpenHoldoutRefusesANilSource(t *testing.T) {
	t.Parallel()

	h, err := openHoldout(nil)
	if h != nil {
		t.Error("openHoldout(nil) returned a reader")
	}
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("openHoldout(nil) = %v, want ErrInvalidInput", err)
	}
}

// TestHoldoutEvalsYieldsOnlyHoldoutCases is the mirror of the seal's own test.
//
// Source order is asserted as well as membership: the holdout is measured in
// two arms over a materialized list, and a reader that reordered would make
// the two arms' Case lists disagree about which draw paired with which.
func TestHoldoutEvalsYieldsOnlyHoldoutCases(t *testing.T) {
	t.Parallel()

	src := &splitCases{cases: []*Case{
		{Id: "h1", Split: knov1.Split_SPLIT_HOLDOUT},
		{Id: "d1", Split: knov1.Split_SPLIT_DEV},
		{Id: "h2", Split: knov1.Split_SPLIT_HOLDOUT},
		{Id: "u1", Split: knov1.Split_SPLIT_UNSPECIFIED},
		{Id: "h3", Split: knov1.Split_SPLIT_HOLDOUT},
	}}

	h, err := openHoldout(src)
	if err != nil {
		t.Fatalf("openHoldout: %v", err)
	}
	seq, err := h.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	var got []string
	for c, err := range seq {
		if err != nil {
			t.Fatalf("iterating: %v", err)
		}
		got = append(got, c.GetId())
	}
	want := []string{"h1", "h2", "h3"}
	if len(got) != len(want) {
		t.Fatalf("yielded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("yielded %v, want %v (source order)", got, want)
		}
	}
}

// TestUnassignedSplitIsNotYieldedAsHoldout is the mirror image of the seal's
// TestUnassignedSplitIsNotTreatedAsDev, and it fails in the direction that
// matters.
//
// A Case with no split has not been through ingestion. Admitting it here
// inflates the denominator of the only number that belongs in a slide — and it
// does so silently, one Case at a time.
func TestUnassignedSplitIsNotYieldedAsHoldout(t *testing.T) {
	t.Parallel()

	src := &splitCases{cases: []*Case{
		{Id: "u1", Split: knov1.Split_SPLIT_UNSPECIFIED},
		{Id: "d1", Split: knov1.Split_SPLIT_DEV},
	}}
	h, err := openHoldout(src)
	if err != nil {
		t.Fatalf("openHoldout: %v", err)
	}
	seq, err := h.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	for c := range seq {
		t.Errorf("the holdout reader yielded %s (split %s); only SPLIT_HOLDOUT passes",
			c.GetId(), c.GetSplit())
	}
}

// TestValidationVerdictIsKeyedOnTheInterval pins §9's table at the level the
// verdict is decided, so the CLI's exit-code table has something to be a
// faithful rendering OF.
func TestValidationVerdictIsKeyedOnTheInterval(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		iv   *knov1.Interval
		want knov1.ValidationVerdict
	}{
		{
			"low above zero is confirmed", &knov1.Interval{Low: 0.01, High: 0.20},
			knov1.ValidationVerdict_VALIDATION_VERDICT_CONFIRMED,
		},
		{
			"straddling zero is inconclusive, not a failure", &knov1.Interval{Low: -0.05, High: 0.20},
			knov1.ValidationVerdict_VALIDATION_VERDICT_INCONCLUSIVE,
		},
		{
			"high at zero is not confirmed", &knov1.Interval{Low: -0.20, High: 0},
			knov1.ValidationVerdict_VALIDATION_VERDICT_NOT_CONFIRMED,
		},
		{
			"high below zero is not confirmed", &knov1.Interval{Low: -0.20, High: -0.01},
			knov1.ValidationVerdict_VALIDATION_VERDICT_NOT_CONFIRMED,
		},
		{
			"no interval is unmeasured", nil,
			knov1.ValidationVerdict_VALIDATION_VERDICT_UNMEASURED,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := validationVerdictFor(tc.iv); got != tc.want {
				t.Errorf("validationVerdictFor(%v) = %v, want %v", tc.iv, got, tc.want)
			}
		})
	}
}

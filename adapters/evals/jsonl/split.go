package jsonl

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// splitResolution is the granularity of the hash bucket. Ten thousand buckets
// means a holdout fraction is honored to within 0.01%, which is finer than any
// eval set this decides for.
const splitResolution = 10_000

// DefaultHoldoutFrac is the share of Cases held back.
//
// 0.2 is conventional and not load-bearing; what matters is that it is
// recorded on the Run and included in the resume fingerprint, so changing it
// invalidates a resume rather than silently reclassifying Cases mid-run.
const DefaultHoldoutFrac = 0.2

// MinHoldout is the point below which a holdout stops supporting a meaningful
// confidence interval.
//
// Below this the Run still executes — refusing would make the tool unusable on
// a small eval set someone is legitimately experimenting with — but it is
// marked underpowered, and the number is reported with that attached rather
// than presented as though it meant the same thing as a real one.
const MinHoldout = 20

// assignSplit decides which half of the Evals a Case belongs to.
//
// The key is the Case ID and nothing else. An earlier design hashed the ID
// together with a per-run salt, which would have let a Case migrate between
// dev and holdout across runs — that does not prevent the leak prime directive
// 5 exists to stop, it delays it by one run: a Case scored and reported in dev
// on Monday becomes "untouched holdout" on Tuesday.
//
// Keying on the ID alone also means adding Cases to an eval set never moves
// the ones already there, so two runs over overlapping sets stay comparable.
//
// seed is normally empty. A non-empty seed is a deliberate re-split, is
// recorded on the Run, and is part of the resume fingerprint.
func assignSplit(caseID, seed string, holdoutFrac float64) knov1.Split {
	h := fnv.New64a()
	// Errors from hash writers are documented never to occur.
	_, _ = h.Write([]byte(caseID))
	if seed != "" {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(seed))
	}

	bucket := h.Sum64() % splitResolution
	if float64(bucket) < holdoutFrac*splitResolution {
		return knov1.Split_SPLIT_HOLDOUT
	}
	return knov1.Split_SPLIT_DEV
}

// SplitCounts reports how an eval set divided.
type SplitCounts struct {
	// Dev is the number of Cases available for measurement.
	Dev int

	// Holdout is the number held back for Validate.
	Holdout int

	// HoldoutFrac is the fraction that produced this division.
	//
	// Carried so that guidance can be computed against the fraction the user
	// actually configured. An earlier version derived it from the package
	// default, which meant a user running at 0.05 was told they needed 6 Cases
	// when the true answer was 21 — a fix that does not fix, which CLAUDE.md's
	// error grammar exists to prevent.
	HoldoutFrac float64
}

// Total returns the number of Cases seen.
func (c SplitCounts) Total() int { return c.Dev + c.Holdout }

// Underpowered reports whether the holdout is too small to support a
// meaningful interval.
func (c SplitCounts) Underpowered() bool { return c.Holdout > 0 && c.Holdout < MinHoldout }

// Validate reports whether the split can support a run at all.
//
// A zero-Case holdout is refused rather than warned about. A run that can never
// produce a holdout number is not a run: every downstream stage would compute
// against a reference that has no honest confirmation, and the one number
// DESIGN.md says belongs in a slide would be permanently absent with no
// explanation attached to it.
func (c SplitCounts) Validate() error {
	if c.Total() == 0 {
		return fmt.Errorf("the eval set is empty")
	}
	if c.Holdout == 0 {
		frac := c.HoldoutFrac
		if frac <= 0 {
			frac = DefaultHoldoutFrac
		}
		return fmt.Errorf(
			"all %d Cases landed in dev, leaving no holdout; at a holdout fraction of %.2f, "+
				"roughly %d or more Cases are needed", c.Total(), frac, minCasesFor(frac))
	}
	if c.Dev == 0 {
		return fmt.Errorf("all %d Cases landed in the holdout, leaving nothing to measure against", c.Total())
	}
	return nil
}

// minCasesFor is the smallest eval set that can be expected to yield a
// non-empty holdout at a given fraction.
func minCasesFor(frac float64) int {
	if frac <= 0 {
		return 0
	}
	return int(1/frac) + 1
}

// fingerprintSplit contributes the split configuration to a Run's resume
// fingerprint.
//
// Both inputs are included because either one changing reclassifies Cases. A
// resume that recomputed a different split would mix results measured against
// different populations into one run, which looks like one measurement and is
// not.
func fingerprintSplit(seed string, holdoutFrac float64) []byte {
	buf := make([]byte, 0, len(seed)+8)
	buf = append(buf, seed...)
	buf = binary.LittleEndian.AppendUint64(buf, uint64(holdoutFrac*splitResolution))
	return buf
}

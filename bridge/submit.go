package bridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
)

// SubmitOutcome reports what SubmitGroup actually did.
type SubmitOutcome int32

const (
	// SubmitOutcomeUnspecified is the zero value and never returned.
	SubmitOutcomeUnspecified SubmitOutcome = iota

	// SubmitOutcomeSubmitted means Tuner.Submit was called and returned a
	// JobRef in this call.
	SubmitOutcomeSubmitted

	// SubmitOutcomeAlreadySubmitted means a durable row already existed at
	// or past TuningJobStateSubmitted; Submit was NOT called.
	SubmitOutcomeAlreadySubmitted

	// SubmitOutcomeAbandoned means the row was found in
	// TuningJobStateSubmitting on entry — a prior process crashed inside the
	// request window — Tuner.ListJobs found no job matching this group's
	// suffix, and the row was closed TuningJobStateAbandoned by this call.
	// Submit was NOT called. The estimate stays settled.
	SubmitOutcomeAbandoned

	// SubmitOutcomeAdopted means the row was found in
	// TuningJobStateSubmitting on entry, Tuner.ListJobs found a job matching
	// this group's suffix, and the row was updated TuningJobStateSubmitted
	// with the discovered JobRef. Submit was NOT called. The estimate stays
	// settled — it already counts once the row leaves "submitting", see
	// store.SettledSpend — and no additional spend is recorded for the
	// adoption itself.
	SubmitOutcomeAdopted
)

// SubmitGroupParams bundles one ablation group's submission.
type SubmitGroupParams struct {
	// RunID and AblationGroup identify the durable row.
	RunID         string
	AblationGroup string

	Store store.Store
	Guard *budget.Guard
	Tuner core.Tuner

	// Job is Submit-ready: BaseModel, AssetIds, TrainingData, Epochs,
	// LoraRank, Suffix, AblationGroup, and EstimatedCostUsdMicros already
	// set by the caller's planning step. SubmitGroup does not compute an
	// estimate — Step 2(a)'s pricing lookup happens before this is called,
	// the same separation core.Estimator draws between estimating and
	// spending.
	Job *core.TuningJob

	// TrainTokens is the token count Job's estimate was computed from.
	// core.TuningJob carries no tokens field (it is a wire message shared
	// with the provider request shape), so it travels alongside Job here
	// for the Guard reservation and the durable row — the same Tokens
	// dimension #170/#172 fixed for the Value and Validate sinks. Dropping
	// it here would reintroduce that defect in a third stage, which is
	// exactly what this field exists to prevent.
	TrainTokens int64

	// TrainingFileSHA256 is recorded on the durable row. The training file
	// itself is never persisted — see store.TuningJobRecord.
	TrainingFileSHA256 string

	// Provider names which Tuner this is, e.g. "together".
	Provider string
}

// SubmitGroupResult is what SubmitGroup produced.
type SubmitGroupResult struct {
	// Outcome reports what happened. See SubmitOutcome.
	Outcome SubmitOutcome

	// Ref is the job reference, set for Submitted (fresh from Tuner.Submit)
	// and AlreadySubmitted (reconstructed from the durable row). Nil for
	// Abandoned.
	Ref *core.JobRef

	// Record is the durable row as it stands after this call.
	Record *store.TuningJobRecord
}

// ErrAlreadyAbandoned is returned when SubmitGroup is called for a group
// whose row is already TuningJobStateAbandoned — a caller bug: an abandoned
// group must never be retried.
var ErrAlreadyAbandoned = errors.New("bridge: this ablation group's job was already abandoned and must not be retried")

// SubmitGroup runs the bridge plan's Step 2(b)-(e) money-safety sequence for
// ONE ablation group's fine-tuning job.
//
// The sequence, in the order Step 2(b) pins as load-bearing:
//
//  1. Resume check. A durable row already at TuningJobStateSubmitted means
//     Submit must NEVER be called again for this group (acceptance
//     criterion 8) — the caller polls the recorded JobRef instead. A row
//     still at TuningJobStateSubmitting means a PRIOR call to this function
//     (in an earlier process) wrote the row and then the process ended
//     before learning whether Submit's request reached the provider.
//
//     The plan's Step 2(d) describes recovering that case by having the
//     adapter "list the provider's jobs and adopt one whose model-name
//     suffix matches". core.Tuner now carries ListJobs for exactly this —
//     an additive method beyond the plan's Step 0 "exactly two" framing
//     (Deploy and Teardown); see core.Tuner.ListJobs's doc and this PR's
//     report for why that framing undercounted. A row found in
//     TuningJobStateSubmitting calls ListJobs(ctx, Job.Suffix): a match
//     ADOPTS the discovered JobRef (the row moves straight to
//     TuningJobStateSubmitted, Submit is never called, and the estimate —
//     already settled once the row leaves "submitting", see
//     store.SettledSpend's predicate — is not re-recorded); no match closes
//     TuningJobStateAbandoned, the same safe fallback this build shipped
//     before ListJobs existed: never re-submits, never under-counts, simply
//     cannot recover a job the provider will not confirm.
//
//  2. Authorize the estimate through the Guard: Calls: 1, CostUSDMicros:
//     Job.EstimatedCostUsdMicros, Tokens: TrainTokens — all three
//     dimensions, matching the fix #170/#172 shipped for Value and
//     Validate.
//
//  3. Write the durable row, state=submitting, and wait for that write to
//     complete BEFORE Submit is entered. A crash between here and Submit's
//     response leaves this row as evidence.
//
//  4. Call Tuner.Submit. NEVER retried by this function on any error: see
//     the plan's Step 2(d) no-retry rule. A transport-transient error here
//     is treated exactly like any other Submit failure — the reservation is
//     released (nothing was spent) and the row stays submitting for a LATER
//     resume to find and close abandoned.
//
//  5. On success: settle the reservation (the estimate, not an actual — see
//     Step 2(c) for how an overrun or underrun is reconciled afterward) and
//     update the row to submitted with the returned JobRef.
func SubmitGroup(ctx context.Context, p SubmitGroupParams) (*SubmitGroupResult, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}

	existing, err := findRecord(ctx, p.Store, p.RunID, p.AblationGroup)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		switch existing.State {
		case store.TuningJobStateSubmitted:
			return &SubmitGroupResult{
				Outcome: SubmitOutcomeAlreadySubmitted,
				Ref: &core.JobRef{
					Id:          existing.ProviderJobID,
					Provider:    existing.Provider,
					SubmittedAt: existing.SubmittedAt,
				},
				Record: existing,
			}, nil
		case store.TuningJobStateAbandoned:
			return nil, fmt.Errorf("%w: run %s group %s", ErrAlreadyAbandoned, p.RunID, p.AblationGroup)
		case store.TuningJobStateSubmitting:
			return adoptOrAbandon(ctx, p.Store, p.Tuner, p.RunID, existing)
		}
	}

	est := budget.Estimate{
		Calls:         1,
		CostUSDMicros: p.Job.GetEstimatedCostUsdMicros(),
		Tokens:        p.TrainTokens,
	}
	res, err := p.Guard.Authorize(ctx, est)
	if err != nil {
		return nil, err
	}
	defer res.Release()

	record := &store.TuningJobRecord{
		AblationGroup:          p.AblationGroup,
		State:                  store.TuningJobStateSubmitting,
		Provider:               p.Provider,
		BaseModel:              p.Job.GetBaseModel().GetRef(),
		Suffix:                 p.Job.GetSuffix(),
		TrainingFileSHA256:     p.TrainingFileSHA256,
		TrainTokens:            p.TrainTokens,
		Epochs:                 p.Job.GetEpochs(),
		LoRARank:               p.Job.GetLoraRank(),
		EstimatedCostUSDMicros: p.Job.GetEstimatedCostUsdMicros(),
	}
	// Write-ahead: durable BEFORE Submit is entered. This ordering is the
	// whole of the bridge's money safety for this step — see the function
	// doc's point 3.
	if err := p.Store.WriteTuningJob(ctx, p.RunID, record); err != nil {
		return nil, fmt.Errorf("writing the write-ahead tuning job row for %s: %w", p.AblationGroup, err)
	}

	ref, err := p.Tuner.Submit(ctx, p.Job)
	if err != nil {
		// The deferred Release above returns the reservation: nothing was
		// spent. The row stays "submitting" for a resume to find — this
		// function does not retry, ever, per the no-retry rule.
		return nil, fmt.Errorf("submitting the %s group's tuning job: %w", p.AblationGroup, err)
	}

	// Settled NOW, at submission — not at completion. See Step 2(b)'s
	// reasoning: the reservation is in-memory and dies with the process;
	// settled spend is durable. A job that runs for forty minutes must not
	// hold a reservation across that whole window.
	res.Settle(budget.Spend{
		Calls:         1,
		CostUSDMicros: p.Job.GetEstimatedCostUsdMicros(),
		Tokens:        p.TrainTokens,
	})

	record.State = store.TuningJobStateSubmitted
	record.ProviderJobID = ref.GetId()
	record.SubmittedAt = ref.GetSubmittedAt()
	if err := p.Store.UpdateTuningJob(ctx, p.RunID, record); err != nil {
		return nil, fmt.Errorf("recording the submitted tuning job for %s: %w", p.AblationGroup, err)
	}

	return &SubmitGroupResult{Outcome: SubmitOutcomeSubmitted, Ref: ref, Record: record}, nil
}

func (p SubmitGroupParams) validate() error {
	switch {
	case p.RunID == "":
		return errors.New("bridge: SubmitGroup requires a run ID")
	case p.AblationGroup == "":
		return errors.New("bridge: SubmitGroup requires an ablation group")
	case p.Store == nil:
		return errors.New("bridge: SubmitGroup requires a store")
	case p.Guard == nil:
		return errors.New("bridge: SubmitGroup requires a budget guard")
	case p.Tuner == nil:
		return errors.New("bridge: SubmitGroup requires a Tuner")
	case p.Job == nil:
		return errors.New("bridge: SubmitGroup requires a Job")
	case p.Job.GetEstimatedCostUsdMicros() <= 0:
		// core.TuningJob's own godoc: "A job is never submitted without
		// one." A zero or negative estimate is not a cheap job, it is a
		// missing one — see pricing.ErrUnpriced's reasoning, applied here.
		return errors.New("bridge: SubmitGroup requires Job.EstimatedCostUsdMicros to be set and positive")
	}
	return nil
}

// findRecord reads the existing row for one ablation group, or nil if none.
func findRecord(ctx context.Context, st store.Store, runID, ablationGroup string) (*store.TuningJobRecord, error) {
	jobs, err := st.TuningJobs(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("reading existing tuning jobs for %s: %w", runID, err)
	}
	for _, j := range jobs {
		if j.AblationGroup == ablationGroup {
			return j, nil
		}
	}
	return nil, nil
}

// adoptOrAbandon resolves a row found in TuningJobStateSubmitting on entry:
// the plan's Step 2(d) adopt-by-suffix recovery. See SubmitGroup's doc point
// 1.
//
// tuner.ListJobs is called with rec.Suffix, never with the caller's fresh
// Job — the row IS the record of what was submitted (or attempted), and its
// suffix is what a real provider job would carry if the prior process's
// request reached it. A match adopts; no match (or an empty suffix, which
// means this row predates the suffix column and can never be matched)
// abandons.
func adoptOrAbandon(
	ctx context.Context, st store.Store, tuner core.Tuner, runID string, rec *store.TuningJobRecord,
) (*SubmitGroupResult, error) {
	if rec.Suffix != "" {
		refs, err := tuner.ListJobs(ctx, rec.Suffix)
		if err != nil {
			return nil, fmt.Errorf("listing provider jobs to adopt %s: %w", rec.AblationGroup, err)
		}
		if len(refs) > 0 {
			// refs is most-recent-first per core.Tuner.ListJobs's contract;
			// the newest match is the one this run's most recent submission
			// attempt would have produced.
			ref := refs[0]
			rec.State = store.TuningJobStateSubmitted
			rec.ProviderJobID = ref.GetId()
			rec.SubmittedAt = ref.GetSubmittedAt()
			if err := st.UpdateTuningJob(ctx, runID, rec); err != nil {
				return nil, fmt.Errorf("adopting tuning job for %s: %w", rec.AblationGroup, err)
			}
			return &SubmitGroupResult{Outcome: SubmitOutcomeAdopted, Ref: ref, Record: rec}, nil
		}
	}

	rec.State = store.TuningJobStateAbandoned
	if err := st.UpdateTuningJob(ctx, runID, rec); err != nil {
		return nil, fmt.Errorf("abandoning tuning job for %s: %w", rec.AblationGroup, err)
	}
	return &SubmitGroupResult{Outcome: SubmitOutcomeAbandoned, Record: rec}, nil
}

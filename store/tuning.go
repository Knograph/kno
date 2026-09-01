package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TuningJobState is the bridge's write-ahead lifecycle marker for one
// ablation group's job — distinct from TuningJobRecord.Status
// (knov1.JobStatus), which is the PROVIDER's reported progress.
//
// This is the field a resume reads to decide whether Submit may run at all.
// A row in TuningJobStateSubmitting was written BEFORE the request left and
// may or may not have reached the provider; every other state means the
// request is known to have landed (or, for abandoned, known to be
// unrecoverable) and Submit must never be called again for that group.
type TuningJobState string

const (
	// TuningJobStateSubmitting is the write-ahead row: authorized and
	// written durably, Submit not yet confirmed to have returned. A crash
	// here is the window the bridge's whole design exists to make safe — see
	// TuningJobStateAbandoned.
	TuningJobStateSubmitting TuningJobState = "submitting"

	// TuningJobStateSubmitted means Submit returned a JobRef. The estimate
	// is settled and counts toward SettledSpend from here on.
	TuningJobStateSubmitted TuningJobState = "submitted"

	// TuningJobStateAbandoned means a resume found this row still
	// "submitting", listed the provider's jobs, and found no match by
	// suffix. The estimate STAYS settled — money may have been spent on a
	// job Kno cannot see — and the group is never re-submitted.
	TuningJobStateAbandoned TuningJobState = "abandoned"
)

// TuningJobRecord is one bridge ablation group's durable job record.
//
// A Go struct, not a proto blob — like Outcome and Measurement, and for the
// same reason: SettledSpend needs real columns to sum in the same statement
// as the other three spend sources, and a blob cannot be summed by SQL.
//
// Carries NO training data and no Asset content: TrainingFileSHA256 is the
// only trace of what was submitted, because the training file is Asset
// content — customer data — and is never persisted. Resume re-renders it
// from the Portfolio and the pool, which core's export-determinism goldens
// guarantee is byte-identical.
type TuningJobRecord struct {
	// AblationGroup is "all-in", or the cluster tag this job leaves out.
	// Half of the primary key.
	AblationGroup string

	// State is the write-ahead lifecycle marker. See TuningJobState.
	State TuningJobState

	// Provider names which Tuner this job was submitted to: "openai",
	// "together", "fireworks".
	Provider string

	// ProviderJobID is the provider-assigned job identifier, empty until
	// Submit returns.
	ProviderJobID string

	// BaseModel is the agent ref of the model being tuned, e.g.
	// "together:meta-llama/Llama-3-8b".
	BaseModel string

	// Suffix is the model-name suffix submitted for traceability
	// ("kno-<run_id>-<group>") and the adoption key a resume matches
	// against when this row is found in TuningJobStateSubmitting.
	Suffix string

	// TrainingFileSHA256 is the only trace of the training file's content:
	// its hash, never the bytes.
	TrainingFileSHA256 string

	// TrainTokens is the training-token count the estimate was computed
	// from, and the Tokens dimension SettledSpend restores on resume.
	TrainTokens int64

	// Epochs is the training epoch count. Zero means the provider default.
	Epochs int32

	// LoRARank is the LoRA rank requested, when the provider supports it.
	// Zero means unset.
	LoRARank int32

	// EstimatedCostUSDMicros is what was authorized and settled AT
	// SUBMISSION — never revised in place. See ActualCostUSDMicros and
	// core's reconciliation path (RecordOrphanSpend) for how an overrun or
	// underrun is recorded instead.
	EstimatedCostUSDMicros int64

	// ActualCostUSDMicros is what the provider reported at the job's
	// terminal status, when it reported one. Nil is NOT the same as zero:
	// several providers report no per-job cost at all, and the estimate
	// stands as the recorded spend in that case — every rendering must say
	// "estimated", never "billed".
	ActualCostUSDMicros *int64

	// Status is the provider's reported job status.
	Status knov1.JobStatus

	// SubmittedAt is the RFC 3339 timestamp Submit returned at. Empty until
	// then.
	SubmittedAt string

	// TerminalAt is the RFC 3339 timestamp the job reached a terminal
	// status. Empty until then.
	TerminalAt string

	// ErrorText is the provider's error text, verbatim, when Status is
	// JOB_STATUS_FAILED.
	ErrorText string

	// EndpointID is the provider-assigned hosting endpoint id, once Deploy
	// has been called for this job. Nil means no endpoint was ever
	// deployed for this job.
	EndpointID *string

	// DeployedAt is the RFC 3339 timestamp Deploy was called at. Empty
	// until then.
	DeployedAt string

	// TornDownAt is the RFC 3339 timestamp Teardown succeeded at. A non-nil
	// EndpointID with a nil TornDownAt is a LIVE OR LEAKED endpoint —
	// exactly what `kno doctor` reports and what a resume's endpoint sweep
	// looks for.
	TornDownAt *string

	// ServeMinutes is serve minutes accrued and settled so far, this job's
	// endpoint lifetime.
	ServeMinutes int32

	// ServeCostUSDMicros is what those minutes cost at the settled rate —
	// the second SettledSpend term this record contributes.
	ServeCostUSDMicros int64
}

// WriteTuningJob inserts or replaces the write-ahead row for one ablation
// group. See the Store interface godoc for the ordering contract this
// enforces: this call must complete before Tuner.Submit is entered.
func (s *SQLite) WriteTuningJob(ctx context.Context, runID string, j *TuningJobRecord) error {
	return s.upsertTuningJob(ctx, runID, j)
}

// UpdateTuningJob replaces one group's job record in place. See the Store
// interface godoc.
func (s *SQLite) UpdateTuningJob(ctx context.Context, runID string, j *TuningJobRecord) error {
	return s.upsertTuningJob(ctx, runID, j)
}

func (s *SQLite) upsertTuningJob(ctx context.Context, runID string, j *TuningJobRecord) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	if runID == "" {
		return errors.New("store: tuning job needs a run ID")
	}
	if j.AblationGroup == "" {
		return errors.New("store: tuning job needs an ablation group")
	}

	// NULL-able columns are passed as `any`, nil when unset — the same
	// convention RecordMeasurement uses for score_value/score_passed, rather
	// than relying on database/sql's pointer-dereferencing default converter
	// implicitly. Explicit at the call site is what makes NULL readable here
	// without following the driver's conversion rules.
	var actualCost, endpointID, tornDownAt any
	if j.ActualCostUSDMicros != nil {
		actualCost = *j.ActualCostUSDMicros
	}
	if j.EndpointID != nil {
		endpointID = *j.EndpointID
	}
	if j.TornDownAt != nil {
		tornDownAt = *j.TornDownAt
	}

	return retryOnBusy(ctx, func() error {
		_, err := db.ExecContext(
			ctx,
			`INSERT OR REPLACE INTO tuning_jobs (
			    run_id, ablation_group, state, provider, provider_job_id,
			    base_model, suffix, training_file_sha256, train_tokens,
			    epochs, lora_rank, estimated_cost_usd_micros, actual_cost_usd_micros,
			    status, submitted_at, terminal_at, error_text,
			    endpoint_id, deployed_at, torn_down_at, serve_minutes, serve_cost_usd_micros
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, j.AblationGroup, string(j.State), j.Provider, j.ProviderJobID,
			j.BaseModel, j.Suffix, j.TrainingFileSHA256, j.TrainTokens,
			j.Epochs, j.LoRARank, j.EstimatedCostUSDMicros, actualCost,
			int32(j.Status), j.SubmittedAt, j.TerminalAt, j.ErrorText,
			endpointID, j.DeployedAt, tornDownAt, j.ServeMinutes, j.ServeCostUSDMicros,
		)
		if err != nil {
			return fmt.Errorf("recording tuning job %s/%s: %w", runID, j.AblationGroup, err)
		}
		return nil
	})
}

// TuningJobs returns every ablation group's record for a run, ordered by
// ablation group.
func (s *SQLite) TuningJobs(ctx context.Context, runID string) ([]*TuningJobRecord, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT ablation_group, state, provider, provider_job_id, base_model, suffix,
		        training_file_sha256, train_tokens, epochs, lora_rank,
		        estimated_cost_usd_micros, actual_cost_usd_micros, status,
		        submitted_at, terminal_at, error_text,
		        endpoint_id, deployed_at, torn_down_at, serve_minutes, serve_cost_usd_micros
		 FROM tuning_jobs WHERE run_id = ? ORDER BY ablation_group`, runID)
	if err != nil {
		return nil, fmt.Errorf("listing tuning jobs for %s: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []*TuningJobRecord
	for rows.Next() {
		j := &TuningJobRecord{}
		var (
			state                  string
			status                 int32
			actualCost             sql.NullInt64
			endpointID, tornDownAt sql.NullString
		)
		if err := rows.Scan(
			&j.AblationGroup, &state, &j.Provider, &j.ProviderJobID, &j.BaseModel, &j.Suffix,
			&j.TrainingFileSHA256, &j.TrainTokens, &j.Epochs, &j.LoRARank,
			&j.EstimatedCostUSDMicros, &actualCost, &status,
			&j.SubmittedAt, &j.TerminalAt, &j.ErrorText,
			&endpointID, &j.DeployedAt, &tornDownAt, &j.ServeMinutes, &j.ServeCostUSDMicros,
		); err != nil {
			return nil, fmt.Errorf("scanning tuning job for %s: %w", runID, err)
		}
		j.State = TuningJobState(state)
		j.Status = knov1.JobStatus(status)
		if actualCost.Valid {
			v := actualCost.Int64
			j.ActualCostUSDMicros = &v
		}
		if endpointID.Valid {
			v := endpointID.String
			j.EndpointID = &v
		}
		if tornDownAt.Valid {
			v := tornDownAt.String
			j.TornDownAt = &v
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating tuning jobs for %s: %w", runID, err)
	}
	return out, nil
}

// LeakedEndpoint is one row LeakedEndpoints reports: a tuning job whose
// endpoint was deployed and never confirmed torn down.
type LeakedEndpoint struct {
	// RunID and AblationGroup identify which run and group deployed this
	// endpoint.
	RunID         string
	AblationGroup string

	// Provider and EndpointID are what a user needs to find and stop the
	// meter in the provider's own console.
	Provider   string
	EndpointID string

	// DeployedAt is when the endpoint was recorded live.
	DeployedAt string

	// ServeMinutes and ServeCostUSDMicros are what Kno last settled for it
	// — a floor, not the true total, since a leaked endpoint keeps billing
	// after the last tick this row recorded.
	ServeMinutes       int32
	ServeCostUSDMicros int64
}

// LeakedEndpoints returns every tuning-job row, across every run, carrying
// a non-null endpoint_id with a null torn_down_at. See the Store interface
// godoc.
func (s *SQLite) LeakedEndpoints(ctx context.Context) ([]LeakedEndpoint, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT run_id, ablation_group, provider, endpoint_id, deployed_at, serve_minutes, serve_cost_usd_micros
		 FROM tuning_jobs
		 WHERE endpoint_id IS NOT NULL AND torn_down_at IS NULL
		 ORDER BY run_id, ablation_group`)
	if err != nil {
		return nil, fmt.Errorf("listing leaked endpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []LeakedEndpoint
	for rows.Next() {
		var le LeakedEndpoint
		if err := rows.Scan(&le.RunID, &le.AblationGroup, &le.Provider, &le.EndpointID,
			&le.DeployedAt, &le.ServeMinutes, &le.ServeCostUSDMicros); err != nil {
			return nil, fmt.Errorf("scanning a leaked endpoint: %w", err)
		}
		out = append(out, le)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating leaked endpoints: %w", err)
	}
	return out, nil
}

// settled reports whether j's training estimate has been confirmed spent.
//
// SettledSpend embeds this SAME predicate directly in SQL
// (`state != 'submitting'`) rather than calling this method, because the
// alternative — a second round trip through TuningJobs — gives up the
// single-statement atomicity every other SettledSpend source has. Anyone
// changing what "settled" means must change both: this method (the
// definition, used by callers that already hold a []*TuningJobRecord — a
// future `kno doctor` or bridge report reader) and the SQL fragment in
// SQLite.SettledSpend.

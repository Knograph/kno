package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"google.golang.org/protobuf/proto"
)

// Arm identifies which side of a paired comparison a Measurement belongs to.
//
// Persisted as an integer and part of the measurements primary key. It has to
// be in that key: the treatment and control arms measure the SAME Case for the
// SAME Asset, so a key without the arm makes the second one written silently
// vanish — which is the failure the measurements table exists to fix, arriving
// one level down.
type Arm int32

// The two arms. Zero is deliberately not a valid arm, so a Measurement whose
// Arm was never set is refused rather than filed as a treatment.
const (
	// ArmUnspecified is the zero value and is always refused.
	ArmUnspecified Arm = 0

	// ArmTreatment is the Case measured with the Asset injected.
	ArmTreatment Arm = 1

	// ArmControl is the Case measured without it — a FRESH measurement, not the
	// recorded baseline. Kno measures a fresh control whenever routing may have
	// conditioned on the baseline's outcome, because reusing the draw that
	// SELECTED a Case as that Case's control manufactures the effect being
	// measured. See docs/adr/0005.
	ArmControl Arm = 2
)

// String renders an Arm for error messages.
func (a Arm) String() string {
	switch a {
	case ArmTreatment:
		return "treatment"
	case ArmControl:
		return "control"
	case ArmUnspecified:
		return "unspecified"
	default:
		return fmt.Sprintf("Arm(%d)", int32(a))
	}
}

// MeasurementKey identifies one measurement within a run.
//
// All four fields, for the same reason the primary key carries all four: an
// Asset is measured over many Cases, each Case in two arms, each arm possibly
// over several trials. Any field dropped from this key collapses distinct
// paid-for measurements onto one row.
type MeasurementKey struct {
	AssetID string
	CaseID  string
	Arm     Arm

	// Trial numbers repeated measurements of the same (Asset, Case, Arm) from
	// 1. Repeats exist to average out sampling noise, so collapsing them would
	// discard the variance reduction they were bought for. Zero is refused: a
	// key normalized on write is a key the writer cannot look up on resume.
	Trial int32
}

// Measurement is one Case measured once, for one Asset, in one arm.
//
// The Value-stage analogue of Outcome, and deliberately a separate type rather
// than an Outcome with extra fields: they are written to different tables with
// different keys, and a shared type would make it possible to hand a
// measurement to RecordOutcome, where the (run_id, case_id) key would discard
// every measurement after the first.
type Measurement struct {
	// Key identifies the measurement. Every field is required, Trial included:
	// it is numbered from 1 and a zero is refused rather than normalized, so
	// the key a caller writes is the key CompletedMeasurements hands back.
	Key MeasurementKey

	// Response is what the agent returned. Nil when the call failed before
	// producing one.
	Response *knov1.Response

	// Score is the Goal's judgement. Nil when the measurement errored.
	Score *knov1.Score

	// Err is the terminal failure's machine-readable code, empty when the
	// measurement scored. Exactly one of Score or Err is set, for the same
	// reason as on Outcome: a shape permitting both would let one measurement
	// land on both sides of a delta's denominator.
	Err string

	// Spend is what this measurement cost, including any failed attempts
	// preceding a successful retry.
	Spend budget.Spend
}

// Scored reports whether this measurement produced a Score.
func (m *Measurement) Scored() bool { return m.Score != nil && m.Err == "" }

// CaseScore is one Case's recorded baseline score.
//
// A struct rather than a bare float64 because "this Case has no score" and
// "this Case scored and the number is gone" are different states with different
// correct handling, and a map[string]float64 collapses them into the first.
// A pair built against a Case whose baseline number was purged is not a pair
// with a zero in it; it is a pair that cannot be formed, and the count of those
// belongs in the report rather than in the denominator.
type CaseScore struct {
	// Value is the recorded score. Meaningless when Unrecoverable is set.
	Value float64

	// Passed is the Goal's own verdict on this Case.
	//
	// Carried alongside Value rather than derived from it, because only the
	// Goal knows where its threshold is: an exact-match Goal passes at 1.0, a
	// similarity Goal somewhere else, and a latency Goal passes BELOW its
	// number. Re-deriving a verdict here would put a second copy of that
	// judgement outside the Goal, and the copy that drifted would be the one
	// deciding which Cases a run routes to.
	Passed bool

	// Unrecoverable means the Case scored but its number is no longer readable
	// — purged before the score column existed, or written by a binary that
	// predates the column. See ScoreSummary for why those two are counted
	// apart.
	Unrecoverable bool
}

// ScoreSummary is what a run's recorded scores add up to, and what could not be
// added up.
//
// Three counts rather than one because they license different statements. A
// caller may report a mean over Counted only when both other counts are zero;
// with Purged above zero the mean is over a population the user deliberately
// shrank, and with UnknownProvenance above zero it is over a population that
// shrank for a reason nobody has established.
type ScoreSummary struct {
	// Sum is the total of every score still readable.
	Sum float64

	// Counted is how many Cases contributed to Sum.
	Counted int

	// Purged is Cases that scored, whose number was removed by `kno purge`
	// before the score lived in a column of its own.
	Purged int

	// UnknownProvenance is Cases that scored with no readable number while
	// their Score blob is still present — what a binary predating the score
	// column leaves behind when it writes into an already-migrated database.
	// Nothing re-runs the backfill, so the number is never lifted out.
	//
	// Counted apart from Purged because conflating them attributes a
	// mixed-binary bug to a deletion the user performed — see docs/debt.md#31,
	// which is what this field repays. See SQLite.ScoreSum for why the blob and
	// not a writer-version marker is what separates them.
	UnknownProvenance int
}

// Unrecoverable is how many scored Cases can no longer contribute a number, by
// either route.
func (s ScoreSummary) Unrecoverable() int { return s.Purged + s.UnknownProvenance }

// RecordMeasurement durably records one measurement in a single transaction.
//
// The Value-stage counterpart of RecordOutcome, with the same contract and for
// the same reason: the recorded row IS the done-marker, so a crash cannot leave
// a measurement that cost real money looking un-run.
//
// Idempotent on the full MeasurementKey via INSERT OR IGNORE, so a resumed run
// re-attempting a measurement that in fact completed keeps the first result
// rather than replacing it.
//
// Spend is clamped at zero per dimension, the same clamp RecordOrphanSpend
// applies: a negative charge subtracts inside SettledSpend's SUM before
// anything in Go can refuse it, handing a resumed run's guard free headroom.
// docs/debt.md#48 reached this conclusion for Reservation.Settle, and every
// money writer here is a place a future caller would otherwise have to
// remember.
func (s *SQLite) RecordMeasurement(ctx context.Context, runID string, m *Measurement) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	if m == nil || m.Key.AssetID == "" || m.Key.CaseID == "" {
		return errors.New("store: measurement needs an asset ID and a case ID")
	}
	if m.Key.Arm != ArmTreatment && m.Key.Arm != ArmControl {
		return fmt.Errorf("store: measurement of case %s for asset %s has arm %s; "+
			"the arm is part of the key, so an unset one would file the control "+
			"arm on top of the treatment arm", m.Key.CaseID, m.Key.AssetID, m.Key.Arm)
	}
	if m.Key.Trial < 1 {
		// Refused, not defaulted to 1. Normalizing here writes a key the caller
		// never held, and CompletedMeasurements returns the STORED key — so a
		// resume looking for what it wrote would miss it, pay the provider
		// again, and then have the second row dropped by INSERT OR IGNORE along
		// with its spend. The run pays twice and SettledSpend never sees the
		// second payment, so a third process reseeds the guard below what was
		// actually spent. Same reasoning as the arm guard above.
		return fmt.Errorf("store: measurement of case %s for asset %s has trial %d; "+
			"trials are numbered from 1 and the number is part of the key, so a "+
			"zero would be stored as a key the caller cannot look up again",
			m.Key.CaseID, m.Key.AssetID, m.Key.Trial)
	}
	// Same guard as RecordOutcome, checked on the two fields directly rather
	// than through Scored(), which already returns false when Err is set and
	// would let a measurement carrying both slip past.
	hasScore, hasErr := m.Score != nil, m.Err != ""
	if hasScore == hasErr {
		return fmt.Errorf("store: measurement of case %s for asset %s must be "+
			"either scored or errored, not both or neither", m.Key.CaseID, m.Key.AssetID)
	}

	var responseBlob, scoreBlob []byte
	if m.Response != nil {
		if responseBlob, err = proto.Marshal(m.Response); err != nil {
			return fmt.Errorf("marshaling response for %s/%s: %w", m.Key.AssetID, m.Key.CaseID, err)
		}
	}
	if m.Score != nil {
		if scoreBlob, err = proto.Marshal(m.Score); err != nil {
			return fmt.Errorf("marshaling score for %s/%s: %w", m.Key.AssetID, m.Key.CaseID, err)
		}
	}

	scored := 0
	if m.Scored() {
		scored = 1
	}
	var scoreValue, scorePassed any // NULL when the measurement errored
	if m.Score != nil {
		scoreValue, scorePassed = m.Score.GetValue(), boolToInt(m.Score.GetPassed())
	}
	r := m.Response
	truncated := r.GetStopReason() == knov1.StopReason_STOP_REASON_LENGTH

	_, err = db.ExecContext(ctx,
		`INSERT OR IGNORE INTO measurements
		   (run_id, asset_id, case_id, arm, trial, scored, err_code,
		    response_proto, score_proto, calls, cost_usd_micros, tokens,
		    score_value, score_passed, refused, truncated, usage_estimated,
		    provider_build_id, resolved_model)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, MAX(0, ?), MAX(0, ?), MAX(0, ?), ?, ?, ?, ?, ?, ?, ?)`,
		runID, m.Key.AssetID, m.Key.CaseID, int32(m.Key.Arm), m.Key.Trial, scored, m.Err,
		responseBlob, scoreBlob,
		m.Spend.Calls, m.Spend.CostUSDMicros, m.Spend.Tokens,
		scoreValue, scorePassed,
		boolToInt(r.GetRefused()), boolToInt(truncated), boolToInt(r.GetUsageEstimated()),
		r.GetProviderBuildId(), r.GetResolvedModel())
	if err != nil {
		return fmt.Errorf("recording measurement %s/%s (%s, trial %d): %w",
			m.Key.AssetID, m.Key.CaseID, m.Key.Arm, m.Key.Trial, err)
	}
	return nil
}

// MeasurementCounts aggregates a run's measurements for CaseExecution:
// attempted, scored, and errored, read from what is DURABLE rather than from
// in-memory counters, so a resumed run's close reports the WHOLE run — the
// first process's paid rows included — instead of only the tail this process
// happened to execute.
func (s *SQLite) MeasurementCounts(ctx context.Context, runID string) (attempted, scored, errored int32, err error) {
	db, err := s.conn()
	if err != nil {
		return 0, 0, 0, err
	}
	row := db.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(*) FILTER (WHERE scored = 1),
		        COUNT(*) FILTER (WHERE err_code != '')
		   FROM measurements WHERE run_id = ?`, runID)
	if err := row.Scan(&attempted, &scored, &errored); err != nil {
		return 0, 0, 0, fmt.Errorf("counting measurements for %s: %w", runID, err)
	}
	return attempted, scored, errored, nil
}

// CompletedMeasurements returns the key of every measurement already recorded.
//
// What a Value resume consults, for the same reason CompletedCases exists for
// Baseline — and it must be this rather than CompletedCases, which reads the
// outcomes table and returns an empty set for every Value run, so a resume
// driven by it would re-pay for the whole run.
//
// Loads the full set into memory. The bound is assets x cases x arms x trials,
// two orders of magnitude past the set docs/debt.md#22 accepted for Baseline;
// that entry is re-dated to this stage rather than silently inherited.
func (s *SQLite) CompletedMeasurements(ctx context.Context, runID string) (map[MeasurementKey]struct{}, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT asset_id, case_id, arm, trial FROM measurements WHERE run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("listing completed measurements for %s: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	done := make(map[MeasurementKey]struct{})
	for rows.Next() {
		var k MeasurementKey
		var arm int32
		if err := rows.Scan(&k.AssetID, &k.CaseID, &arm, &k.Trial); err != nil {
			return nil, fmt.Errorf("scanning a completed measurement for %s: %w", runID, err)
		}
		k.Arm = Arm(arm)
		done[k] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading completed measurements for %s: %w", runID, err)
	}
	return done, nil
}

// CaseScores returns the recorded score of every Case in a run that produced
// one.
//
// Absence from the map means the Case never scored — it errored, or was never
// attempted. Presence with Unrecoverable set means it scored and the number is
// gone. Those are different facts and the Value stage reports them differently,
// which is why this does not return map[string]float64.
//
// Reads the outcomes table only. A Value run pairs against a BASELINE run's
// recorded scores, and the baseline is where those live; reading measurements
// here would let one Asset's measurement stand in as another Asset's control.
func (s *SQLite) CaseScores(ctx context.Context, runID string) (map[string]CaseScore, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT case_id, score_value, score_passed
		 FROM outcomes WHERE run_id = ? AND scored = 1`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading case scores for %s: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	scores := make(map[string]CaseScore)
	for rows.Next() {
		var (
			id     string
			v      sql.NullFloat64
			passed sql.NullBool
		)
		if err := rows.Scan(&id, &v, &passed); err != nil {
			return nil, fmt.Errorf("scanning a case score for %s: %w", runID, err)
		}
		scores[id] = CaseScore{
			Value:  v.Float64,
			Passed: passed.Bool,
			// NULL in either column: the row scored and its number is gone.
			// score_passed is written in the same statement as score_value, so
			// they are absent together, but the flag is checked rather than
			// assumed — a Passed read out of a NULL is a silent false, and
			// false is "the baseline failed this", which is what routing
			// selects ON.
			Unrecoverable: !v.Valid || !passed.Valid,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading case scores for %s: %w", runID, err)
	}
	return scores, nil
}

// RecordedMeasurement is one measurement read back: its key and what it scored.
//
// The score, not the Response. A Valuation is recomputed from numbers, and
// `kno purge` nulls the blobs — so a resume that needed the blob would find a
// purged run unresumable, which is the trade kno-examples' recipes/retention.md
// promises it is not.
type RecordedMeasurement struct {
	// Key identifies the measurement.
	Key MeasurementKey

	// Score is what the Goal returned. Meaningless when Unrecoverable or when
	// Err is set.
	Score float64

	// Unrecoverable means this measurement scored and its number is gone. Same
	// three-state discipline as CaseScore, and for the same reason: pairing
	// against a zero that stands in for a missing number manufactures a delta.
	Unrecoverable bool

	// Err is the terminal failure's code, empty when the measurement scored.
	Err string
}

// Measurements returns everything recorded for one Asset in a run.
//
// What makes WriteValuation's contract implementable. A run stopped by its cost
// cap part-way through an Asset leaves paid measurements and no Valuation, and
// the resume must recompute that Valuation over BOTH processes' measurements —
// so it has to read back what the first process scored. Without this it could
// only recompute over its own half, which is the delta-over-half-a-sample that
// leaving the Valuation unwritten exists to prevent, or re-pay to recover the
// numbers, which is the double-spend the table exists to prevent.
//
// Ordered by the full key, so a recomputation is reproducible and a golden test
// over it is not flaky.
func (s *SQLite) Measurements(ctx context.Context, runID, assetID string) ([]RecordedMeasurement, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT case_id, arm, trial, score_value, score_proto, scored, err_code
		 FROM measurements WHERE run_id = ? AND asset_id = ?
		 ORDER BY case_id, arm, trial`, runID, assetID)
	if err != nil {
		return nil, fmt.Errorf("reading measurements for %s/%s: %w", runID, assetID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []RecordedMeasurement
	for rows.Next() {
		var (
			rec   RecordedMeasurement
			arm   int32
			value sql.NullFloat64
			blob  []byte
			score int
		)
		if err := rows.Scan(&rec.Key.CaseID, &arm, &rec.Key.Trial,
			&value, &blob, &score, &rec.Err); err != nil {
			return nil, fmt.Errorf("scanning a measurement for %s/%s: %w", runID, assetID, err)
		}
		rec.Key.AssetID, rec.Key.Arm = assetID, Arm(arm)
		rec.Score = value.Float64
		// Scored with no number left: the same state CaseScore reports, and it
		// must not read as a score of zero.
		rec.Unrecoverable = score == 1 && !value.Valid
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading measurements for %s/%s: %w", runID, assetID, err)
	}
	return out, nil
}

// WriteValuation records one Asset's finished Valuation.
//
// Written only when an Asset's measurements are ALL in. A run stopped by its
// cost cap part-way through an Asset leaves the paid measurements durable and
// no Valuation, so resume finishes the Asset from where it stopped and pays for
// nothing twice — while nothing downstream can read a delta computed over half
// a sample.
//
// INSERT OR REPLACE, unlike every other writer here. A Valuation is DERIVED
// from the measurements rather than being itself a record of spend, so
// recomputing one after a resume must produce the row that matches what is now
// recorded; keeping a stale first write would pin the report to a partial
// sample. The measurements it is derived from remain insert-or-ignore.
func (s *SQLite) WriteValuation(ctx context.Context, runID string, v *knov1.Valuation) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	if v.GetAssetId() == "" {
		return errors.New("store: valuation needs an asset ID")
	}
	blob, err := proto.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling valuation for %s: %w", v.GetAssetId(), err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO valuations (run_id, asset_id, proto) VALUES (?, ?, ?)`,
		runID, v.GetAssetId(), blob); err != nil {
		return fmt.Errorf("recording valuation for %s: %w", v.GetAssetId(), err)
	}
	return nil
}

// Valuations returns every Valuation recorded for a run, ordered by Asset ID.
//
// Ordered because the report and the `--json` output are golden-tested, and
// SQLite promises no order without an ORDER BY — an unordered read would make
// those tests flaky rather than wrong, which is worse.
func (s *SQLite) Valuations(ctx context.Context, runID string) ([]*knov1.Valuation, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT proto FROM valuations WHERE run_id = ? ORDER BY asset_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading valuations for %s: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []*knov1.Valuation
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, fmt.Errorf("scanning a valuation for %s: %w", runID, err)
		}
		v := &knov1.Valuation{}
		if err := proto.Unmarshal(blob, v); err != nil {
			return nil, fmt.Errorf("unmarshaling a valuation for %s: %w", runID, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading valuations for %s: %w", runID, err)
	}
	return out, nil
}

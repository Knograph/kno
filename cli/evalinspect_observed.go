package cli

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"strings"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/store"
)

// inspectObserved is what a recorded Value run actually did. Present only when
// --value-run-id names a run whose plan decodes and whose eval fingerprint
// still matches the source.
type inspectObserved struct {
	ValueRunID     string
	ValueRunStatus string
	BaselineRunID  string
	RoutingMode    string
	EligibleCases  int
	ControlCases   int

	// ControlUnderpowered and MinDetectableHarm are Plan's own fields,
	// unchanged. MinDetectableHarm is ONE-SIDED — it answers "did this get
	// worse", a directional question — while every behavior's SeparableEffect
	// is two-sided. Both are labeled at every appearance so the two cannot be
	// mistaken for each other.
	ControlUnderpowered bool
	MinDetectableHarm   float64

	Behaviors []inspectObservedBehavior
}

// inspectObservedBehavior is one planned cluster and its recorded verdict.
type inspectObservedBehavior struct {
	Tag string
	// ClusterCases is how many dev Cases routing clustered as failures under
	// this tag. Failures by construction: cluster() clusters only the Cases
	// the baseline failed, from the eligible partition.
	ClusterCases int
	// DevCases is the same tag's dev Case count in the eval source, so the two
	// read as "N of M". Zero when the tag no longer appears in the source.
	DevCases     int
	GapStatus    string
	BestAssetID  string
	BestDelta    float64
	CoveredCount int
}

// attachObserved loads the named Value run and, if everything lines up,
// attaches what it did. Every "does not line up" branch leaves Observed nil and
// lets checkObserved report UNKNOWN with the reason — never a guess.
func attachObserved(ctx context.Context, src evalSource, insp *inspection, f evalInspectFlags) error {
	db, err := store.NewSQLite(ctx, f.dbPath)
	if err != nil {
		return errs.ErrInvalidInput.WithFix("check --db is readable").Wrap(err)
	}
	defer func() { _ = db.Close() }()

	run, err := db.GetRun(ctx, f.valueRunID)
	if err != nil {
		return errs.ErrInvalidInput.
			WithFix("pass the run ID `kno value` printed when it finished").
			Wrap(fmt.Errorf("loading value run %s: %w", f.valueRunID, err))
	}
	if got := run.GetStage(); got != knov1.Stage_STAGE_VALUE {
		return errs.ErrInvalidInput.
			WithFix("pass the run ID of a `kno value` run").
			Wrap(fmt.Errorf("run %s is a %s run, not a value run", f.valueRunID, got))
	}

	insp.observedUnknown = observedReasonFor(ctx, src, run)
	if insp.observedUnknown != "" {
		return nil
	}

	var plan value.Plan
	if err := gob.NewDecoder(bytes.NewReader(run.GetValuePlan())).Decode(&plan); err != nil {
		// The treatment core/export.go already applies to an undecodable plan:
		// no cluster data for this run, never a panic and never a guess.
		insp.observedUnknown = "the recorded routing plan could not be decoded"
		return nil
	}
	valuations, err := db.Valuations(ctx, f.valueRunID)
	if err != nil {
		return fmt.Errorf("loading the value run's valuations: %w", err)
	}
	insp.Observed = observedFrom(run, &plan, core.ComputeGaps(&plan, valuations), insp)
	return nil
}

// observedReasonFor reports why the observed section cannot be rendered, or ""
// when it can.
func observedReasonFor(ctx context.Context, src evalSource, run *knov1.Run) string {
	if len(run.GetValuePlan()) == 0 {
		return "the run recorded no routing plan (it stopped before planning)"
	}
	hash, err := src.ContentHash(ctx)
	if err != nil {
		return "the eval source's fingerprint could not be read"
	}
	if recorded := run.GetEvalContentHash(); recorded != "" && recorded != hash {
		return "the eval source has changed since this run"
	}
	return ""
}

// observedFrom composes the observed section from the plan and the verdicts.
func observedFrom(
	run *knov1.Run, plan *value.Plan, gaps *knov1.Gaps, insp *inspection,
) *inspectObserved {
	devByTag := make(map[string]int, len(insp.Behaviors))
	for _, b := range insp.Behaviors {
		devByTag[b.Tag] = b.DevCases
	}
	o := &inspectObserved{
		ValueRunID:          run.GetId(),
		ValueRunStatus:      statusName(run.GetStatus()),
		BaselineRunID:       run.GetBaselineRunId(),
		RoutingMode:         plan.Mode.String(),
		EligibleCases:       plan.EligibleCases,
		ControlCases:        len(plan.ControlCaseIDs),
		ControlUnderpowered: plan.ControlUnderpowered,
		MinDetectableHarm:   plan.MinDetectableHarm,
	}
	for _, cl := range gaps.GetClusters() {
		o.Behaviors = append(o.Behaviors, inspectObservedBehavior{
			Tag:          cl.GetTag(),
			ClusterCases: int(cl.GetCaseCount()),
			DevCases:     devByTag[cl.GetTag()],
			GapStatus:    gapStatusName(cl.GetStatus()),
			BestAssetID:  cl.GetBestAssetId(),
			BestDelta:    cl.GetBestDelta(),
			CoveredCount: int(cl.GetCoveredCount()),
		})
	}
	return o
}

// gapStatusName renders a GapStatus the way this command's contract spells it:
// the enum's short name, lowercased. A hand-written jq contract, not a
// protojson encoding (ADR-0001), matching stageName and destinationName.
func gapStatusName(s knov1.GapStatus) string {
	switch s {
	case knov1.GapStatus_GAP_STATUS_IMPROVED:
		return "improved"
	case knov1.GapStatus_GAP_STATUS_GAP:
		return "gap"
	case knov1.GapStatus_GAP_STATUS_UNKNOWN, knov1.GapStatus_GAP_STATUS_UNSPECIFIED:
		return "unknown"
	default:
		return "unknown"
	}
}

// checkObserved: what did routing actually do, and did every behavior get a
// verdict?
//
// UNKNOWN without --value-run-id — never ok, because a static property of the
// eval file must not be reported as confirmed by a run that was never named.
// Flagged only on conditions already anchored in the tree: routing that
// degraded to all-failed, a control arm the plan itself marked underpowered, or
// a cluster whose verdict is GAP_STATUS_UNKNOWN, which is ComputeGaps saying
// "we did not really look".
func (i *inspection) checkObserved(valueRunID string) inspectCheck {
	c := inspectCheck{Name: checkAttributionObserved, Status: inspectUnknown}
	switch {
	case valueRunID == "":
		c.Detail = "no --value-run-id given"
		return c
	case i.Observed == nil:
		c.Detail = i.observedUnknown
		return c
	}

	o := i.Observed
	var why []string
	if o.RoutingMode == value.ModeAllFailed.String() {
		why = append(why, "routing ran in all-failed mode, so nothing was attributed per behavior")
	}
	if o.ControlUnderpowered {
		why = append(why, fmt.Sprintf(
			"the control arm was underpowered (minimum detectable harm %.2f, one-sided 95%%)",
			o.MinDetectableHarm))
	}
	if n := o.unknownVerdicts(); n > 0 {
		why = append(why, fmt.Sprintf("%d behaviors got no verdict (GAP_STATUS_UNKNOWN)", n))
	}
	if len(why) == 0 {
		c.Status = inspectOK
		c.Detail = fmt.Sprintf("routing ran in %s mode; every behavior got a verdict", o.RoutingMode)
		return c
	}
	c.Status = inspectFlagged
	c.Detail = strings.Join(why, "; ")
	return c
}

// unknownVerdicts counts the clusters ComputeGaps could not testify about.
func (o *inspectObserved) unknownVerdicts() int {
	n := 0
	for _, b := range o.Behaviors {
		if b.GapStatus == "unknown" {
			n++
		}
	}
	return n
}

package bridge

import (
	"fmt"
	"sort"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	"github.com/knograph/kno/core/value"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// AllIn names the group that trains on every population Asset — the
// baseline every leave-one-out group is compared against.
const AllIn = "all-in"

// Population computes the tuning-set Asset population from a Portfolio:
// every DESTINATION_TUNING_SET entry's Asset ID, in Portfolio order.
//
// Refuses a Portfolio whose tuning-set entries include a KIND_KNOWLEDGE
// Asset. The plan describes this refusal as conditional on
// `user_overridden` — a manually-pinned knowledge Asset the pool operator
// deliberately routed to the tuning set. NO SUCH FIELD EXISTS on
// PortfolioEntry or Valuation today (proto/kno/v1/portfolio.proto,
// proto/kno/v1/valuation.proto — both owned by a parallel workstream this
// PR does not edit), so this build refuses EVERY KIND_KNOWLEDGE entry
// unconditionally. That is the strictly SAFER subset of the specified
// behavior: it never bridges a routing bug (the property the refusal
// exists for), it simply cannot yet honor a deliberate override. See this
// PR's report.
func Population(p *knov1.Portfolio) ([]string, error) {
	var out []string
	for _, e := range p.GetSelected() {
		if e.GetDestination() != knov1.Destination_DESTINATION_TUNING_SET {
			continue
		}
		if e.GetValuation().GetKind() == knov1.Kind_KIND_KNOWLEDGE {
			return nil, errs.ErrInvalidInput.
				WithFix("route this Asset to a different Destination, or remove it from the Portfolio before bridging").
				Wrap(fmt.Errorf(
					"asset %s is KIND_KNOWLEDGE but is routed to the tuning set; "+
						"the bridge measures fine-tuning transfer and Tier 1's whole "+
						"claim is that knowledge never faces it — bridging a "+
						"knowledge Asset would pay to fine-tune on a routing bug, "+
						"not to take a measurement", e.GetAssetId(),
				))
		}
		out = append(out, e.GetAssetId())
	}
	return out, nil
}

// GroupsPlan is the bridge's job plan: which groups get tuned, which
// clusters were skipped, and which population Assets have no bridge
// verdict at all.
type GroupsPlan struct {
	// AllIn is every population Asset with a primary group — the training
	// set for the AllIn job. Excludes Unknown Assets: an Asset with no
	// primary group is not folded into "all-in" either, because the
	// all-in/leave-one-out comparison is only meaningful over Assets whose
	// membership is defined.
	AllIn []string

	// LeaveOneOut is one entry per cluster that both has at least one
	// primary-assigned population Asset and meets core.MinClusterCases: the
	// group's name (the cluster tag) mapped to AllIn minus that cluster's
	// members — the training set for that group's leave-one-out job.
	LeaveOneOut map[string][]string

	// Skipped is clusters that had primary-assigned population Assets but
	// fell below core.MinClusterCases — reported, zero jobs.
	Skipped []string

	// Unknown is population Assets with no primary group (routed to zero
	// clusters). They get no bridge verdict.
	Unknown []string
}

// Groups reports every group name that gets a job, "all-in" first, then the
// leave-one-out groups sorted by cluster tag — the order jobs are quoted and
// submitted in.
func (g *GroupsPlan) Groups() []string {
	out := make([]string, 0, len(g.LeaveOneOut)+1)
	out = append(out, AllIn)
	tags := make([]string, 0, len(g.LeaveOneOut))
	for tag := range g.LeaveOneOut {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	out = append(out, tags...)
	return out
}

// ErrTooManyGroups means the plan's cluster count exceeds --bridge-max-groups.
var ErrTooManyGroups = fmt.Errorf("bridge: too many ablation groups")

// BuildGroups turns a value.Plan and a tuning-set population into the
// bridge's GroupsPlan: 1 all-in job plus one leave-one-out job per
// qualifying cluster, per the tuner-bridge plan's Step 3.
//
// A cluster QUALIFIES when it has at least one primary-assigned population
// Asset (see AssignGroups) AND at least core.MinClusterCases dev Cases —
// the same floor ComputeGaps already uses to decide when a cluster is too
// small to measure. A cluster with primary-assigned Assets but too few
// Cases is SKIPPED and reported, not tuned: the measurement it would buy is
// already known to be underpowered.
//
// maxGroups caps the number of LEAVE-ONE-OUT jobs (the all-in job is always
// exactly one job regardless of cluster count, matching DESIGN's own count
// of "all-in + N leave-one-out"). Beyond the cap the whole run is REFUSED,
// never merged or truncated — a merged group's LOO delta is uninterpretable.
func BuildGroups(plan *value.Plan, population []string, maxGroups int) (*GroupsPlan, error) {
	assignments := AssignGroups(plan, population)

	byCluster := make(map[string][]string) // cluster tag -> primary-assigned population Assets
	var unknown []string
	for _, a := range assignments {
		if a.Unknown {
			unknown = append(unknown, a.AssetID)
			continue
		}
		byCluster[a.Cluster] = append(byCluster[a.Cluster], a.AssetID)
	}

	caseCountByTag := make(map[string]int, len(plan.Clusters))
	for _, c := range plan.Clusters {
		caseCountByTag[c.Tag] = len(c.CaseIDs)
	}

	var allIn []string
	for _, a := range assignments {
		if !a.Unknown {
			allIn = append(allIn, a.AssetID)
		}
	}
	sort.Strings(allIn)

	loo := make(map[string][]string)
	var skipped []string
	var qualifying []string
	for tag, members := range byCluster {
		if caseCountByTag[tag] < core.MinClusterCases {
			skipped = append(skipped, tag)
			continue
		}
		qualifying = append(qualifying, tag)
		excluded := make(map[string]struct{}, len(members))
		for _, id := range members {
			excluded[id] = struct{}{}
		}
		var without []string
		for _, id := range allIn {
			if _, ok := excluded[id]; !ok {
				without = append(without, id)
			}
		}
		loo[tag] = without
	}

	if maxGroups > 0 && len(qualifying) > maxGroups {
		sort.Strings(qualifying)
		return nil, errs.ErrInvalidInput.
			WithFix(fmt.Sprintf(
				"raise --bridge-max-groups above %d; each group adds one fine-tuning job",
				len(qualifying),
			)).
			Wrap(fmt.Errorf(
				"%w: %d qualifying clusters exceed the cap of %d (%v)",
				ErrTooManyGroups, len(qualifying), maxGroups, qualifying,
			))
	}

	sort.Strings(skipped)
	sort.Strings(unknown)
	return &GroupsPlan{AllIn: allIn, LeaveOneOut: loo, Skipped: skipped, Unknown: unknown}, nil
}

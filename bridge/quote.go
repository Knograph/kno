package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/knograph/kno/adapters/agent/pricing"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// GroupQuote is one ablation group's local, un-armed estimate — Step 2(a)'s
// arithmetic and Step 4's "the un-armed run IS the estimator" in one
// struct: it costs zero network calls and zero dollars to produce.
type GroupQuote struct {
	// Group is "all-in", or the cluster tag being left out.
	Group string

	// AssetIDs is this group's training-set membership.
	AssetIDs []string

	// TrainingData is the rendered training file — byte-identical to what
	// `kno export --destination tuning_set` would write for the same
	// Portfolio filtered to AssetIDs. See core.RenderTuningSetForAssets.
	TrainingData []byte

	// TrainingFileSHA256 is what gets persisted on the durable job row —
	// the training data itself never is. See store.TuningJobRecord.
	TrainingFileSHA256 string

	// TrainTokens is the pessimistic token count EstimatedCostUSDMicros was
	// computed from.
	TrainTokens int64

	// EstimatedCostUSDMicros is the pessimistic training cost, per
	// pricing.EstimateTrain.
	EstimatedCostUSDMicros int64
}

// TotalEstimatedCostUSDMicros sums every quote's training estimate — the
// job-list total the consent quote and the un-armed plan print.
func TotalEstimatedCostUSDMicros(quotes []GroupQuote) int64 {
	var total int64
	for _, q := range quotes {
		total += q.EstimatedCostUSDMicros
	}
	return total
}

// QuoteGroups renders every group's training file and prices it, without
// spending anything or making any network call.
//
// This is the whole of the bridge's un-armed run: Step 4 says "the un-armed
// run IS the estimator... over the network zero times, spending zero", and
// this function is where that property lives — it takes a Tuner only
// through baseModel's name (for token counting and pricing lookup), never
// a core.Tuner value, so a caller literally cannot reach the network from
// here.
//
// price must be looked up by the caller (pricing.LookupTrainPrice) and
// passed in refused-or-present: an unpriced base model is refused before
// this function is ever called, per Step 2(a) — "There is no
// --accept-unknown-cost escape for the bridge." A group whose rendered
// training file is empty is refused here: a zero-example fine-tune is a
// paid no-op.
func QuoteGroups(
	p *knov1.Portfolio,
	plan *GroupsPlan,
	assets map[string]*core.Asset,
	baseModelName string,
	price pricing.TrainPrice,
	epochs int32,
) ([]GroupQuote, error) {
	var out []GroupQuote
	for _, name := range plan.Groups() {
		ids := plan.AllIn
		if name != AllIn {
			ids = plan.LeaveOneOut[name]
		}

		data, err := core.RenderTuningSetForAssets(p, ids, assets)
		if err != nil {
			return nil, fmt.Errorf("rendering the %s group's training file: %w", name, err)
		}
		if len(data) == 0 {
			return nil, errs.ErrInvalidInput.
				WithFix("this group has no renderable training examples; check the Portfolio's tuning-set entries").
				Wrap(fmt.Errorf("the %s group's training file has zero examples; "+
					"a zero-example fine-tune is a paid no-op", name))
		}

		tokens := pricing.CountTokens(len(data), baseModelName)
		cost := pricing.EstimateTrain(price, tokens, epochs)
		sum := sha256.Sum256(data)

		out = append(out, GroupQuote{
			Group:                  name,
			AssetIDs:               ids,
			TrainingData:           data,
			TrainingFileSHA256:     hex.EncodeToString(sum[:]),
			TrainTokens:            tokens,
			EstimatedCostUSDMicros: cost,
		})
	}
	return out, nil
}

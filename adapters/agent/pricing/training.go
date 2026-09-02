package pricing

import (
	"math"
	"math/bits"

	"google.golang.org/protobuf/proto"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TrainPrice is a training-token rate for a base model, plus an optional
// per-job floor.
//
// A plain Go struct, not a knov1.Price. Price's four fields are all
// INFERENCE rates (input/cached-input/cache-write/output per million
// tokens): a training rate is a different dimension entirely — a model can
// be trainable and not servable and vice versa — and a nil rate on Price
// already means "not billed separately" rather than "no price exists",
// which is not the claim an unpriced TRAINING rate needs to make. Overloading
// a fifth meaning onto Price would blur that distinction rather than extend
// it. This mirrors stats/budget.Estimate/Spend, which are plain Go structs
// for the analogous reason: the money domain does not need the wire format
// wherever it is computed.
type TrainPrice struct {
	// PerMTokUSDMicros is the rate per million TRAINING tokens, in micro-USD.
	PerMTokUSDMicros int64

	// FloorUSDMicros is a per-job minimum charge. Zero means no floor.
	FloorUSDMicros int64
}

// ServePrice is a per-minute hosting rate for a deployed (served) model —
// Step 2(f)'s second spend dimension. Billed per REPLICA per MINUTE,
// including while idle.
type ServePrice struct {
	// PerMinuteUSDMicros is the rate per replica per minute, in micro-USD.
	PerMinuteUSDMicros int64
}

// trainTable and serveTable are keyed table[scheme][baseModel], exactly like
// table in table.go.
//
// BOTH SHIP EMPTY. The bridge plan tags Together's exact per-model training
// rate and Together's per-minute dedicated-endpoint rate ***(verify)*** —
// Phase 1 confirmed the SHAPE (a published per-token training rate exists,
// roughly $0.48-$3.20/MTok by base-model size and method) but not a specific
// number this build can stand behind on an $3-8, irreversible-at-submission
// spend path. Shipping a guessed number here would be confidently wrong in
// exactly the class of mistake CLAUDE.md's prime directive 4 exists to
// prevent — better to ship the mechanism unpriced than to ship a wrong
// price that authorizes real money. Until a reviewed diff adds rows (the
// same discipline table.go's own doc comment describes for the inference
// table), every base and served model is refused under LookupTrainPrice and
// LookupServePrice, and the only way to run a bridge is
// --price-train-per-mtok / --price-serve-per-minute — the same explicit,
// user-stated escape hatch the plan specifies in place of
// --accept-unknown-cost. See docs/debt.md for the ledger entry this ships
// with, whose trigger includes "the pricing drift detector gains a
// training-price or serve-price check".
var trainTable = map[string]map[string]TrainPrice{}

var serveTable = map[string]map[string]ServePrice{}

// fineTunedTable is the THIRD bridge pricing dimension: what a fine-tuned
// model costs to INVOKE, keyed table[scheme][baseModel] exactly like
// trainTable and serveTable above.
//
// SHIPS EMPTY, for the same reason those two do, and one the eval-seam
// pricing plan's third review round insisted on stating precisely: a
// fine-tuned model's inference rate is NOT the kind of unmeasurable
// provider-internal slop TrainingHeadroomPct and RegionalMultiplierPct
// bound. OpenAI publishes it, per base model, on the same page table.go
// already cites. A multiplier applied to the BASE rate here would be
// exactly the fabricated-rate failure table.go's own package doc refuses —
// "An unknown model is NOT priced at zero... Lookup reports absence; the
// caller decides" — dressed up as a safety margin. It is not a margin: it is
// a guess wearing a percentage sign, on a spend path with a real number one
// lookup away. Until a reviewed diff adds rows (the same discipline
// table.go and trainTable/serveTable both apply), every base model is
// refused under LookupFineTunedPrice, which is the same "an unpriced model
// under a cap is refused, never guessed" rule core/score.go's
// AcceptFreeCalls-false path already enforces for every other Agent. See
// docs/debt.md for the ledger entry this ships with.
//
// Values are knov1.Price, not a bespoke struct like TrainPrice/ServePrice:
// a fine-tuned inference rate is the SAME shape as the base inference rates
// table.go carries (input/cached-input/cache-write/output per million
// tokens) — it is Estimate's own p argument, fed straight into
// pricing.EstimateWithPrice, the same function WorstCase already calls.
// TrainPrice and ServePrice are bespoke because training and hosting are
// genuinely different dimensions (see TrainPrice's doc); inference is not.
var fineTunedTable = map[string]map[string]*knov1.Price{}

// LookupFineTunedPrice returns the published fine-tuned inference rate for a
// base model — the rate OpenAI (or any per-token provider) bills for calls
// against a model it fine-tuned from baseModel, which is ABOVE the base
// rate table.go's Lookup carries.
//
// The second return is false for a model with no row — until fineTunedTable
// gains entries, every model — and the caller must not substitute the base
// rate, a multiplier, or zero: see fineTunedTable's doc. A COPY, always, for
// the identical data-race reason Lookup's own doc gives.
func LookupFineTunedPrice(scheme, model string) (*knov1.Price, bool) {
	byModel, ok := fineTunedTable[scheme]
	if !ok {
		return nil, false
	}
	if p, ok := byModel[model]; ok {
		return proto.CloneOf(p), true
	}
	if base, ok := longestPrefix(model, byModel); ok && isVersionSuffix(model[len(base):]) {
		return proto.CloneOf(byModel[base]), true
	}
	return nil, false
}

// TrainingHeadroomPct is the provider-class headroom multiplier Step 2(a)
// calls for: providers bill packed sequences, padding, and sometimes an
// automatic validation split, none of which a local token count sees. 120
// means "charge 1.20x" — a DOCUMENTED CONSTANT, not a measured one (in the
// same class as RegionalMultiplierPct's own +10%, but larger: that constant
// corrects a known, bounded regional markup, and this one bounds several
// unmeasured provider behaviors at once, so it takes the more conservative
// side of unknown rather than guessing tighter). See docs/debt.md: this
// constant is exactly the kind of thing the pricing drift detector should
// gain a check for.
const TrainingHeadroomPct = 120

// LookupTrainPrice returns the training rate for a base model.
//
// The second return is false for a model with no row — which, until
// trainTable gains entries, is every model — and the caller must refuse
// rather than substitute zero: a zero estimate makes a dollar cap
// unenforceable on a $3-8 commitment that cannot be un-submitted.
func LookupTrainPrice(scheme, model string) (TrainPrice, bool) {
	byModel, ok := trainTable[scheme]
	if !ok {
		return TrainPrice{}, false
	}
	if p, ok := byModel[model]; ok {
		return p, true
	}
	if base, ok := longestPrefix(model, byModel); ok && isVersionSuffix(model[len(base):]) {
		return byModel[base], true
	}
	return TrainPrice{}, false
}

// LookupServePrice returns the hosting rate for a served (deployed) model.
// See LookupTrainPrice for the presence contract.
func LookupServePrice(scheme, model string) (ServePrice, bool) {
	byModel, ok := serveTable[scheme]
	if !ok {
		return ServePrice{}, false
	}
	if p, ok := byModel[model]; ok {
		return p, true
	}
	if base, ok := longestPrefix(model, byModel); ok && isVersionSuffix(model[len(base):]) {
		return byModel[base], true
	}
	return ServePrice{}, false
}

// EstimateTrain reports the most a fine-tuning job could cost, in MICRO-USD.
//
// Pessimistic by construction, matching core.Estimator's doctrine for the
// inference path: trainTokens is expected to already be
// pricing.CountTokens's output (which applies safetyMargin), this multiplies
// by epochs, applies TrainingHeadroomPct, and floors the result — never the
// reverse order, because flooring before headroom would let the multiplier
// erase a floor meant to be a MINIMUM.
func EstimateTrain(price TrainPrice, trainTokens int64, epochs int32) int64 {
	if epochs <= 0 {
		epochs = 1
	}
	perEpoch := perMTok(price.PerMTokUSDMicros, trainTokens)
	total := saturatingMul(perEpoch, int64(epochs))
	total = Regional(total, TrainingHeadroomPct)
	if price.FloorUSDMicros > total {
		total = price.FloorUSDMicros
	}
	return total
}

// EstimateServeCap reports the worst-case hosting cost for one endpoint,
// bounded by maxServeMinutes and maxReplicas — the cap-bounded quote Step
// 2(f) puts in the consent line, stated as a worst case rather than a
// prediction because hosting is stoppable and usually costs less.
func EstimateServeCap(price ServePrice, maxServeMinutes int, maxReplicas int) int64 {
	if maxServeMinutes <= 0 || maxReplicas <= 0 {
		return 0
	}
	perMinuteAllReplicas := saturatingMul(price.PerMinuteUSDMicros, int64(maxReplicas))
	return saturatingMul(perMinuteAllReplicas, int64(maxServeMinutes))
}

// SettleServeMinutes reports what minutes actually accrued cost, at price's
// rate — the settle-forward counterpart to EstimateServeCap's worst-case
// quote. Called once per tick with the WHOLE-MINUTE delta since the last
// settled tick (never a running total), so a caller settling incrementally
// never re-prices minutes it already settled.
func SettleServeMinutes(price ServePrice, minutes int, replicas int) int64 {
	if minutes <= 0 || replicas <= 0 {
		return 0
	}
	perMinuteAllReplicas := saturatingMul(price.PerMinuteUSDMicros, int64(replicas))
	return saturatingMul(perMinuteAllReplicas, int64(minutes))
}

// saturatingMul multiplies two non-negative int64s, saturating at
// math.MaxInt64 rather than wrapping. A wrapped product can land small and
// positive, which reads as a cheap call rather than as an error — the same
// failure perMTok and Regional both guard against.
func saturatingMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 || lo > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(lo)
}

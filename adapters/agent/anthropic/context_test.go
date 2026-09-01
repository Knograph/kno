package anthropic_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/anthropic"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// These tests are about ONE property that nothing else in this package can
// check: that context injection produces a treatment arm without disturbing the
// control arm, and that everything the budget guard reads comes with it. A
// wrapper that forgot either would pass every other test here.

// assetText is the injected Asset's content. Distinctive enough that its
// position in a request body is unambiguous, and long enough that a token
// estimate visibly moves.
const assetText = "ASSET-CONTENT-" +
	"0123456789012345678901234567890123456789012345678901234567890123456789" +
	"0123456789012345678901234567890123456789012345678901234567890123456789" +
	"0123456789012345678901234567890123456789012345678901234567890123456789"

// answeredBody is a well-formed Messages API reply, enough for Invoke to reach
// the end without the response shape being the subject.
const answeredBody = `{
  "id": "msg_ctx",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-4-6",
  "content": [{"type": "text", "text": "4"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 10, "output_tokens": 2}
}`

// anAsset is a well-formed text Asset.
func anAsset(content string) *core.Asset {
	return &knov1.Asset{Id: "asset-1", Content: []byte(content)}
}

// injected is WithContext with the failure made fatal, for the tests where the
// injection succeeding is a precondition rather than the subject.
func injected(t *testing.T, a *anthropic.Agent, content string) core.Agent {
	t.Helper()
	g, err := a.WithContext(anAsset(content))
	if err != nil {
		t.Fatalf("WithContext: %v", err)
	}
	return g
}

// newInjectableAgent builds an Agent aimed at a server that answers every Case
// and records what it was sent.
//
// Assertions are made against what the SERVER saw rather than against anything
// the adapter reports about itself: a client-side record of what it believes it
// sent cannot detect the case where something else was sent.
func newInjectableAgent(t *testing.T, tweak ...func(*anthropic.Options)) (*anthropic.Agent, *capture) {
	t.Helper()
	srv, rec := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		answer(w, answeredBody)
	})
	// A nil tweak is how a table row says "the defaults"; newAgent calls every
	// entry it is handed.
	opts := make([]func(*anthropic.Options), 0, len(tweak))
	for _, f := range tweak {
		if f != nil {
			opts = append(opts, f)
		}
	}
	return newAgent(t, srv, opts...), rec
}

// TestTheAssetIsSentImmediatelyAfterTheSystemPromptAndAheadOfTheCase.
//
// The POSITION is the assertion, not the presence. Providers cache on a prefix,
// and [system][asset] is byte-identical across every Case in an Asset's sample
// while the Case varies — so an Asset placed there is billed at the cache-read
// rate for the whole sample, and an Asset placed behind the history is billed
// fresh on every single Case. costOf prices those at four separate rates, so
// this is a cost property and not a stylistic one.
//
// The Messages API takes the system prompt as a TOP-LEVEL field, so "after the
// system prompt and before the Case" is expressed by joining the two — which
// puts the Asset ahead of every message, including a ROLE_SYSTEM history turn,
// because a history turn belongs to the Case and varies with it.
func TestTheAssetIsSentImmediatelyAfterTheSystemPromptAndAheadOfTheCase(t *testing.T) {
	t.Parallel()

	a, rec := newInjectableAgent(t, func(o *anthropic.Options) { o.System = "SYSTEM-PROMPT" })

	c := aCase("CASE-INPUT")
	c.History = []*knov1.Turn{
		{Role: knov1.Role_ROLE_SYSTEM, Content: "CASE-SYSTEM-TURN"},
		{Role: knov1.Role_ROLE_USER, Content: "HISTORY-TURN"},
	}

	if _, err := injected(t, a, "ASSET-CONTENT").Invoke(t.Context(), c); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// The exact system field, not three Contains calls. Contains would pass on
	// an Asset appended after the Case's own system turn, which is the
	// arrangement that breaks the cacheable prefix.
	const wantSystem = `"system":"SYSTEM-PROMPT\n\nASSET-CONTENT\n\nCASE-SYSTEM-TURN"`
	got := rec.body(t, 0)
	if !strings.Contains(got, wantSystem) {
		t.Errorf("the Asset is not sent immediately after the system prompt.\n got %s\nwant a body containing %s", got, wantSystem)
	}
	if strings.Index(got, "ASSET-CONTENT") > strings.Index(got, "HISTORY-TURN") {
		t.Errorf("the Asset is sent behind the Case's history, so every Case pays "+
			"for it fresh instead of reading it from the provider's cache: %s", got)
	}
}

// TestTheReceiverStillSendsNoAssetAfterInjection.
//
// The control-arm contract, and the one failure that would corrupt every
// measurement this stage makes rather than just one. Value reports a PAIRED
// difference between an Agent carrying the Asset and the same Agent without it;
// if WithContext mutated its receiver, both arms would carry the Asset, every
// delta would be exactly zero, and the report would present that as a measured
// finding with a tight interval around it.
func TestTheReceiverStillSendsNoAssetAfterInjection(t *testing.T) {
	t.Parallel()

	a, rec := newInjectableAgent(t, func(o *anthropic.Options) { o.System = "SYSTEM-PROMPT" })
	c := aCase("CASE-INPUT")

	before, err := a.Estimate(t.Context(), c)
	if err != nil {
		t.Fatalf("Estimate before injection: %v", err)
	}
	beforeWorst := a.WorstCase()

	treatment := injected(t, a, assetText)
	if _, err := treatment.Invoke(t.Context(), c); err != nil {
		t.Fatalf("Invoke on the treatment arm: %v", err)
	}
	if _, err := a.Invoke(t.Context(), c); err != nil {
		t.Fatalf("Invoke on the control arm: %v", err)
	}

	if got := rec.body(t, 0); !strings.Contains(got, assetText) {
		t.Fatalf("the treatment arm sent no Asset, so this test cannot tell the two arms apart: %s", got)
	}
	if got := rec.body(t, 1); strings.Contains(got, assetText) {
		t.Errorf("the control arm carries the Asset, so every paired delta would be "+
			"zero and would be reported as a measurement: %s", got)
	}

	after, err := a.Estimate(t.Context(), c)
	if err != nil {
		t.Fatalf("Estimate after injection: %v", err)
	}
	if before.CostUSDMicros != after.CostUSDMicros || before.Tokens != after.Tokens {
		t.Errorf("injection moved the receiver's own estimate from %+v to %+v; the "+
			"control arm would then reserve for a prompt it does not send", before, after)
	}
	if beforeWorst.CostUSDMicros != a.WorstCase().CostUSDMicros {
		t.Errorf("injection moved the receiver's memoized WorstCase from %d to %d",
			beforeWorst.CostUSDMicros, a.WorstCase().CostUSDMicros)
	}
}

// TestTheInjectedAgentForwardsEverythingTheGuardReads.
//
// WithContext returns core.Agent — the NARROWEST interface — so an
// implementation that wrapped rather than copied would drop Estimator and
// Capable without a compile error. Dropping Estimator is the expensive one:
// core falls back to BaselineOptions.EstCostPerCallUSDMicros, a run-scoped
// scalar that knows nothing about the Asset, and every reservation is then made
// against a constant while the prompt carries the whole Asset. WorstCase's own
// godoc records that failure already measured once — $0.06 quoted for $12.00 of
// real exposure.
func TestTheInjectedAgentForwardsEverythingTheGuardReads(t *testing.T) {
	t.Parallel()

	a, _ := newInjectableAgent(t)
	treatment := injected(t, a, assetText)

	est, ok := treatment.(core.Estimator)
	if !ok {
		t.Fatal("the injected Agent is not an Estimator; the budget guard would " +
			"reserve against a run-scoped scalar containing no Asset while every " +
			"prompt carried one")
	}
	capable, ok := treatment.(core.Capable)
	if !ok {
		t.Fatal("the injected Agent is not Capable; the stage cannot check the " +
			"injection mode it is about to use")
	}
	if !capable.Capabilities().GetContextInject() {
		t.Error("the injected Agent does not declare context_inject")
	}

	c := aCase("CASE-INPUT")
	base, err := a.Estimate(t.Context(), c)
	if err != nil {
		t.Fatalf("Estimate on the control arm: %v", err)
	}
	grown, err := est.Estimate(t.Context(), c)
	if err != nil {
		t.Fatalf("Estimate on the treatment arm: %v", err)
	}
	assertGrewByTheAsset(t, base, grown, len(assetText))

	if grown.Calls != 1 {
		t.Errorf("Calls = %d, want 1; one Invoke settles as one provider call, and "+
			"reserving N would drift the call cap by N-1 for every Case", grown.Calls)
	}

	// WorstCase is memoized at construction, so the copy has to recompute it.
	// Inheriting the receiver's number would hand core a planning figure for a
	// prompt this Agent does not send — which is the whole failure above,
	// reached through the other method on the same interface.
	assertGrewByTheAsset(t, a.WorstCase(), est.WorstCase(), len(assetText))
}

// assertGrewByTheAsset checks that an estimate moved by the Asset's own weight.
//
// A band rather than an exact figure, because the count is an approximation by
// construction — but a NARROW band, because the three failures worth catching
// are all outside it: an Asset not counted at all (delta 0), an Asset counted in
// the request but not in the estimate (delta 0), and an Asset counted twice
// (delta 1.5x). pricing.countTokens is two bytes per token before a 1.5 safety
// margin, so the Asset's contribution is 0.75 tokens per byte for a model on the
// older tokenizer, which testModel is.
func assertGrewByTheAsset(t *testing.T, base, grown budget.Estimate, assetBytes int) {
	t.Helper()

	delta := grown.Tokens - base.Tokens
	lo := int64(assetBytes) * 70 / 100
	hi := int64(assetBytes) * 80 / 100
	if delta < lo || delta > hi {
		t.Errorf("the estimate grew by %d tokens for a %d-byte Asset, want between "+
			"%d and %d; the cost cap has to bind on the thing being measured",
			delta, assetBytes, lo, hi)
	}
	if grown.CostUSDMicros <= base.CostUSDMicros {
		t.Errorf("cost did not grow: %d then %d", base.CostUSDMicros, grown.CostUSDMicros)
	}
}

// TestWithContextRefusesAnAssetItCannotHonestlyMeasure.
//
// Every refusal here is free and lands before a single Case is sent. The
// alternative in each row is a full-price run whose numbers read as a result:
// an interval, a delta, a rank in the portfolio.
func TestWithContextRefusesAnAssetItCannotHonestlyMeasure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		tweak func(*anthropic.Options)
		asset *core.Asset
		// second injects into the Agent this one already produced.
		second bool
		wants  string
	}{
		{
			name:  "no Asset at all",
			asset: nil,
			wants: "there is no Asset to inject",
		},
		{
			name:  "an Asset with no content",
			asset: &knov1.Asset{Id: "asset-empty"},
			wants: "is empty",
		},
		{
			name:  "an Asset that is not text",
			asset: &knov1.Asset{Id: "asset-binary", Content: []byte{0xff, 0xfe, 'a'}},
			wants: "not valid UTF-8",
		},
		{
			name:  "an Asset past the ceiling this Agent plans against",
			tweak: func(o *anthropic.Options) { o.MaxPromptBytes = 1024 },
			asset: anAsset(strings.Repeat("a", 2048)),
			wants: "--max-prompt-bytes",
		},
		{
			name:   "a second Asset on an Agent that already carries one",
			asset:  anAsset(assetText),
			second: true,
			wants:  "already carries an injected Asset",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			base, _ := newInjectableAgent(t, tc.tweak)
			var target core.ContextInjector = base
			if tc.second {
				first, err := target.WithContext(anAsset("first"))
				if err != nil {
					t.Fatalf("the first injection: %v", err)
				}
				inj, ok := first.(core.ContextInjector)
				if !ok {
					t.Fatal("the injected Agent is not a ContextInjector, so a second " +
						"injection cannot be refused at all")
				}
				target = inj
			}

			got, err := target.WithContext(tc.asset)
			if err == nil {
				t.Fatalf("WithContext accepted it and returned %T", got)
			}
			if got != nil {
				t.Errorf("a refused injection still returned an Agent (%T); a caller "+
					"checking only the Agent would measure with it", got)
			}
			if !errors.Is(err, errs.ErrInvalidInput) {
				t.Errorf("error is not ErrInvalidInput, so the CLI cannot map it to an "+
					"exit code: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("the refusal does not say %q, so it names nothing the user can "+
					"act on: %v", tc.wants, err)
			}
		})
	}
}

// These tests cover WithContextSet: the Portfolio-injection half of the same
// contract WithContext already proves for one Asset. See ring0.go's
// ContextSetInjector for why order and a whole-set ceiling are the two
// properties that matter here and nowhere else.

// injectedSet is WithContextSet with the failure made fatal, for the tests
// where the injection succeeding is a precondition rather than the subject.
func injectedSet(t *testing.T, a *anthropic.Agent, assets ...*core.Asset) core.Agent {
	t.Helper()
	g, err := a.WithContextSet(assets)
	if err != nil {
		t.Fatalf("WithContextSet: %v", err)
	}
	return g
}

// TestWithContextSetJoinsEveryAssetInRankOrder.
//
// ORDER IS PART OF THE MEASUREMENT: Validate applies PortfolioEntry.rank
// before calling this, and a caller-supplied order that the adapter silently
// resorted would measure a Portfolio other than the one Validate asked about.
func TestWithContextSetJoinsEveryAssetInRankOrder(t *testing.T) {
	t.Parallel()

	a, rec := newInjectableAgent(t, func(o *anthropic.Options) { o.System = "SYSTEM-PROMPT" })

	first := &knov1.Asset{Id: "a1", Content: []byte("FIRST-ASSET-CONTENT")}
	second := &knov1.Asset{Id: "a2", Content: []byte("SECOND-ASSET-CONTENT")}

	treatment := injectedSet(t, a, first, second)
	if _, err := treatment.Invoke(t.Context(), aCase("CASE-INPUT")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	got := rec.body(t, 0)
	iFirst := strings.Index(got, "FIRST-ASSET-CONTENT")
	iSecond := strings.Index(got, "SECOND-ASSET-CONTENT")
	if iFirst == -1 || iSecond == -1 {
		t.Fatalf("the request does not carry both Assets: %s", got)
	}
	if iFirst > iSecond {
		t.Errorf("the Assets were not sent in the given order: %s", got)
	}
}

// TestWithContextSetRefusesAnEmptyOrNilSet.
//
// An Agent carrying no Assets IS the control arm: answering here would
// measure the control against itself and report the difference as zero, with
// an interval — indistinguishable from an honest null result.
func TestWithContextSetRefusesAnEmptyOrNilSet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		assets []*core.Asset
	}{
		{"nil set", nil},
		{"empty set", []*core.Asset{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, _ := newInjectableAgent(t)

			got, err := a.WithContextSet(tc.assets)
			if err == nil {
				t.Fatalf("WithContextSet accepted it and returned %T", got)
			}
			if got != nil {
				t.Errorf("a refused injection still returned an Agent (%T)", got)
			}
			if !errors.Is(err, errs.ErrInvalidInput) {
				t.Errorf("error is not ErrInvalidInput: %v", err)
			}
			if !strings.Contains(err.Error(), "control arm") {
				t.Errorf("the refusal does not explain why an empty set cannot be "+
					"measured: %v", err)
			}
		})
	}
}

// TestWithContextSetRefusesASetPastTheCeiling.
//
// The WHOLE joined set is bound against --max-prompt-bytes ONCE, before any
// Case is sent — the same free-refusal property injectable gives a single
// Asset, applied to the set as a whole because it rides as one system-field
// payload.
func TestWithContextSetRefusesASetPastTheCeiling(t *testing.T) {
	t.Parallel()

	a, rec := newInjectableAgent(t, func(o *anthropic.Options) { o.MaxPromptBytes = 4096 })

	assets := []*core.Asset{
		{Id: "a1", Content: []byte(strings.Repeat("a", 3000))},
		{Id: "a2", Content: []byte(strings.Repeat("b", 3000))},
	}

	got, err := a.WithContextSet(assets)
	if err == nil {
		t.Fatalf("WithContextSet accepted a set past the ceiling and returned %T", got)
	}
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Errorf("error is not ErrInvalidInput: %v", err)
	}
	if !strings.Contains(err.Error(), "--max-prompt-bytes") {
		t.Errorf("the refusal does not name the flag: %v", err)
	}
	if rec.calls() != 0 {
		t.Errorf("a refused set still reached the network: %d requests", rec.calls())
	}
}

// TestWithContextSetRefusesAnInvalidAssetInTheSet.
//
// Every Asset in the set is checked with the same rule WithContext applies to
// one, naming the offending Asset so the refusal is actionable rather than a
// run that fails on every Case in the sample.
func TestWithContextSetRefusesAnInvalidAssetInTheSet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		asset *core.Asset
		wants string
	}{
		{"nil element", nil, "there is no Asset at index"},
		{"empty content", &knov1.Asset{Id: "asset-empty"}, "asset-empty"},
		{"invalid UTF-8", &knov1.Asset{Id: "asset-binary", Content: []byte{0xff, 0xfe, 'a'}}, "asset-binary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, _ := newInjectableAgent(t)

			got, err := a.WithContextSet([]*core.Asset{anAsset("good content"), tc.asset})
			if err == nil {
				t.Fatalf("WithContextSet accepted an invalid Asset and returned %T", got)
			}
			if !errors.Is(err, errs.ErrInvalidInput) {
				t.Errorf("error is not ErrInvalidInput: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("the refusal does not name the offending Asset: %v, want it to "+
					"contain %q", err, tc.wants)
			}
		})
	}
}

// TestTheReceiverStillSendsNoAssetAfterSetInjection mirrors
// TestTheReceiverStillSendsNoAssetAfterInjection for the whole-set path: the
// receiver is the control arm of the same measurement WithContextSet's
// treatment arm belongs to, including the memoized WorstCase.
func TestTheReceiverStillSendsNoAssetAfterSetInjection(t *testing.T) {
	t.Parallel()

	a, rec := newInjectableAgent(t, func(o *anthropic.Options) { o.System = "SYSTEM-PROMPT" })
	c := aCase("CASE-INPUT")

	before, err := a.Estimate(t.Context(), c)
	if err != nil {
		t.Fatalf("Estimate before injection: %v", err)
	}
	beforeWorst := a.WorstCase()

	treatment := injectedSet(t, a, anAsset(assetText))
	if _, err := treatment.Invoke(t.Context(), c); err != nil {
		t.Fatalf("Invoke on the treatment arm: %v", err)
	}
	if _, err := a.Invoke(t.Context(), c); err != nil {
		t.Fatalf("Invoke on the control arm: %v", err)
	}

	if got := rec.body(t, 0); !strings.Contains(got, assetText) {
		t.Fatalf("the treatment arm sent no Asset, so this test cannot tell the two arms apart: %s", got)
	}
	if got := rec.body(t, 1); strings.Contains(got, assetText) {
		t.Errorf("the control arm carries the set, so every paired delta would be "+
			"zero and would be reported as a measurement: %s", got)
	}

	after, err := a.Estimate(t.Context(), c)
	if err != nil {
		t.Fatalf("Estimate after injection: %v", err)
	}
	if before.CostUSDMicros != after.CostUSDMicros || before.Tokens != after.Tokens {
		t.Errorf("injection moved the receiver's own estimate from %+v to %+v; the "+
			"control arm would then reserve for a prompt it does not send", before, after)
	}
	if beforeWorst.CostUSDMicros != a.WorstCase().CostUSDMicros {
		t.Errorf("injection moved the receiver's memoized WorstCase from %d to %d",
			beforeWorst.CostUSDMicros, a.WorstCase().CostUSDMicros)
	}
}

// TestTheSetInjectedAgentDeclaresContextSetInject asserts the treatment arm
// reports the capability the Value stage checks before routing a Portfolio.
func TestTheSetInjectedAgentDeclaresContextSetInject(t *testing.T) {
	t.Parallel()

	a, _ := newInjectableAgent(t)
	treatment := injectedSet(t, a, anAsset(assetText))

	capable, ok := treatment.(core.Capable)
	if !ok {
		t.Fatal("the injected Agent is not Capable")
	}
	if !capable.Capabilities().GetContextSetInject() {
		t.Error("the injected Agent does not declare context_set_inject")
	}
}

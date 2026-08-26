package openaicompat_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/knograph/kno/adapters/agent/openaicompat"
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

// anAsset is a well-formed text Asset.
func anAsset(content string) *core.Asset {
	return &knov1.Asset{Id: "asset-1", Content: []byte(content)}
}

// requestLog collects every request body a server received.
//
// Assertions are made against what the SERVER saw rather than against anything
// the adapter reports about itself: a client-side record of what it believes it
// sent cannot detect the case where something else was sent.
type requestLog struct {
	mu     sync.Mutex
	bodies []string
}

func (r *requestLog) add(b string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, b)
}

func (r *requestLog) body(t *testing.T, n int) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if n >= len(r.bodies) {
		t.Fatalf("the server saw %d requests, wanted at least %d", len(r.bodies), n+1)
	}
	return r.bodies[n]
}

// recording starts a server that answers every Case and keeps what it was sent.
func recording(t *testing.T) (*httptest.Server, *requestLog) {
	t.Helper()
	rec := &requestLog{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request body: %v", err)
			return
		}
		rec.add(string(b))
		jsonReply(w, http.StatusOK, answeredBody)
	})
	return srv, rec
}

// injected is WithContext with the failure made fatal, for the tests where the
// injection succeeding is a precondition rather than the subject.
func injected(t *testing.T, a *openaicompat.Agent, content string) core.Agent {
	t.Helper()
	g, err := a.WithContext(anAsset(content))
	if err != nil {
		t.Fatalf("WithContext: %v", err)
	}
	return g
}

// TestTheAssetIsSentImmediatelyAfterTheSystemPromptAndAheadOfTheCase.
//
// The POSITION is the assertion, not the presence. Providers cache on a prefix,
// and [system][asset] is byte-identical across every Case in an Asset's sample
// while the Case varies — so an Asset placed there is billed at the cache-read
// rate for the whole sample, and an Asset placed behind the history is billed
// fresh on every single Case. costOf prices those two at rates an order of
// magnitude apart, so this is a cost property and not a stylistic one.
func TestTheAssetIsSentImmediatelyAfterTheSystemPromptAndAheadOfTheCase(t *testing.T) {
	t.Parallel()

	srv, rec := recording(t)
	a := newAgent(t, srv, func(o *openaicompat.Options) { o.System = "SYSTEM-PROMPT" })

	c := newCase("c", "CASE-INPUT")
	c.History = []*knov1.Turn{{Role: knov1.Role_ROLE_ASSISTANT, Content: "HISTORY-TURN"}}

	if _, err := injected(t, a, "ASSET-CONTENT").Invoke(t.Context(), c); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// The exact wire order, not four Contains calls. Contains would pass on an
	// Asset appended after the Case, which is the arrangement that costs money.
	const want = `"messages":[` +
		`{"role":"system","content":"SYSTEM-PROMPT"},` +
		`{"role":"system","content":"ASSET-CONTENT"},` +
		`{"role":"assistant","content":"HISTORY-TURN"},` +
		`{"role":"user","content":"CASE-INPUT"}]`
	if got := rec.body(t, 0); !strings.Contains(got, want) {
		t.Errorf("the Asset is not sent immediately after the system prompt.\n got %s\nwant a body containing %s", got, want)
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

	srv, rec := recording(t)
	a := newAgent(t, srv, func(o *openaicompat.Options) { o.System = "SYSTEM-PROMPT" })
	c := newCase("c", "CASE-INPUT")

	before, err := a.Estimate(t.Context(), c)
	if err != nil {
		t.Fatalf("Estimate before injection: %v", err)
	}

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
}

// TestTheInjectedAgentForwardsEverythingTheGuardReads.
//
// WithContext returns core.Agent — the NARROWEST interface — so an
// implementation that wrapped rather than copied would drop Estimator and
// Capable without a compile error. Dropping Estimator is the expensive one:
// core falls back to BaselineOptions.EstCostPerCallUSDMicros, a run-scoped
// scalar that knows nothing about the Asset, and every reservation is then made
// against a constant while the prompt carries the whole Asset. core/ring0.go
// records that failure already measured once — $0.06 quoted for $12.00 of real
// exposure.
func TestTheInjectedAgentForwardsEverythingTheGuardReads(t *testing.T) {
	t.Parallel()

	srv, _ := recording(t)
	a := newAgent(t, srv)
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

	c := newCase("c", "CASE-INPUT")
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
}

// assertGrewByTheAsset checks that an estimate moved by the Asset's own weight.
//
// A band rather than an exact figure, because the count is an approximation by
// construction — but a NARROW band, because the three failures worth catching
// are all outside it: an Asset not counted at all (delta 0), an Asset counted in
// the request but not in the estimate (delta 0), and an Asset counted twice
// (delta 1.5x). pricing.countTokens is two bytes per token before a 1.5 safety
// margin, so the Asset's contribution is 0.75 tokens per byte for a model on the
// older tokenizer.
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

// TestWorstCaseStillBoundsEveryCaseTheInjectedAgentWillSend.
//
// MaxPromptBytes is a TOTAL in this adapter — checkPromptSize enforces it
// against the same four fields the estimate prices — so an injected Asset
// spends part of that budget rather than adding to it, and WorstCase does not
// move. What must hold is the property WorstCase's godoc claims: there is no
// Case this Agent will send whose Estimate exceeds it. A WorstCase that priced
// the Case allowance while forgetting the Asset would break exactly that, and
// core plans concurrency and quotes the human against this number.
func TestWorstCaseStillBoundsEveryCaseTheInjectedAgentWillSend(t *testing.T) {
	t.Parallel()

	const ceiling = 4096
	srv, _ := recording(t)
	a := newAgent(t, srv, func(o *openaicompat.Options) { o.MaxPromptBytes = ceiling })

	asset := strings.Repeat("a", 1024)
	est, ok := injected(t, a, asset).(core.Estimator)
	if !ok {
		t.Fatal("the injected Agent is not an Estimator")
	}

	if got, want := est.WorstCase(), a.WorstCase(); got.CostUSDMicros != want.CostUSDMicros {
		t.Errorf("WorstCase moved from %d to %d under injection; the ceiling is a "+
			"total, so the Asset spends part of it rather than adding to it",
			want.CostUSDMicros, got.CostUSDMicros)
	}

	// The largest Case that still fits beside the Asset. Its estimate is the
	// most this Agent can be asked to reserve for one call.
	largest, err := est.Estimate(t.Context(), newCase("c", strings.Repeat("c", ceiling-len(asset))))
	if err != nil {
		t.Fatalf("Estimate for the largest permissible Case: %v", err)
	}
	if largest.CostUSDMicros > est.WorstCase().CostUSDMicros {
		t.Errorf("a Case this Agent will send estimates at %d against a WorstCase of "+
			"%d, so planning is done against a number the run walks past",
			largest.CostUSDMicros, est.WorstCase().CostUSDMicros)
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
		tweak func(*openaicompat.Options)
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
			name:  "an Asset that leaves no room for a Case",
			tweak: func(o *openaicompat.Options) { o.MaxPromptBytes = 1024 },
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

			srv, _ := recording(t)
			var target core.ContextInjector = newAgent(t, srv, tc.tweak)
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

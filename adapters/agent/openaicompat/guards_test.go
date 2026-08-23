package openaicompat_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/adapters/agent/internal/transport"
	"github.com/knograph/kno/adapters/agent/openaicompat"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TestAPriceThatCannotPriceIsRefusedAtConstruction.
//
// The override is a second door into the pricing arithmetic, and it opens onto
// the same failure the table's presence rules exist to prevent. A Price with no
// input or output rate makes the settled cost zero for a reply that reported
// real usage — and usage_estimated stays UNSET, so nothing downstream says the
// number is missing. That is what made a dollar cap unenforceable in M1,
// reached through the override rather than through the table.
func TestAPriceThatCannotPriceIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()

	srv := serve(t, func(_ http.ResponseWriter, _ *http.Request) {})
	ref, err := agentref.Parse("openai:" + pricedModel + "@" + srv.URL)
	if err != nil {
		t.Fatalf("parsing the agent ref: %v", err)
	}
	rate := int64(1_000_000)

	for _, tc := range []struct {
		name  string
		price *knov1.Price
	}{
		{"no input rate", &knov1.Price{OutputPerMtokUsdMicros: &rate}},
		{"no output rate", &knov1.Price{InputPerMtokUsdMicros: &rate}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := openaicompat.New(openaicompat.Options{
				Ref:        ref,
				HTTPClient: srv.Client(),
				Policy:     transport.Policy{AllowInsecureHTTP: true, AllowPrivateAddress: true},
				Price:      tc.price,
			}); !errors.Is(err, errs.ErrInvalidInput) {
				t.Errorf("New error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

// TestTheProviderCannotProduceInvalidUTF8InAnErrorString.
//
// Truncating a message on a byte boundary splits a multi-byte rune, and the
// broken bytes land in a string the CLI renders, the API serializes, and the
// store persists. protojson refuses to marshal a string that is not valid
// UTF-8, so the damage surfaces as a failure to report the failure — at which
// point the original error is gone.
func TestTheProviderCannotProduceInvalidUTF8InAnErrorString(t *testing.T) {
	t.Parallel()

	// Three-byte runes, so a byte-boundary cut lands mid-sequence.
	long := strings.Repeat("日", 4096)
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, http.StatusBadRequest, `{"error":{"message":"`+long+`"}}`)
	})
	a := newAgent(t, srv)

	_, err := a.Invoke(t.Context(), newCase("c", "hi"))
	if err == nil {
		t.Fatal("a 400 produced no error")
	}
	if !utf8.ValidString(err.Error()) {
		t.Error("the error string is not valid UTF-8, so protojson cannot marshal " +
			"it and the failure cannot be reported")
	}
}

// TestThePromptCeilingIsAppliedToTheSameBytesTheEstimatePrices.
//
// promptSize and pricing.Prompt's own accounting are two implementations of one
// sum, because pricing keeps its version unexported. If they disagreed, the
// ceiling would bound a different number than the reservation prices — and
// WorstCase, which is derived from the ceiling, would stop bounding Estimate.
//
// Asserted at the exact boundary, in both directions, with every part of the
// prompt non-empty so a forgotten term is visible rather than absorbed.
func TestThePromptCeilingIsAppliedToTheSameBytesTheEstimatePrices(t *testing.T) {
	t.Parallel()

	const (
		ceiling = 512
		system  = "be terse"
		history = "earlier turn"
	)
	srv := serve(t, func(_ http.ResponseWriter, _ *http.Request) {})
	a := newAgent(t, srv, func(o *openaicompat.Options) {
		o.MaxPromptBytes = ceiling
		o.System = system
	})

	// The role NAME is counted too: it is tokens in the request, and an
	// accounting that skipped it would under-reserve by one term per turn.
	overhead := len(system) + len("user") + len(history)

	build := func(inputLen int) *core.Case {
		c := newCase("c", strings.Repeat("x", inputLen))
		c.History = []*knov1.Turn{{Role: knov1.Role_ROLE_USER, Content: history}}
		return c
	}

	if _, err := a.Estimate(t.Context(), build(ceiling-overhead)); err != nil {
		t.Errorf("a prompt of exactly %d bytes was refused: %v", ceiling, err)
	}
	if _, err := a.Estimate(t.Context(), build(ceiling-overhead+1)); err == nil {
		t.Errorf("a prompt of %d bytes was accepted against a ceiling of %d; the "+
			"ceiling and the estimate are counting different things, so WorstCase "+
			"no longer bounds Estimate", ceiling+1, ceiling)
	}
}

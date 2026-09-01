package vertex

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
)

// This file is the context-injection half of the adapter: the UPPER-BOUND
// measurement mode, where the Asset is handed to the model directly rather
// than reached through a retriever. Everything here is arranged around one
// requirement — the Agent this returns is the TREATMENT arm and the receiver
// stays usable as the CONTROL arm of the same measurement. An implementation
// that mutated the receiver would compare an Asset against itself and report
// the difference as zero, with an interval. See the anthropic adapter's
// context.go for the full reasoning; this file mirrors it with :rawPredict's
// Messages shape.

// WithContext returns an Agent that carries the Asset ahead of every Case.
//
// The receiver is unmodified, and that is the contract rather than a courtesy:
// Value measures a paired difference, and the un-injected Agent is the control
// arm of the very same measurement.
//
// The returned Agent is a COPY of the receiver — a copy of *Agent cannot
// forget to forward Estimator and Capable, and a hand-written wrapper that
// forgot would drop the budget guard back to a run-scoped scalar that knows
// nothing about the Asset.
//
// The copy shares the transport, so the two arms share one connection pool
// and one rate limiter — and the token cache, whose expiry waits at the
// exchange margin. Both are deliberate: the paired measurement is one run,
// and the token is one token.
//
// POSITION: the Asset joins the system string, immediately after the
// configured system prompt and ahead of the Case's history and input — the
// byte-identical prefix every Case in an Asset's sample shares, which is what
// lets the provider's cache price it at the far cheaper cached rate.
func (a *Agent) WithContext(asset *core.Asset) (core.Agent, error) {
	content, err := a.injectable(asset)
	if err != nil {
		return nil, err
	}
	injected := *a
	injected.asset = content
	// Recomputed, never inherited. worst is memoized at construction and the
	// Asset is the largest term in it; carrying the receiver's number over
	// would hand core a planning figure for a prompt this Agent does not send.
	injected.worst = injected.computeWorstCase()
	return &injected, nil
}

// injectable reports the Asset's content as prompt text, or refuses it.
//
// Every refusal here is free and happens before any Case is sent. The
// alternative for each is a full-price run whose numbers are wrong in a way
// that reads as a result.
func (a *Agent) injectable(asset *core.Asset) (string, error) {
	if asset == nil {
		return "", fmt.Errorf("%w: vertex: there is no Asset to inject", errs.ErrInvalidInput)
	}
	if a.asset != "" {
		// Replacing silently would measure the second Asset and attribute the
		// result to a pair, and nothing downstream carries enough information
		// to notice. Refusing keeps one Agent to one Asset, which is what a
		// per-Asset Valuation means.
		return "", errs.ErrInvalidInput.
			WithFix("build the treatment arm from the un-injected Agent, which is also the control arm").
			Wrap(fmt.Errorf("vertex: this Agent already carries an injected Asset"))
	}

	content, err := assetContent(asset)
	if err != nil {
		return "", err
	}

	// MaxPromptBytes is the one statement a caller makes about how large a
	// prompt this Agent may plan for, checked at the one moment the adapter
	// holds the dominant term in its hand and the check costs nothing.
	ceiling := a.promptCeiling()
	if int64(len(a.opts.System)+len(content)) > ceiling {
		return "", errs.ErrInvalidInput.WithFix(fmt.Sprintf(
			"raise --max-prompt-bytes above %d, or measure a smaller Asset — note "+
				"that raising it also raises the run's planned cost, so the "+
				"feasibility check runs fewer Cases at once", len(a.opts.System)+len(content),
		)).
			Wrap(fmt.Errorf("vertex: asset %s is %d bytes and the system prompt is "+
				"%d, past the --max-prompt-bytes ceiling of %d this Agent plans "+
				"against", asset.GetId(), len(content), len(a.opts.System), ceiling))
	}

	return string(content), nil
}

// assetContent reports one Asset's content as prompt bytes, or refuses it —
// the checks that apply to any Asset entering a prompt regardless of how many
// others ride alongside it: non-empty, valid UTF-8. Extracted out of
// injectable so WithContext (one Asset) and WithContextSet (a whole
// Portfolio) apply the identical rule to every Asset; a second hand-copied
// version of these checks is exactly the drift that would let one path
// accept an Asset the other refuses.
//
// The nil check and the "already carries an Asset" check stay with their
// callers rather than moving here: nil has no content to report on (it is a
// different failure, not a smaller instance of this one), and "already
// carries" is a statement about the Agent, not about the Asset in hand.
func assetContent(asset *core.Asset) ([]byte, error) {
	content := asset.GetContent()
	if len(content) == 0 {
		// An empty Asset produces a request byte-identical to the control's, so
		// every paired difference is exactly zero and the interval around it is
		// tight. That is indistinguishable in the report from "measured, and
		// inert" — the one conclusion this stage exists to reach honestly.
		return nil, errs.ErrInvalidInput.
			WithFix("check the Pool: this Asset has no content to measure").
			Wrap(fmt.Errorf("vertex: asset %s is empty", asset.GetId()))
	}
	if !utf8.Valid(content) {
		// Asset.content is bytes and the request body is JSON. encoding/json
		// replaces every invalid byte with U+FFFD, so the model would see
		// something other than the Asset and the provider would bill three
		// bytes where the estimate counted one — a reservation built from the
		// wrong prompt, silently.
		return nil, errs.ErrInvalidInput.
			WithFix("inject a text Asset; a binary one belongs in a knowledge index, not in a prompt").
			Wrap(fmt.Errorf("vertex: asset %s is not valid UTF-8", asset.GetId()))
	}
	return content, nil
}

// WithContextSet returns an Agent that carries every Asset in assets, joined
// in order, ahead of every Case.
//
// The receiver is unmodified and remains usable as the control arm — the
// same contract as WithContext, for the same reason: Value measures a paired
// difference, and mutating the receiver would make both arms carry the
// Portfolio and every delta would report as zero, with an interval.
//
// A nil or empty slice is refused rather than answered: an Agent carrying no
// Assets IS the control arm, so returning one here would measure the control
// against itself and report the difference as zero, with an interval —
// indistinguishable in the report from an honest null result.
//
// ORDER IS NOT RENEGOTIATED: assets are joined exactly as given, with "\n\n"
// between them, because ORDER IS PART OF THE MEASUREMENT — see
// core.ContextSetInjector. Every Asset is validated with the same rule
// WithContext applies to one (present, non-empty, valid UTF-8), naming the
// offending Asset's ID so the refusal is actionable.
//
// The whole joined payload is bound against --max-prompt-bytes ONCE, the same
// way a single Asset is bound in injectable — not per Asset — because the set
// rides as one system-string payload and it is the WHOLE set that must fit
// ahead of every Case.
//
// POSITION: like WithContext, the joined set joins the system string,
// immediately after the configured system prompt and ahead of the Case's
// history and input — the byte-identical prefix every holdout Case shares,
// which is what lets the provider's cache price the Portfolio's tokens once
// instead of once per Case.
func (a *Agent) WithContextSet(assets []*core.Asset) (core.Agent, error) {
	if len(assets) == 0 {
		return nil, errs.ErrInvalidInput.
			WithFix("pass at least one Asset, or measure the un-injected Agent directly as the control").
			Wrap(fmt.Errorf("vertex: no Assets to inject; an Agent carrying none " +
				"is the control arm, and injecting an empty set would measure the " +
				"control against itself and report the difference as zero, with an interval"))
	}
	if a.asset != "" {
		return nil, errs.ErrInvalidInput.
			WithFix("build the treatment arm from the un-injected Agent, which is also the control arm").
			Wrap(fmt.Errorf("vertex: this Agent already carries an injected Asset"))
	}

	contents := make([]string, len(assets))
	for i, asset := range assets {
		if asset == nil {
			return nil, fmt.Errorf("%w: vertex: there is no Asset at index %d of the set to inject",
				errs.ErrInvalidInput, i)
		}
		content, err := assetContent(asset)
		if err != nil {
			return nil, err
		}
		contents[i] = string(content)
	}
	joined := strings.Join(contents, "\n\n")

	ceiling := a.promptCeiling()
	if int64(len(a.opts.System)+len(joined)) > ceiling {
		return nil, errs.ErrInvalidInput.WithFix(fmt.Sprintf(
			"raise --max-prompt-bytes above %d, or measure a smaller Portfolio — note "+
				"that raising it also raises the run's planned cost, so the "+
				"feasibility check runs fewer Cases at once", len(a.opts.System)+len(joined),
		)).
			Wrap(fmt.Errorf("vertex: the %d-Asset Portfolio is %d bytes and the system "+
				"prompt is %d, past the --max-prompt-bytes ceiling of %d this Agent plans "+
				"against", len(assets), len(joined), len(a.opts.System), ceiling))
	}

	injected := *a
	injected.asset = joined
	// Recomputed, never inherited. See WithContext for why.
	injected.worst = injected.computeWorstCase()
	return &injected, nil
}

// systemPrefix is everything that precedes the Case: the configured system
// prompt, then the injected Asset.
//
// One function because two callers must agree byte for byte. compose builds
// what is SENT and prompt builds what is PRICED, and a divergence between them
// is a reservation for a prompt other than the one that went out — invisible
// in review and invisible in the numbers.
func (a *Agent) systemPrefix() string { return join(a.opts.System, a.asset) }

// promptCeiling reports the prompt size this Agent plans against, after the
// default and the clamp.
func (a *Agent) promptCeiling() int64 {
	n := a.opts.MaxPromptBytes
	if n <= 0 {
		n = defaultWorstCasePromptBytes
	}
	if n > maxWorstCasePromptBytes {
		// A fat-fingered ceiling must not turn a planning call into a
		// multi-gigabyte allocation. Clamped rather than refused: WorstCase has
		// no error return, and the clamp is far above any real context window.
		n = maxWorstCasePromptBytes
	}
	return n
}

var (
	_ core.ContextInjector    = (*Agent)(nil)
	_ core.ContextSetInjector = (*Agent)(nil)
)

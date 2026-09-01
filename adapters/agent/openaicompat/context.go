package openaicompat

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
)

// This file is the context-injection half of the adapter: the UPPER-BOUND
// measurement mode, where the Asset is handed to the model directly rather than
// reached through a retriever. Everything about it is arranged around one
// requirement — the Agent this returns is the TREATMENT arm and the receiver
// stays usable as the CONTROL arm of the same measurement. An implementation
// that mutated the receiver would compare an Asset against itself and report
// the difference as zero, with an interval.

// WithContext returns an Agent that carries the Asset ahead of every Case.
//
// The receiver is unmodified, and that is the contract rather than a courtesy:
// Value measures a paired difference, and the un-injected Agent is the control
// arm of the very same measurement. Mutating it in place would make both arms
// carry the Asset, every delta would be zero, and the report would present that
// as a measured finding.
//
// The returned Agent is a COPY of the receiver, which is what makes the
// forwarding requirement true by construction. WithContext's return type is
// core.Agent — the narrowest interface — so a hand-written wrapper would have
// to remember to forward Estimator and Capable, and an Estimator that is not
// forwarded drops the budget guard back to a run-scoped scalar that knows
// nothing about the Asset: the guard would reserve against a constant while the
// prompt carries the whole Asset. core/ring0.go records that failure already
// measured once, quoting $0.06 for a run whose real exposure was $12.00. A copy
// of *Agent cannot forget.
//
// POSITION: the Asset is sent as its own system message, immediately after the
// configured system prompt and ahead of the Case's history and input. The
// position is what matters, not the role — providers cache on a PREFIX, and
// [system][asset] is byte-identical across every Case in an Asset's sample
// while the Case varies, so the Asset's tokens can be served from cache for the
// whole sample. Putting it after the history would put varying bytes ahead of
// it and every Case would pay for it fresh.
func (a *Agent) WithContext(asset *core.Asset) (core.Agent, error) {
	content, err := a.injectable(asset)
	if err != nil {
		return nil, err
	}
	injected := *a
	injected.asset = content
	return &injected, nil
}

// injectable reports the Asset's content as prompt text, or refuses it.
//
// Every refusal here is free and happens before any Case is sent. The
// alternative for each is a full-price run whose numbers are wrong in a way
// that reads as a result.
func (a *Agent) injectable(asset *core.Asset) (string, error) {
	if asset == nil {
		return "", fmt.Errorf("%w: openaicompat: there is no Asset to inject", errs.ErrInvalidInput)
	}
	if a.asset != "" {
		// Replacing silently would measure the second Asset and attribute the
		// result to a pair, and nothing downstream carries enough information
		// to notice. Refusing keeps one Agent to one Asset, which is what a
		// per-Asset Valuation means.
		return "", errs.ErrInvalidInput.
			WithFix("build the treatment arm from the un-injected Agent, which is also the control arm").
			Wrap(fmt.Errorf("openaicompat: this Agent already carries an injected Asset"))
	}

	content, err := assetContent(asset)
	if err != nil {
		return "", err
	}

	// MaxPromptBytes bounds the CASE, and the Asset is charged on top — see
	// WorstCase for why the alternative biases the delta. So this is not the
	// Case's allowance being crowded; it is the bound on the Asset itself,
	// which the flag has to supply because nothing else does.
	//
	// The same rule anthropic applies, checked at the same moment: once per
	// Asset, before any Case. Per Case it would be one identical refusal for
	// every Case in the sample, and the run would end as "too many cases
	// errored", naming nothing about the Asset. Refusing per ASSET is also
	// what keeps it symmetric — a refused Asset is measured by neither arm.
	if len(a.system)+len(content) >= a.maxPrompt {
		return "", errs.ErrInvalidInput.WithFix(fmt.Sprintf(
			"raise --max-prompt-bytes above %d, or measure a smaller Asset — note "+
				"that raising it also raises the planned cost of every Case, so fewer "+
				"run concurrently under a cost cap", len(a.system)+len(content),
		)).
			Wrap(fmt.Errorf("openaicompat: asset %s is %d bytes and the system prompt is "+
				"%d, together past the --max-prompt-bytes ceiling of %d that bounds "+
				"what may ride on every Case", asset.GetId(), len(content), len(a.system), a.maxPrompt))
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
			Wrap(fmt.Errorf("openaicompat: asset %s is empty", asset.GetId()))
	}
	if !utf8.Valid(content) {
		// Asset.content is bytes and the request body is JSON. encoding/json
		// replaces every invalid byte with U+FFFD, so the model would see
		// something other than the Asset and the provider would bill three
		// bytes where the estimate counted one — a reservation built from the
		// wrong prompt, silently.
		return nil, errs.ErrInvalidInput.
			WithFix("inject a text Asset; a binary one belongs in a knowledge index, not in a prompt").
			Wrap(fmt.Errorf("openaicompat: asset %s is not valid UTF-8", asset.GetId()))
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
// rides as one system message and it is the WHOLE set that must fit ahead of
// every Case.
//
// POSITION: like WithContext, the joined set is sent as its own system
// message, immediately after the configured system prompt and ahead of the
// Case's history and input. Providers cache on a PREFIX, and
// [system][portfolio] is byte-identical across every holdout Case, so the
// Portfolio's tokens are paid for once — from cache — instead of once per
// Case in the holdout.
func (a *Agent) WithContextSet(assets []*core.Asset) (core.Agent, error) {
	if len(assets) == 0 {
		return nil, errs.ErrInvalidInput.
			WithFix("pass at least one Asset, or measure the un-injected Agent directly as the control").
			Wrap(fmt.Errorf("openaicompat: no Assets to inject; an Agent carrying none " +
				"is the control arm, and injecting an empty set would measure the " +
				"control against itself and report the difference as zero, with an interval"))
	}
	if a.asset != "" {
		return nil, errs.ErrInvalidInput.
			WithFix("build the treatment arm from the un-injected Agent, which is also the control arm").
			Wrap(fmt.Errorf("openaicompat: this Agent already carries an injected Asset"))
	}

	contents := make([]string, len(assets))
	for i, asset := range assets {
		if asset == nil {
			return nil, fmt.Errorf("%w: openaicompat: there is no Asset at index %d of the set to inject",
				errs.ErrInvalidInput, i)
		}
		content, err := assetContent(asset)
		if err != nil {
			return nil, err
		}
		contents[i] = string(content)
	}
	joined := strings.Join(contents, "\n\n")

	total := len(a.system) + len(joined)
	if total >= a.maxPrompt {
		return nil, errs.ErrInvalidInput.WithFix(fmt.Sprintf(
			"raise --max-prompt-bytes above %d, or measure a smaller Portfolio — note "+
				"that raising it also raises the planned cost of every Case, so fewer "+
				"run concurrently under a cost cap", total,
		)).
			Wrap(fmt.Errorf("openaicompat: the %d-Asset Portfolio is %d bytes and the "+
				"system prompt is %d, together past the --max-prompt-bytes ceiling of "+
				"%d that bounds what may ride on every Case", len(assets), len(joined), len(a.system), a.maxPrompt))
	}

	injected := *a
	injected.asset = joined
	return &injected, nil
}

var (
	_ core.ContextInjector    = (*Agent)(nil)
	_ core.ContextSetInjector = (*Agent)(nil)
)

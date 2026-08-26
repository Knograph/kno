package openaicompat

import (
	"fmt"
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

	content := asset.GetContent()
	if len(content) == 0 {
		// An empty Asset produces a request byte-identical to the control's, so
		// every paired difference is exactly zero and the interval around it is
		// tight. That is indistinguishable in the report from "measured, and
		// inert" — the one conclusion this stage exists to reach honestly.
		return "", errs.ErrInvalidInput.
			WithFix("check the Pool: this Asset has no content to measure").
			Wrap(fmt.Errorf("openaicompat: asset %s is empty", asset.GetId()))
	}
	if !utf8.Valid(content) {
		// Asset.content is bytes and the request body is JSON. encoding/json
		// replaces every invalid byte with U+FFFD, so the model would see
		// something other than the Asset and the provider would bill three
		// bytes where the estimate counted one — a reservation built from the
		// wrong prompt, silently.
		return "", errs.ErrInvalidInput.
			WithFix("inject a text Asset; a binary one belongs in a knowledge index, not in a prompt").
			Wrap(fmt.Errorf("openaicompat: asset %s is not valid UTF-8", asset.GetId()))
	}

	// MaxPromptBytes bounds the WHOLE prompt in this adapter — checkPromptSize
	// counts pricing.Prompt.Context along with everything else — so an injected
	// Asset spends part of the same budget the Case does. An Asset that leaves
	// no room for a Case is refused here rather than per Case, because
	// otherwise it is one identical refusal for every Case in the sample and
	// the run ends as "too many cases errored", naming nothing about the Asset.
	if len(a.system)+len(content) >= a.maxPrompt {
		return "", errs.ErrInvalidInput.WithFix(fmt.Sprintf(
			"raise --max-prompt-bytes above %d, or measure a smaller Asset — note "+
				"that raising it also raises the planned cost of every Case, so fewer "+
				"run concurrently under a cost cap", len(a.system)+len(content),
		)).
			Wrap(fmt.Errorf("openaicompat: asset %s is %d bytes and the system prompt is "+
				"%d, against a --max-prompt-bytes ceiling of %d; no Case would fit "+
				"alongside it", asset.GetId(), len(content), len(a.system), a.maxPrompt))
	}

	return string(content), nil
}

var _ core.ContextInjector = (*Agent)(nil)

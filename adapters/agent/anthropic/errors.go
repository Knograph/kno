package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/knograph/kno/adapters/agent/internal/transport"
	"github.com/knograph/kno/core/errs"
)

// Sentinels this adapter adds to the error grammar.
//
// Compare with errors.Is, never with ==: Actionable matches on Code, so a
// sentinel rebuilt from the wire still answers truthfully.
var (
	// ErrProvider means the Messages API answered with a status this run cannot
	// fix by trying again.
	//
	// Distinct from errs.ErrTransportTransient and errs.ErrRateLimited, which
	// are the two retryable shapes. Everything else that reaches a handler and
	// comes back non-2xx is terminal, and calling it retryable would spend the
	// Case's whole retry budget — and (attempts) calls against --max-calls —
	// re-asking a question already answered.
	ErrProvider = &errs.Actionable{
		Code:     "ANTHROPIC_PROVIDER_ERROR",
		Message:  "the Anthropic Messages API rejected this Case",
		Fix:      "check the model name and the request settings named above",
		ExitCode: errs.ExitError,
	}

	// ErrAuthentication means the credential was missing, rejected, or not
	// permitted to use the model.
	//
	// Its own sentinel because its fix is the only one a user can act on
	// without reading anything else, and because it is the failure that
	// otherwise errors every Case in a run and reports "too many cases errored
	// for this to be a usable baseline" — a message naming nothing about the
	// cause.
	ErrAuthentication = &errs.Actionable{
		Code:     "ANTHROPIC_AUTH",
		Message:  "Anthropic rejected the credential for this run",
		Fix:      "set ANTHROPIC_API_KEY in the environment, or bind a key for this host with --key-env host=VAR",
		ExitCode: errs.ExitError,
	}

	// ErrMalformedResponse means a 200 did not carry a Messages API response.
	//
	// TERMINAL, not transient, and the distinction is money. The provider
	// answered, so it billed; retrying pays a second time for one answer. That
	// is the opposite trade from ErrTransportTransient, which is retried
	// precisely because the request never reached a handler.
	ErrMalformedResponse = &errs.Actionable{
		Code:     "ANTHROPIC_MALFORMED_RESPONSE",
		Message:  "Anthropic returned a 200 that is not a Messages API response",
		Fix:      "if a proxy or gateway sits in front of --base-url, check that it is not rewriting response bodies",
		ExitCode: errs.ExitError,
	}
)

// retryAfterError carries a provider-requested wait alongside a sentinel.
//
// core.retryAfterOf finds it with errors.As on an anonymous
// `interface{ RetryAfter() time.Duration }`, so the METHOD is the contract and
// the type is not. Embedding *errs.Actionable keeps Error, Is, and Unwrap, so
// errors.Is(err, errs.ErrRateLimited) still answers true.
type retryAfterError struct {
	*errs.Actionable
	after time.Duration
}

// RetryAfter reports how long the provider asked us to wait.
func (e *retryAfterError) RetryAfter() time.Duration { return e.after }

// Unwrap returns the embedded Actionable rather than the Actionable's own
// cause.
//
// Without this, the PROMOTED Unwrap runs instead and returns a.wrapped, which
// jumps straight past the Actionable in the chain: errors.Is(ErrRateLimited)
// still answers true through the promoted Is, but
// errors.As(err, **errs.Actionable) answers FALSE. Measured consequence —
// core.codeOf records "AGENT_ERROR" instead of "RATE_LIMITED" for every
// retry-exhausted rate-limited Case, in the persisted Outcome, on the event
// stream, and in --json. The type that exists to carry one extra fact silently
// destroyed the fact it was wrapping.
func (e *retryAfterError) Unwrap() error { return e.Actionable }

// rateLimited builds the error a 429 becomes.
//
// The wait comes from the transport, which has already parsed Retry-After in
// both RFC 9110 forms (delta-seconds and HTTP-date) and clamped it — a hostile
// or merely misconfigured `Retry-After: 86400` cannot hang a run. Parsing it
// again here would be a second implementation of a rule that must not have two.
func rateLimited(after time.Duration, cause error) error {
	return &retryAfterError{
		Actionable: errs.ErrRateLimited.Wrap(cause),
		after:      after,
	}
}

// spendLimitCode marks the 429 that never clears.
//
// Anthropic returns HTTP 429 with error type rate_limit_error both when an
// organization is being throttled and when it has crossed its monthly spend
// cap. The second carries no Retry-After and keeps failing until the next
// calendar month. Retrying it burns the Case's whole retry budget and settles
// one call per attempt against --max-calls, for every Case in the run.
const spendLimitCode = "enforced_spend_limit_reached"

// selfSetSpendLimit is how a user's own spend limit announces itself: HTTP 400,
// type invalid_request_error, with this prefix. Terminal for the same reason.
const selfSetSpendLimit = "You have reached your specified API usage limits"

// contextTooLong is the substring in Anthropic's context-window 400.
//
// Matched on the message because the API expresses it as a generic
// invalid_request_error; there is no distinct type or code for it. The match
// only picks the FIX line, never whether the error is terminal — a 400 is
// terminal either way — so a wording change degrades the advice rather than the
// classification.
const contextTooLong = "prompt is too long"

// fromTransport maps a transport failure onto the engine's grammar.
//
// The transport classifies; it does not decide policy. This is where its
// vocabulary becomes core's, and docs/debt.md#38 exists because until an
// adapter did this, transport.ErrTransient was unreachable from core and the
// remedy it exists for was inert.
func fromTransport(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// Returned unwrapped. core tells a run that is shutting down from a
		// Case that genuinely failed by asking exactly this question, and a
		// Case cancelled during shutdown must stay unrecorded so the resume
		// picks it up rather than skipping it.
		return err

	case errors.Is(err, transport.ErrTransient):
		// A stale pooled connection, a reset before any bytes were written, a
		// body that ended early. At concurrency, any pause in a long run
		// produces a handful of these; treating them as agent errors trips the
		// 5% error-rate threshold and marks a healthy baseline unusable.
		return errs.ErrTransportTransient.Wrap(err)

	case errors.Is(err, transport.ErrRefusedDestination),
		errors.Is(err, transport.ErrKeyBinding):
		// A cross-host redirect or a credential aimed somewhere it is not bound.
		// Terminal by construction: the policy will not have changed by the next
		// attempt, and retrying a redirect refusal means re-offering the key.
		return errs.ErrInvalidInput.
			WithFix("point --base-url at the endpoint directly; Kno does not follow redirects off the host a key is bound to").
			Wrap(err)

	case errors.Is(err, transport.ErrResponseTooLarge):
		return ErrProvider.
			WithFix("this response exceeded the transport's size ceiling; check whether --base-url points at a Messages API endpoint").
			Wrap(err)

	default:
		return ErrProvider.Wrap(err)
	}
}

// fromStatus maps a non-2xx Messages API response onto the grammar.
//
// env may be nil when the body was not the provider's JSON, which is what a
// gateway between us and the API produces. The status alone is then the whole
// story, and it is still classified — a 503 from a proxy is as transient as a
// 503 from Anthropic.
func fromStatus(status int, retryAfter time.Duration, env *errorEnvelope) error {
	cause := statusCause(status, env)

	if status == http.StatusTooManyRequests {
		if env != nil && env.ErrorCode == spendLimitCode {
			return ErrProvider.
				WithFix("your organization has crossed its monthly API spend cap; raise the tier or wait for it to reset — retrying will not clear it").
				Wrap(cause)
		}
		return rateLimited(retryAfter, cause)
	}

	// Everything at 500 and above is the provider failing to answer a request
	// it accepted, including 504 timeout_error and 529 overloaded_error.
	// Retryable under core's own reservation-per-attempt discipline.
	if status >= http.StatusInternalServerError {
		return errs.ErrTransportTransient.Wrap(cause)
	}

	return terminalStatus(status, env, cause)
}

// terminalStatus picks the fix line for a 4xx.
//
// Split out because the fix is the only part that varies and a single switch
// mixing classification with advice is where the two drift apart.
func terminalStatus(status int, env *errorEnvelope, cause error) error {
	msg := ""
	if env != nil {
		msg = env.Message
	}

	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return ErrAuthentication.Wrap(cause)

	case status == http.StatusPaymentRequired:
		return ErrProvider.
			WithFix("check the payment details on the Anthropic account this key belongs to").
			Wrap(cause)

	case status == http.StatusNotFound:
		return ErrProvider.
			WithFix("check the model name against Anthropic's published model IDs, and check --base-url points at the API root rather than at /v1/messages").
			Wrap(cause)

	case status == http.StatusRequestEntityTooLarge,
		strings.Contains(msg, contextTooLong):
		// Kno's own settings are half of this: the prompt is the Case plus
		// whatever system prompt the run configured, so the fix names both.
		return ErrProvider.
			WithFix("shorten the Cases or the system prompt; the request exceeded the model's context window").
			Wrap(cause)

	case strings.HasPrefix(msg, selfSetSpendLimit):
		return ErrProvider.
			WithFix("raise or remove the spend limit set on this Anthropic organization or workspace; retrying will not clear it").
			Wrap(cause)

	default:
		return ErrProvider.Wrap(cause)
	}
}

// statusCause renders what the provider said, redacted and bounded.
//
// The provider's message is the only part of this that a human can act on, and
// it is also the only part that can carry a fragment of a Case back — so it
// goes through sanitize before it reaches an error string that is logged,
// persisted on the Outcome, and rendered in --json.
func statusCause(status int, env *errorEnvelope) error {
	if env == nil {
		return fmt.Errorf("anthropic: HTTP %d with no Messages API error body", status)
	}
	return fmt.Errorf("anthropic: HTTP %d %s: %s",
		status, sanitize(env.ErrorType), sanitize(env.Message))
}

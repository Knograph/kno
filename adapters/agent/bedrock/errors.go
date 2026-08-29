package bedrock

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/knograph/kno/adapters/agent/internal/agenterr"
	"github.com/knograph/kno/adapters/agent/internal/transport"
	"github.com/knograph/kno/core/errs"
)

// Sentinels this adapter adds to the error grammar.
//
// Compare with errors.Is, never with ==: Actionable matches on Code, so a
// sentinel rebuilt from the wire still answers truthfully.
var (
	// ErrProvider means Converse answered with a status this run cannot fix by
	// trying again.
	ErrProvider = &errs.Actionable{
		Code:     "BEDROCK_PROVIDER_ERROR",
		Message:  "Bedrock rejected this Case",
		Fix:      "check the model id and the request settings named above",
		ExitCode: errs.ExitError,
	}

	// ErrAuthentication means the credential was missing, rejected, or not
	// permitted to use the model in this region.
	//
	// Its own sentinel because its fix is the only one a user can act on
	// without reading anything else, and because it is the failure that
	// otherwise errors every Case in a run and reports "too many cases errored
	// for this to be a usable baseline".
	ErrAuthentication = &errs.Actionable{
		Code:     "BEDROCK_AUTH",
		Message:  "Bedrock rejected the credential for this run",
		Fix:      "check AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, and that the identity has access to the model in the AWS_REGION it is signed for",
		ExitCode: errs.ExitError,
	}

	// ErrMalformedResponse means a 200 did not carry a Converse response.
	//
	// TERMINAL, not transient, and the distinction is money: the provider
	// answered, so it billed; retrying pays a second time for one answer.
	ErrMalformedResponse = &errs.Actionable{
		Code:     "BEDROCK_MALFORMED_RESPONSE",
		Message:  "Bedrock returned a 200 that is not a Converse response",
		Fix:      "if a proxy or gateway sits in front of the endpoint, check that it is not rewriting response bodies",
		ExitCode: errs.ExitError,
	}
)

// ErrInvalidInput is errs' own sentinel, aliased for the reads this file makes
// of it. The construction-time refusals in auth.go use it for region problems,
// which are configuration rather than authentication.
var ErrInvalidInput = errs.ErrInvalidInput

// retryAfterError carries a provider-requested wait alongside a sentinel.
//
// Same shape as the anthropic adapter's: core.retryAfterOf finds it with
// errors.As on an anonymous `interface{ RetryAfter() time.Duration }`, so the
// METHOD is the contract and the type is not.
type retryAfterError struct {
	*errs.Actionable
	after time.Duration
}

// RetryAfter reports how long the provider asked us to wait.
func (e *retryAfterError) RetryAfter() time.Duration { return e.after }

// Unwrap returns the embedded Actionable. See the anthropic adapter's same
// method for why the promoted Unwrap must not run.
func (e *retryAfterError) Unwrap() error { return e.Actionable }

// rateLimited builds the error a 429 becomes.
//
// The wait comes from the transport, which has already parsed Retry-After in
// both RFC 9110 forms and clamped it — parsing it again here would be a second
// implementation of a rule that must not have two.
func rateLimited(after time.Duration, cause error) error {
	return &retryAfterError{
		Actionable: errs.ErrRateLimited.Wrap(cause),
		after:      after,
	}
}

// skew signals that a 403 is the clock rather than the credential.
//
// AWS returns 403 for both, and the two fixes are different: a skewed clock
// needs one retry with a fresh stamp, a rejected credential is run-fatal.
// Matched on the provider's own words because there is no structured code for
// it — and the match only picks the retry path, never the classification, so a
// wording change degrades the retry to the terminal handling instead of
// misclassifying a terminal error as retryable.
func skew(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "skew") ||
		strings.Contains(m, "x-amz-date") ||
		strings.Contains(m, "signature expired")
}

// fromTransport maps a transport failure onto the engine's grammar.
func fromTransport(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// Returned unwrapped. core tells a run that is shutting down from a
		// Case that genuinely failed by asking exactly this question.
		return err

	case errors.Is(err, transport.ErrTransient):
		return errs.ErrTransportTransient.Wrap(err)

	case errors.Is(err, transport.ErrRefusedDestination),
		errors.Is(err, transport.ErrKeyBinding):
		// A cross-host redirect, or a credential aimed somewhere it is not
		// bound. Terminal by construction, and run-fatal: config is read once,
		// so the policy that refused this request refuses every one after it.
		return agenterr.AsRunFatal(errs.ErrInvalidInput.
			WithFix("the endpoint is fixed by AWS_REGION; Kno does not follow redirects off it, and the endpoint checks cannot be bypassed").
			Wrap(err))

	case errors.Is(err, transport.ErrResponseTooLarge):
		return ErrProvider.
			WithFix("this response exceeded the transport's size ceiling; check whether the endpoint is really the Converse API").
			Wrap(err)

	default:
		return ErrProvider.Wrap(err)
	}
}

// fromStatus maps a non-2xx Converse response onto the grammar.
//
// env may be nil when the body was not the provider's JSON, which is what a
// gateway between us and the API produces. The status alone is then the whole
// story, and it is still classified.
func fromStatus(status int, retryAfter time.Duration, env *errorEnvelope) error {
	cause := statusCause(status, env)

	if status == http.StatusTooManyRequests {
		// ThrottlingException, with or without Retry-After. Bedrock's rate
		// limits clear, unlike a spend cap — nothing here is run-fatal.
		return rateLimited(retryAfter, cause)
	}

	if status >= http.StatusInternalServerError {
		return errs.ErrTransportTransient.Wrap(cause)
	}

	return terminalStatus(status, env, cause)
}

// terminalStatus picks the classification and fix line for a 4xx.
func terminalStatus(status int, env *errorEnvelope, cause error) error {
	msg := ""
	if env != nil {
		msg = env.Message
	}

	switch {
	case status == http.StatusUnauthorized:
		// Run-fatal: the credential is read once at construction and is not
		// rotated mid-run, so a rejected key rejects every Case.
		return agenterr.AsRunFatal(ErrAuthentication.Wrap(cause))

	case status == http.StatusForbidden:
		// 403 is BOTH a rejected credential and a denied model. The skew path
		// was already tried by invoke — this is the second, terminal 403, and
		// the fix line names both causes because the user cannot tell them
		// apart from here.
		return agenterr.AsRunFatal(ErrAuthentication.
			WithFix("the credential was rejected, the clock is skewed beyond the " +
				"15-minute window AWS allows (already retried once with a fresh " +
				"timestamp), or the identity has no access to this model in this " +
				"region — enable model access in the Bedrock console").
			Wrap(cause))

	case status == http.StatusNotFound:
		// Run-fatal: a model id that does not resolve will not resolve on the
		// next Case either. A typo otherwise errors every Case in the set.
		return agenterr.AsRunFatal(ErrProvider.
			WithFix("check the model id against the Bedrock console for this " +
				"region, and check that the model is available in AWS_REGION").
			Wrap(cause))

	case status == http.StatusRequestEntityTooLarge,
		strings.Contains(msg, "prompt is too long"):
		return ErrProvider.
			WithFix("shorten the Cases or the system prompt; the request exceeded the model's context window").
			Wrap(cause)

	default:
		return ErrProvider.Wrap(cause)
	}
}

// statusCause renders what the provider said, redacted and bounded.
func statusCause(status int, env *errorEnvelope) error {
	if env == nil {
		return fmt.Errorf("bedrock: HTTP %d with no Converse error body", status)
	}
	return fmt.Errorf("bedrock: HTTP %d %s: %s",
		status, sanitize(env.Type), sanitize(env.Message))
}

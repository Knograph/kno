package vertex

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
	// ErrProvider means :rawPredict answered with a status this run cannot fix
	// by trying again.
	ErrProvider = &errs.Actionable{
		Code:     "VERTEX_PROVIDER_ERROR",
		Message:  "Vertex rejected this Case",
		Fix:      "check the model id and the request settings named above",
		ExitCode: errs.ExitError,
	}

	// ErrAuthentication means the credential was missing, rejected, or not
	// permitted to use the model in this project.
	//
	// Its own sentinel because its fix is the only one a user can act on
	// without reading anything else, and because it is the failure that
	// otherwise errors every Case in a run and reports "too many cases errored
	// for this to be a usable baseline".
	ErrAuthentication = &errs.Actionable{
		Code:     "VERTEX_AUTH",
		Message:  "Vertex rejected the credential for this run",
		Fix:      "check GOOGLE_APPLICATION_CREDENTIALS (or GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_REGION), and that the service account has Vertex AI User in the project",
		ExitCode: errs.ExitError,
	}

	// ErrMalformedResponse means a 200 did not carry a Messages response.
	//
	// TERMINAL, not transient, and the distinction is money: the provider
	// answered, so it billed; retrying pays a second time for one answer.
	ErrMalformedResponse = &errs.Actionable{
		Code:     "VERTEX_MALFORMED_RESPONSE",
		Message:  "Vertex returned a 200 that is not a Messages response",
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
// Same shape as the sibling adapters': core.retryAfterOf finds it with
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
			WithFix("the endpoint is fixed by the region; Kno does not follow redirects off it, and the endpoint checks cannot be bypassed").
			Wrap(err))

	case errors.Is(err, transport.ErrResponseTooLarge):
		return ErrProvider.
			WithFix("this response exceeded the transport's size ceiling; check whether the endpoint is really the :rawPredict API").
			Wrap(err)

	default:
		return ErrProvider.Wrap(err)
	}
}

// fromStatus maps a non-2xx :rawPredict response onto the grammar.
//
// env may be nil when the body was not the provider's JSON, which is what a
// gateway between us and the API produces. The status alone is then the whole
// story, and it is still classified.
func fromStatus(status int, retryAfter time.Duration, env *errorEnvelope) error {
	cause := statusCause(status, env)

	if status == http.StatusTooManyRequests {
		// Vertex's rate limits clear, unlike a spend cap — nothing here is
		// run-fatal.
		return rateLimited(retryAfter, cause)
	}

	if status >= http.StatusInternalServerError {
		return errs.ErrTransportTransient.Wrap(cause)
	}

	return terminalStatus(status, env, cause)
}

// terminalStatus picks the classification and fix line for a 4xx.
func terminalStatus(status int, env *errorEnvelope, cause error) error {
	statusText := ""
	if env != nil {
		statusText = env.Error.Status
	}
	msg := ""
	if env != nil {
		msg = env.Error.Message
	}

	switch {
	case status == http.StatusUnauthorized:
		// Run-fatal: the credential is read once at construction and is not
		// rotated mid-run, so a rejected identity rejects every Case.
		return agenterr.AsRunFatal(ErrAuthentication.Wrap(cause))

	case status == http.StatusForbidden:
		// 403 is BOTH a rejected token and a denied model. The fix line names
		// both causes because the user cannot tell them apart from here.
		return agenterr.AsRunFatal(ErrAuthentication.
			WithFix("the access token was rejected, or the service account has no " +
				"permission on the model in this project — grant Vertex AI User " +
				"to the service account, or check the project id in the endpoint path").
			Wrap(cause))

	case status == http.StatusNotFound:
		// NOT_FOUND on publishers/anthropic/models/* has a well-known cause
		// that is NOT a phantom endpoint bug: Model Garden's terms for Claude
		// have not been accepted in this project. Named outright, because the
		// alternative is a user chasing the endpoint.
		if strings.Contains(statusText, "NOT_FOUND") {
			return agenterr.AsRunFatal(ErrProvider.
				WithFix("accept the Model Garden terms for Anthropic models in this " +
					"project; a 404 on publishers/anthropic/models/* is the " +
					"endpoint's way of saying the terms are not accepted, not " +
					"that the endpoint moved").
				Wrap(cause))
		}
		return agenterr.AsRunFatal(ErrProvider.
			WithFix("check the model id against the Model Garden catalog for this " +
				"project and region, and that the region hosts the model").
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
	if env == nil || env.Error == nil {
		return fmt.Errorf("vertex: HTTP %d with no error body", status)
	}
	return fmt.Errorf("vertex: HTTP %d %s: %s",
		status, sanitize(env.Error.Status), sanitize(env.Error.Message))
}

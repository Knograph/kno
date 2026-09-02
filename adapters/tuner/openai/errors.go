package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/knograph/kno/core/errs"
)

// errorEnvelope is OpenAI's published error body shape: {"error":
// {"message", "type", "param", "code"}}. (verify) per this package's own
// PROVENANCE WARNING.
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// fromTransport maps a local transport failure onto the engine's grammar —
// see client.go's doc for why this package classifies transport failures
// itself rather than reusing adapters/agent/internal/transport's.
func fromTransport(err error) error {
	switch {
	case errors.Is(err, errRedirectRefused):
		return errs.ErrInvalidInput.
			WithFix("point --base-url at the endpoint directly; Kno does not follow redirects off the host a key is bound to").
			Wrap(err)
	case errors.Is(err, errTransientTransport):
		return errs.ErrTransportTransient.Wrap(err)
	default:
		return ErrProvider.Wrap(err)
	}
}

// fromStatus maps a non-2xx response onto the grammar.
func fromStatus(status int, body []byte) error {
	var env *errorEnvelope
	var e errorEnvelope
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		env = &e
	}
	cause := statusCause(status, env)

	switch {
	case status == http.StatusTooManyRequests:
		return errs.ErrRateLimited.Wrap(cause)
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		// Terminal: the credential is read once at construction. A rejected
		// key rejects every job in the run, and ErrAuthentication names the
		// env var and states plainly that no file, profile, or metadata
		// service is read.
		return ErrAuthentication.Wrap(cause)
	case status >= http.StatusInternalServerError:
		return errs.ErrTransportTransient.Wrap(cause)
	default:
		return ErrProvider.Wrap(cause)
	}
}

// statusCause renders what the provider said. No headers, ever — see the
// fixture discipline (adapters/agent/anthropic/testdata/fixtures/README.md
// and this package's own testdata/fixtures/README.md) this package's tests
// carry forward.
func statusCause(status int, env *errorEnvelope) error {
	if env == nil {
		return fmt.Errorf("openai: HTTP %d with no recognizable error body", status)
	}
	return fmt.Errorf("openai: HTTP %d %s: %s", status, env.Error.Type, env.Error.Message)
}

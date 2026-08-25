package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/knograph/kno/adapters/agent/internal/agenterr"
	"github.com/knograph/kno/adapters/agent/internal/transport"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// errMalformedBody means a 200 whose body is not a Chat Completions reply.
//
// Terminal, not transient. A body that does not parse will not parse on the
// next attempt either, and retrying it spends the Case's price again for the
// same answer. A body that was CUT SHORT is a different thing and never
// reaches here: the transport's read fails and classifies it as transient
// before this package sees anything.
var errMalformedBody = errors.New("openaicompat: the provider's reply is not a chat completion")

// rateLimitedError is errs.ErrRateLimited carrying the provider's own
// Retry-After.
//
// It WRAPS the Actionable rather than embedding it, and the difference is not
// stylistic. With an embedded *errs.Actionable, Unwrap is promoted and returns
// the Actionable's own cause, so errors.As(err, **errs.Actionable) skips past
// the sentinel entirely — errs.ExitCodeOf would report an unclassified failure
// and the CLI would exit 1 for a run that stopped on a rate limit. Wrapping
// puts the Actionable itself in the chain, so:
//
//   - errors.Is(err, errs.ErrRateLimited) matches through Unwrap, which is
//     what core.retryable reads;
//   - errors.As finds *errs.Actionable, which is what errs.ExitCodeOf reads;
//   - errors.As finds this type's RetryAfter method, which is what
//     core.retryAfterOf reads to pace the retry at the PROVIDER's rate rather
//     than at our doubling backoff.
//
// See docs/debt.md#39: the transport correctly returns a 429 as an ordinary
// response with a parsed, clamped Retry-After. Turning that into a
// classification core can act on is the adapter's job, because the sentinel
// lives in core/errs and the transport may not import it.
type rateLimitedError struct {
	err   *errs.Actionable
	after time.Duration
}

// Error renders the wrapped grammar unchanged.
func (e *rateLimitedError) Error() string { return e.err.Error() }

// Unwrap exposes the sentinel, so errors.Is and errors.As both reach it.
func (e *rateLimitedError) Unwrap() error { return e.err }

// RetryAfter reports how long the provider asked us to wait.
//
// Already clamped by the transport (transport.RetryAfter), so a hostile or
// merely misconfigured `Retry-After: 86400` cannot hang a run for a day.
func (e *rateLimitedError) RetryAfter() time.Duration { return e.after }

// classify turns a transport-level failure into something core can act on.
//
// The transport CLASSIFIES and this TRANSLATES. transport.ErrTransient lives in
// an internal package core cannot import — nor should it, under prime directive
// 3 — so without this step the sentinel is unreachable from the one place that
// decides whether to retry, and a stale pooled connection becomes a terminally
// errored Case labelled AGENT_ERROR. At concurrency 8 against a provider with a
// 60s idle timeout, any pause in a long run errors a handful of Cases and 5% of
// them trips DefaultMaxErrorRate, marking a good baseline unusable. That is the
// adapter half of docs/debt.md#38.
func classify(err error) error {
	if err == nil {
		return nil
	}
	// Cancellation and deadlines are the caller's, not the provider's. They
	// must pass through unchanged so core still sees context.Canceled and
	// stops resumably instead of counting an errored Case.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// KNOWN GAP, and it is not this package's to close. errs.ErrTransportTransient
	// promises "no evidence the provider processed it", and transport.ErrTransient
	// is not always that: transport/client.go wraps it around a body-read failure
	// that happened AFTER a 200, i.e. after the provider generated and billed.
	// core retries it, so real spend exceeds recorded spend by
	// (attempts-1) x per-call cost.
	//
	// The two cases are indistinguishable here — Client.Do returns one error and
	// no response either way — so classifying them apart requires a second
	// sentinel from the transport, which is a separate workstream's package.
	// Deliberately NOT worked around by matching on the transport's error prose:
	// that coupling would break silently the first time a message is reworded,
	// and it would hide the gap rather than record it. See docs/debt.md#43.
	if errors.Is(err, transport.ErrTransient) {
		return errs.ErrTransportTransient.Wrap(err)
	}
	// A refused destination, a key bound elsewhere, a body past the ceiling:
	// all decisions that will be made identically on every attempt. Retrying
	// them spends nothing but the run's retry budget, and the user needs the
	// message, not three copies of it.
	//
	// Destination and key-binding refusals are run-fatal on top of that: config
	// is read once, so the policy that refused this request refuses every one
	// after it. A response past the size ceiling is NOT — that is a property of
	// one answer, and a single fat response must not kill the run.
	if errors.Is(err, transport.ErrRefusedDestination) ||
		errors.Is(err, transport.ErrKeyBinding) {
		// Wrapped in an Actionable before it is marked, matching what
		// anthropic does for the identical condition. The bare transport error
		// was harmless while it was one Case's failure; as the RUN-ending
		// error it reaches codeOf, which records "AGENT_ERROR", and
		// ExitCodeOf, which returns the unclassified default — so the user
		// gets no fix line for a misconfiguration that has an obvious one.
		return agenterr.AsRunFatal(errs.ErrInvalidInput.
			WithFix("point --base-url at the endpoint directly, and bind a key " +
				"for that host with --key-env host=VAR; Kno does not follow " +
				"redirects off the host a key is bound to").
			Wrap(err))
	}
	return err
}

// errorFor turns a non-2xx reply into the error core will see.
//
// The three groups are separated by what a SECOND attempt would do, not by
// what the status looks like:
//
//   - 429 is the provider deliberately refusing, and it says when to come
//     back. Retryable, and the wait is its own.
//   - 5xx and 408 are the provider failing to answer. Retryable; errs
//     ErrTransportTransient's own godoc names 5xx.
//   - everything else is a decision. An expired key, a context length
//     exceeded, a model that does not exist: the same request will be refused
//     the same way, so retrying it only multiplies whatever a failed request
//     costs (see docs/debt.md#43).
//
// The branches that can carry a charge are passed through a.billed. OpenAI
// attaches no usage block to an error response — every recorded fixture
// confirms it — but several compatible gateways do, and a charge the provider
// reported is a charge whether or not the call succeeded.
func (a *Agent) errorFor(resp *transport.Response) error {
	we := decodeError(resp.Body)
	model, host := a.model, a.host

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		// Not routed through billed, deliberately rather than by omission: a
		// request refused before generation produced no tokens, and a usage
		// block on a 429 would be a gateway reporting somebody else's call.
		return &rateLimitedError{
			after: resp.RetryAfter,
			err: errs.ErrRateLimited.
				WithFix(fmt.Sprintf("lower --concurrency, or wait %s for %s's "+
					"rate-limit window to reopen", resp.RetryAfter.Round(time.Second), host)).
				Wrap(fmt.Errorf("%s refused a request for %s: %s",
					host, model, describe(resp.StatusCode, we))),
		}

	case resp.StatusCode == http.StatusRequestTimeout:
		// Same sentinel as a 5xx — both are "the provider failed to answer",
		// both retryable — but NOT the same reason. core could only classify
		// from the sentinel, so a timeout was reported as
		// RETRY_REASON_PROVIDER_UNAVAILABLE, whose schema definition is "the
		// provider returned a 5xx". RETRY_REASON_TIMEOUT existed and nothing
		// ever emitted it. See docs/debt.md#53.
		//
		// The reason rides on the error because the status code is knowledge
		// only this layer has, and a sentinel cannot carry it up.
		return agenterr.WithRetryReason(
			a.billed(errs.ErrTransportTransient.Wrap(fmt.Errorf(
				"%s did not answer for %s in time: %s",
				host, model, describe(resp.StatusCode, we))),
				decodeUsage(resp.Body)),
			knov1.RetryReason_RETRY_REASON_TIMEOUT)

	case resp.StatusCode >= 500:
		return a.billed(errs.ErrTransportTransient.Wrap(fmt.Errorf(
			"%s did not answer for %s: %s", host, model, describe(resp.StatusCode, we))),
			decodeUsage(resp.Body))

	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		// The fix names the variable that was actually consulted for THIS
		// host. A generic "check your API key" is unhelpful precisely when the
		// per-host binding is what went wrong — the user set OPENAI_API_KEY and
		// pointed the run at a different provider, and the message must say
		// which host had no key rather than implying the key is wrong.
		// Run-fatal: the credential is resolved once at construction, so a
		// rejected key rejects every remaining Case. See docs/debt.md#47.
		return agenterr.AsRunFatal(
			errs.ErrInvalidInput.WithFix(credentialFix(host, a.keyEnv)).
				Wrap(fmt.Errorf("%s rejected the credential: %s",
					host, describe(resp.StatusCode, we))))

	case isContextLength(we):
		// A distinct message because the fix is distinct and neither of the
		// obvious guesses is right: the model is fine and the key is fine, the
		// Case simply does not fit. Naming --max-output-tokens matters because
		// it is the term the user controls AND the output term of every
		// reservation, so lowering it is not free.
		return errs.ErrInvalidInput.
			WithFix("shorten the Case, or lower --max-output-tokens — the output " +
				"ceiling counts against the model's context window as well as " +
				"against every reservation").
			Wrap(fmt.Errorf("%s reports the prompt plus the output ceiling exceeds "+
				"%s's context window: %s", host, model, describe(resp.StatusCode, we)))

	case resp.StatusCode == http.StatusNotFound:
		// Run-fatal: neither the model name nor the base URL changes mid-run,
		// so a 404 on the first Case is a 404 on all of them.
		return agenterr.AsRunFatal(errs.ErrInvalidInput.
			WithFix(fmt.Sprintf("check the model name and the base URL; %s has no "+
				"chat-completions endpoint at this path", host)).
			Wrap(fmt.Errorf("%s returned 404 for %s: %s",
				host, model, describe(resp.StatusCode, we))))

	default:
		return a.billed(errs.ErrInvalidInput.
			WithFix("check the model name, the base URL, and the generation "+
				"parameters; the provider refused this request as written").
			Wrap(fmt.Errorf("%s refused a request for %s: %s",
				host, model, describe(resp.StatusCode, we))),
			decodeUsage(resp.Body))
	}
}

// credentialFix names the environment variable bound to this host, or says
// plainly that none is.
func credentialFix(host, keyEnv string) string {
	if keyEnv == "" {
		return fmt.Sprintf("no credential is bound to %s — bind one with "+
			"--key-env %s=YOUR_VAR_NAME (the NAME of an environment variable; "+
			"the key itself must never appear in a flag)", host, host)
	}
	return fmt.Sprintf("check %s, which is the variable bound to %s; keys come "+
		"from the environment, never from a flag or kno.yaml", keyEnv, host)
}

// isContextLength recognizes the one 4xx whose fix is neither the key nor the
// model name.
//
// Matched on the provider's own code first and on the message only as a
// fallback, because the code is stable and the prose is not.
func isContextLength(we *wireError) bool {
	if we == nil {
		return false
	}
	if we.code() == "context_length_exceeded" || we.code() == "string_above_max_length" {
		return true
	}
	m := strings.ToLower(we.Message)
	return strings.Contains(m, "context length") ||
		strings.Contains(m, "context_length") ||
		strings.Contains(m, "maximum context")
}

// describe renders a provider error for a human, bounded.
//
// Bounded because the message is the provider echoing our own request back:
// "Invalid value for 'messages[1].content'" arrives with the content attached
// on some gateways, which would put a Case's text into an error string that is
// rendered in the terminal and persisted with the outcome. The status is always
// present, so truncating loses classification information — not diagnosis.
// EVERY provider-controlled field goes through flatten, not just the one that
// looks like prose. `code` is nominally a short stable identifier, but nothing
// stops a provider putting 8 KiB with an embedded newline in it — and a
// demonstration did exactly that, reaching the error grammar with a forged
// "fix:" line telling the user to pipe a URL into a shell. Sanitizing the
// obvious field and trusting its neighbour is not sanitizing.
func describe(status int, we *wireError) string {
	if we == nil || we.Message == "" {
		return fmt.Sprintf("HTTP %d, with no error object in the body", status)
	}
	msg := flatten(we.Message, maxProviderMessage)

	if c := flatten(we.code(), maxProviderCode); c != "" {
		return fmt.Sprintf("HTTP %d %s: %s", status, c, msg)
	}
	return fmt.Sprintf("HTTP %d: %s", status, msg)
}

// flatten bounds a provider-controlled string and strips the characters that
// would let it forge structure in the error grammar.
//
// Newlines go because the CLI's grammar is line-oriented: a provider that
// controls line breaks controls what the "fix:" line appears to say. Carriage
// returns go with them, since a bare \r rewrites the line already printed on a
// terminal. Truncation is rune-safe (see truncate).
func flatten(s string, n int) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(truncate(s, n))
}

// maxProviderMessage bounds how much provider prose is quoted back.
const maxProviderMessage = 512

// maxProviderCode bounds the provider's error code.
//
// Far shorter than the message because a code is an identifier —
// "context_length_exceeded" is 23 bytes and the longest real one is nowhere
// near this. Anything longer is not a code, and quoting it back at length only
// buries the message that follows.
const maxProviderCode = 64

// truncate cuts s to at most n bytes without splitting a rune.
//
// Slicing on a byte boundary is the obvious version and it is wrong: a message
// in any non-Latin script would be cut mid-sequence, and the invalid UTF-8 goes
// into an error string that is rendered in a terminal, serialized by the API,
// and persisted with the outcome. protojson refuses to marshal a string that is
// not valid UTF-8, so the damage surfaces as a failure to report the failure.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Walk back to the start of the rune that straddles the cut.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// billedError is a failed call that the provider nevertheless charged for.
//
// It exists because a failure is not always free. A gateway that answers
// `200 {"error":{…},"usage":{"prompt_tokens":50000,…}}` generated tokens, billed
// them, and then told us the call went wrong — the usage block is sitting in
// the payload we already parsed. Dropping it records a paid call as costing
// nothing.
//
// That matters more than it looks. core.invokeOnce settles ANY Invoke error as
// budget.Spend{Calls: 1} with zero dollars, so under --max-cost-usd this is
// spend the cap cannot see: real spend exceeds recorded spend, and a resumed
// run restores the understated figure and spends the difference again.
//
// The shape deliberately mirrors rateLimitedError: wrap the cause rather than
// embed it, so errors.Is and errors.As still reach whatever classification the
// caller put underneath, and expose ONE reader method that core can find with
// an anonymous interface assertion — exactly how core.retryAfterOf already
// reads Retry-After off an adapter error. See docs/debt.md#43.
//
// core does not read it yet; the live-test spend path in this package does, so
// the figure is settled against a real budget.Guard today rather than waiting.
type billedError struct {
	err    error
	micros int64
}

// Error renders the wrapped cause unchanged.
func (e *billedError) Error() string { return e.err.Error() }

// Unwrap exposes the cause, so classification survives.
func (e *billedError) Unwrap() error { return e.err }

// BilledCostUSDMicros reports what the provider charged for this failed call.
func (e *billedError) BilledCostUSDMicros() int64 { return e.micros }

// BilledCostOf reports what a provider charged for a call that failed, when the
// provider said so.
//
// The second return distinguishes "the provider reported a charge of zero" from
// "the provider reported nothing", which are not the same claim: the first is
// evidence, the second is absence. A caller settling the second as zero would be
// asserting something no provider told it.
//
// Exported because the settlement decision belongs to whoever holds the budget
// reservation, not to the adapter. Today that is this package's live-test spend
// path; when core learns to settle a failed attempt it reads the same value
// through its own interface assertion.
func BilledCostOf(err error) (int64, bool) {
	var b interface{ BilledCostUSDMicros() int64 }
	if errors.As(err, &b) {
		return b.BilledCostUSDMicros(), true
	}
	return 0, false
}

// billed attaches an observed charge to err, if the provider reported one.
//
// Returns err untouched when there is no usable usage block, so an error that
// carries no evidence of a charge is distinguishable from one that carries
// evidence of a zero charge.
func (a *Agent) billed(err error, u *chatUsage) error {
	usable := usableUsage(u)
	if usable == nil || a.price == nil {
		return err
	}
	// Priced through the same function the success path uses, so a failed call
	// and a successful one with identical usage settle at the identical figure.
	cost := costOf(a.price, &knov1.Response{
		PromptTokens:     usable.PromptTokens,
		CompletionTokens: usable.CompletionTokens,
		CachedTokens:     cachedOf(usable),
	})
	if cost <= 0 {
		return err
	}
	return &billedError{err: err, micros: cost}
}

package transport

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxRetryAfter clamps how long a provider may tell us to wait.
//
// Retry-After is attacker-influenced input in the compromised-endpoint case and
// merely wrong in the ordinary one — a misconfigured gateway returning
// `Retry-After: 86400` would otherwise hang a run for a day. Kno's own retry
// budget decides whether to wait at all; this bounds what a single wait can be.
const maxRetryAfter = 60 * time.Second

// RetryAfter extracts the wait a response asks for.
//
// Both forms in RFC 9110 are honored: delta-seconds, and an HTTP-date. Parsing
// only the first is the common shortcut and it silently reads "Retry-After: Wed,
// 21 Oct 2026 07:28:00 GMT" as no delay at all, which turns a polite backoff
// into a hot loop against a provider that just asked us to stop.
//
// now is injected so the date branch is testable without sleeping.
func RetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}

	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return clampWait(time.Duration(secs) * time.Second), true
	}

	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	d := t.Sub(now)
	if d <= 0 {
		// A date in the past means "you may retry now", not "wait forever".
		return 0, true
	}
	return clampWait(d), true
}

func clampWait(d time.Duration) time.Duration {
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// Limiter paces requests per host.
//
// Per host rather than per process: a run against two providers should not have
// one provider's 429 slow down the other, and rate limits are enforced by the
// host anyway.
//
// Deliberately NOT a token bucket with a configured rate. Kno does not know a
// provider's limits, and guessing them either wastes throughput or ignores the
// limit entirely. Instead this is a gate: it is open until a provider says
// otherwise, and a Retry-After closes it for exactly as long as asked. The
// provider is the authority on its own limits.
type Limiter struct {
	mu sync.Mutex

	// until is when each host may be used again.
	until map[string]time.Time

	// now is injected for tests.
	now func() time.Time
}

// NewLimiter returns a Limiter that is open for every host.
func NewLimiter() *Limiter {
	return &Limiter{until: map[string]time.Time{}, now: time.Now}
}

// Wait blocks until host may be used, or ctx ends.
//
// Returns how long it waited, so a caller can emit the "waiting on a rate
// limit" signal rather than looking frozen — a run backing off and a run hung
// are indistinguishable from the outside, and only one of them is a problem.
func (l *Limiter) Wait(ctx context.Context, host string) (time.Duration, error) {
	l.mu.Lock()
	until, ok := l.until[normalizeHost(host)]
	now := l.now()
	l.mu.Unlock()

	if !ok || !until.After(now) {
		return 0, nil
	}

	d := until.Sub(now)
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-timer.C:
		return d, nil
	}
}

// Close holds host until the given duration has passed.
//
// Extends an existing hold rather than replacing it, so two workers that both
// receive a 429 cannot shorten each other's wait by racing.
func (l *Limiter) Close(host string, d time.Duration) {
	if d <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	h := normalizeHost(host)
	until := l.now().Add(d)
	if prev, ok := l.until[h]; ok && prev.After(until) {
		return
	}
	l.until[h] = until
}

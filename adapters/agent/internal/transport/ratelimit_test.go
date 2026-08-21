package transport_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/knograph/kno/adapters/agent/internal/transport"
)

// TestRetryAfterHonorsBothForms.
//
// RFC 9110 allows delta-seconds AND an HTTP-date. Parsing only the first is the
// common shortcut, and it reads "Retry-After: Wed, 21 Oct 2026 07:28:00 GMT" as
// no delay at all — turning a polite backoff into a hot loop against a provider
// that just asked us to stop.
func TestRetryAfterHonorsBothForms(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{"delta-seconds", "30", 30 * time.Second, true},
		{"an HTTP-date in the future", "Wed, 21 Oct 2026 07:00:20 GMT", 20 * time.Second, true},
		{"an HTTP-date in the past means retry now", "Wed, 21 Oct 2026 06:00:00 GMT", 0, true},
		{"absent", "", 0, false},
		{"unparseable", "soon", 0, false},
		{"negative seconds", "-5", 0, false},
		// A misconfigured gateway saying "come back tomorrow" would otherwise
		// hang a run for a day; on a compromised endpoint it is a denial of
		// service the user paid to be subjected to.
		{"a hostile delay is clamped", "86400", 60 * time.Second, true},
		{"a hostile date is clamped", "Thu, 22 Oct 2026 07:00:00 GMT", 60 * time.Second, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := http.Header{}
			if tc.value != "" {
				h.Set("Retry-After", tc.value)
			}
			got, ok := transport.RetryAfter(h, now)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("wait = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLimiterIsOpenUntilAProviderSaysOtherwise.
//
// Kno does not know a provider's limits, and guessing them either wastes
// throughput or ignores the limit. The gate stays open until a 429 closes it.
func TestLimiterIsOpenUntilAProviderSaysOtherwise(t *testing.T) {
	t.Parallel()

	l := transport.NewLimiter()
	waited, err := l.Wait(t.Context(), "api.example.com")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if waited != 0 {
		t.Errorf("waited %v before any provider asked us to", waited)
	}
}

// TestLimiterExtendsRatherThanShortensAHold.
//
// Two workers both receiving a 429 must not shorten each other's wait by
// racing: the later, longer hold wins.
func TestLimiterExtendsRatherThanShortensAHold(t *testing.T) {
	t.Parallel()

	l := transport.NewLimiter()
	l.Close("api.example.com", time.Hour)
	l.Close("api.example.com", time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if _, err := l.Wait(ctx, "api.example.com"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v; a short hold overwrote a long one, so a worker "+
			"resumed while the provider was still refusing", err)
	}
}

// TestLimiterIsPerHost: a run against two providers must not have one
// provider's 429 slow down the other.
func TestLimiterIsPerHost(t *testing.T) {
	t.Parallel()

	l := transport.NewLimiter()
	l.Close("slow.example.com", time.Hour)

	waited, err := l.Wait(t.Context(), "fast.example.com")
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if waited != 0 {
		t.Errorf("a hold on one host delayed another by %v", waited)
	}
}

// TestLimiterHonorsCancellation: a Ctrl-C during a rate-limit wait ends the
// wait rather than serving it out.
func TestLimiterHonorsCancellation(t *testing.T) {
	t.Parallel()

	l := transport.NewLimiter()
	l.Close("api.example.com", time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	start := time.Now()
	if _, err := l.Wait(ctx, "api.example.com"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("waited %v after cancellation", d)
	}
}

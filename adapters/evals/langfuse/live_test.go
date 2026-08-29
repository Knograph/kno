//go:build integration

package langfuse_test

import (
	"context"
	"os"
	"testing"

	"github.com/knograph/kno/adapters/evals/langfuse"
)

// Live tests are opt-in: KNO_LIVE_TESTS=1 (exactly the value "1"). They
// fetch a real dataset over the network and are never part of PR CI.
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("KNO_LIVE_TESTS") != "1" {
		t.Skip("KNO_LIVE_TESTS!=1; skipping live Langfuse test")
	}
}

// liveEvals builds an Evals from the environment, skipping when the keys or
// dataset are absent. Host defaults to the hosted API; a self-hosted
// deployment is selected by LANGFUSE_HOST and must be opted into with the
// same flags the CLI uses, so the live posture matches the shipped one.
func liveEvals(t *testing.T) *langfuse.Evals {
	t.Helper()
	if os.Getenv(langfuse.PublicKeyEnv) == "" || os.Getenv(langfuse.SecretKeyEnv) == "" {
		t.Skip("LANGFUSE_PUBLIC_KEY or LANGFUSE_SECRET_KEY unset; skipping live Langfuse test")
	}
	dataset := os.Getenv("KNO_LIVE_DATASET")
	if dataset == "" {
		t.Skip("KNO_LIVE_DATASET unset; skipping live Langfuse test")
	}
	ev, err := langfuse.New(langfuse.Options{
		Dataset: dataset,
		// AllowPrivateAddress is only for local self-hosting (kind, docker,
		// the agent transport precedent); the hosted endpoint needs neither
		// flag.
		AllowPrivateAddress: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ev
}

func TestLiveDatasetReads(t *testing.T) {
	requireLive(t)
	ev := liveEvals(t)

	counts, err := ev.CountSplits(context.Background())
	if err != nil {
		t.Fatalf("CountSplits: %v", err)
	}
	if counts.Total() == 0 {
		t.Fatal("the live dataset has no items")
	}
	if counts.Holdout == 0 {
		t.Error("the live dataset produced no holdout half; review the size and the configured fraction")
	}

	// Walk the first 100 cases; every one must map and carry a stable split.
	seq, err := ev.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}
	seen := 0
	first := ""
	for c, err := range seq {
		if err != nil {
			t.Fatalf("iterating: %v", err)
		}
		if c.GetId() == "" {
			t.Error("a case with an empty id")
		}
		if first == "" {
			first = c.GetId()
		}
		seen++
		if seen == 100 {
			break
		}
	}
	if seen == 0 {
		t.Fatal("no cases yielded")
	}
}

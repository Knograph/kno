package cli

import (
	"context"
	"strings"

	"github.com/knograph/kno/adapters/evals/jsonl"
	"github.com/knograph/kno/adapters/evals/langfuse"
	"github.com/knograph/kno/adapters/evals/langsmith"
	"github.com/knograph/kno/adapters/evals/split"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
)

// evalSource is what the baseline and value commands need from an eval set:
// the iterator contract plus the split counts and the content fingerprint.
// Both adapters satisfy it; the renderers consume the shared split counts
// type either way.
type evalSource interface {
	core.Evals
	CountSplits(ctx context.Context) (split.Counts, error)
	ContentHash(ctx context.Context) (string, error)
}

// The --evals grammar: a bare path is a JSONL file; the prefixes select the
// dataset adapters. A dataset name is exactly what follows a prefix, so a
// dataset literally named "langsmith:..." or "langfuse:..." cannot be
// addressed — a name that looks like the grammar is the grammar.
const (
	evalsLangsmithPrefix = "langsmith:"
	evalsLangfusePrefix  = "langfuse:"
)

// resolveEvals turns the --evals flag into an eval source.
//
// The bare path is the jsonl adapter, unchanged. The langsmith: prefix
// selects the LangSmith adapter, which reads its key from
// LANGSMITH_API_KEY (never from a flag — a credential in a shell history or
// process listing is a credential lost) and its endpoint from
// LANGSMITH_ENDPOINT. The langfuse: prefix selects the Langfuse adapter,
// which reads its credential pair from LANGFUSE_PUBLIC_KEY and
// LANGFUSE_SECRET_KEY and its host from LANGFUSE_HOST. The endpoint
// security opt-outs mirror the agent transport's --allow-insecure-base-url
// and --allow-private-address, so a self-hosted deployment is reachable
// with the same two flags a self-hosted model endpoint needs.
//
// The error fix is per source, because the failure modes are: for JSONL, the
// path is wrong; for LangSmith or Langfuse, the keys, the dataset name, or
// the endpoint are. WithFix replaces, so this is the only wrapper — callers
// must not wrap again.
func resolveEvals(f *baselineFlags) (evalSource, error) {
	if strings.HasPrefix(f.evalsPath, evalsLangsmithPrefix) {
		ev, err := langsmith.New(langsmith.Options{
			Dataset:              strings.TrimPrefix(f.evalsPath, evalsLangsmithPrefix),
			HoldoutFrac:          f.holdoutFrac,
			SplitSeed:            f.splitSeed,
			AllowInsecureBaseURL: f.allowInsecureURL,
			AllowPrivateAddress:  f.allowPrivateAddress,
		})
		if err != nil {
			return nil, errs.ErrInvalidInput.WithFix(langsmithNewFix(err)).Wrap(err)
		}
		return ev, nil
	}

	if strings.HasPrefix(f.evalsPath, evalsLangfusePrefix) {
		ev, err := langfuse.New(langfuse.Options{
			Dataset:              strings.TrimPrefix(f.evalsPath, evalsLangfusePrefix),
			HoldoutFrac:          f.holdoutFrac,
			SplitSeed:            f.splitSeed,
			AllowInsecureBaseURL: f.allowInsecureURL,
			AllowPrivateAddress:  f.allowPrivateAddress,
		})
		if err != nil {
			return nil, errs.ErrInvalidInput.WithFix(langfuseNewFix(err)).Wrap(err)
		}
		return ev, nil
	}

	ev, err := jsonl.New(jsonl.Options{
		Path:        f.evalsPath,
		HoldoutFrac: f.holdoutFrac,
		SplitSeed:   f.splitSeed,
	})
	if err != nil {
		return nil, errs.ErrInvalidInput.WithFix(
			"check --evals and --holdout-frac",
		).Wrap(err)
	}
	return ev, nil
}

// langsmithNewFix picks the actionable fix for a langsmith.New refusal, by
// cause. The failure classes are distinct, and a compound fix for all of
// them is noise: a missing key is not fixed by re-checking the endpoint,
// and a refused endpoint is not a key problem.
func langsmithNewFix(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, langsmith.DefaultKeyEnv):
		return "set " + langsmith.DefaultKeyEnv
	case strings.Contains(msg, "no dataset name"):
		return "name the dataset in --evals, as langsmith:<dataset-name>"
	case strings.Contains(msg, "holdout fraction"):
		return "check --holdout-frac: it must be in [0, 1)"
	default:
		// The endpoint refusals: parse, scheme, host, plain HTTP, and the
		// private/link-local address families.
		return "check LANGSMITH_ENDPOINT, and opt into insecure or private " +
			"endpoints only when the deployment is self-hosted"
	}
}

// langfuseNewFix is the langfuse.New counterpart to langsmithNewFix: same
// failure classes, different names. A missing key names both environment
// variables, because Langfuse authenticates every request with the pair as
// basic auth.
func langfuseNewFix(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, langfuse.PublicKeyEnv) || strings.Contains(msg, langfuse.SecretKeyEnv):
		return "set " + langfuse.PublicKeyEnv + " and " + langfuse.SecretKeyEnv
	case strings.Contains(msg, "no dataset name"):
		return "name the dataset in --evals, as langfuse:<dataset-name>"
	case strings.Contains(msg, "holdout fraction"):
		return "check --holdout-frac: it must be in [0, 1)"
	default:
		// The endpoint refusals: parse, scheme, host, plain HTTP, and the
		// private/link-local address families.
		return "check LANGFUSE_HOST, and opt into insecure or private " +
			"endpoints only when the deployment is self-hosted"
	}
}

// countsSplitFix picks the fix for a CountSplits failure, per source.
// jsonl's failures point at a line in a file; the dataset adapters have no
// line — their failures name a dataset, an endpoint, or an item, and telling
// the user to fix "the reported line" would send them looking for a line
// that does not exist.
func countsSplitFix(src evalSource) string {
	switch src.(type) {
	case *langsmith.Evals, *langfuse.Evals:
		return "fix the reported dataset, endpoint, or example, then re-run"
	}
	return "fix the reported line, then re-run"
}

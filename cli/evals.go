package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/knograph/kno/adapters/evals/braintrust"
	"github.com/knograph/kno/adapters/evals/hf"
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
// addressed — a name that looks like the grammar is the grammar. An hf:
// source is four slash-separated segments — hf:<org>/<name>/<config>/<split>
// — because a Hugging Face split is addressed by that pair of pairs, and
// nothing else.
const (
	evalsLangsmithPrefix  = "langsmith:"
	evalsLangfusePrefix   = "langfuse:"
	evalsBraintrustPrefix = "braintrust:"
	evalsHFPrefix         = "hf:"
)

// resolveEvals turns the --evals flag into an eval source.
//
// The bare path is the jsonl adapter, unchanged. The langsmith: prefix
// selects the LangSmith adapter, which reads its key from
// LANGSMITH_API_KEY (never from a flag — a credential in a shell history or
// process listing is a credential lost) and its endpoint from
// LANGSMITH_ENDPOINT. The langfuse: prefix selects the Langfuse adapter,
// which reads its credential pair from LANGFUSE_PUBLIC_KEY and
// LANGFUSE_SECRET_KEY and its host from LANGFUSE_HOST. The braintrust:
// prefix selects the Braintrust adapter, which reads its key from
// BRAINTRUST_API_KEY and its host from BRAINTRUST_API_BASE_URL. The endpoint
// security opt-outs mirror the agent transport's --allow-insecure-base-url
// and --allow-private-address, so a self-hosted deployment is reachable
// with the same two flags a self-hosted model endpoint needs.
//
// The error fix is per source, because the failure modes are: for JSONL, the
// path is wrong; for the dataset adapters, the keys, the dataset name, or
// the endpoint are. WithFix replaces, so this is the only wrapper — callers
// must not wrap again.
func resolveEvals(f *baselineFlags) (evalSource, error) {
	if strings.HasPrefix(f.evalsPath, evalsBraintrustPrefix) {
		ev, err := braintrust.New(braintrust.Options{
			Dataset:              strings.TrimPrefix(f.evalsPath, evalsBraintrustPrefix),
			HoldoutFrac:          f.holdoutFrac,
			SplitSeed:            f.splitSeed,
			AllowInsecureBaseURL: f.allowInsecureURL,
			AllowPrivateAddress:  f.allowPrivateAddress,
		})
		if err != nil {
			return nil, errs.ErrInvalidInput.WithFix(braintrustNewFix(err)).Wrap(err)
		}
		return ev, nil
	}

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

	if strings.HasPrefix(f.evalsPath, evalsHFPrefix) {
		dataset, config, split, err := parseHFEvals(f.evalsPath)
		if err != nil {
			return nil, errs.ErrInvalidInput.WithFix(
				"name the dataset in --evals, as hf:<org>/<name>/<config>/<split>",
			).Wrap(err)
		}
		ev, err := hf.New(hf.Options{
			Dataset:     dataset,
			Config:      config,
			Split:       split,
			HoldoutFrac: f.holdoutFrac,
			SplitSeed:   f.splitSeed,
		})
		if err != nil {
			return nil, errs.ErrInvalidInput.WithFix(hfNewFix(err)).Wrap(err)
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

// parseHFEvals splits an hf: source into its four segments.
//
// Four segments and only four: the org, the dataset name, the config, and
// the split. A dataset name cannot be re-joined from fewer, and a revision
// is never the fifth — the x-revision header is the fingerprint, and a
// revision query parameter is ignored by the server. Empty segments are
// refused by name, not counted silently.
func parseHFEvals(path string) (dataset, config, split string, err error) {
	rest := strings.TrimPrefix(path, evalsHFPrefix)
	seg := strings.Split(rest, "/")
	if len(seg) != 4 {
		return "", "", "", fmt.Errorf("an hf: eval source is hf:<org>/<name>/<config>/<split> "+
			"— four slash-separated segments; got %d", len(seg))
	}
	for i, s := range seg {
		if s == "" {
			return "", "", "", fmt.Errorf("segment %d of the hf: eval source is empty; an "+
				"hf: eval source is hf:<org>/<name>/<config>/<split>", i+1)
		}
	}
	return seg[0] + "/" + seg[1], seg[2], seg[3], nil
}

// hfNewFix picks the actionable fix for an hf.New refusal. The grammar
// refusals point at the grammar; the holdout refusal points at the flag;
// everything else — endpoint refusals reachable only by library callers,
// who can set Host — gets the source-level pointer.
func hfNewFix(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no dataset name"), strings.Contains(msg, "no config"), strings.Contains(msg, "no split"):
		return "name the dataset in --evals, as hf:<org>/<name>/<config>/<split>"
	case strings.Contains(msg, "holdout fraction"):
		return "check --holdout-frac: it must be in [0, 1)"
	default:
		return "check the hf: source in --evals"
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

// braintrustNewFix is the braintrust.New counterpart to langsmithNewFix: the
// same failure classes under Braintrust's environment names. The endpoint
// defaults to the hosted API, so a refusal in that class names the opt-in
// flags rather than a variable the user may never have set.
func braintrustNewFix(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, braintrust.KeyEnv):
		return "set " + braintrust.KeyEnv
	case strings.Contains(msg, "no dataset name"):
		return "name the dataset in --evals, as braintrust:<dataset-name>"
	case strings.Contains(msg, "holdout fraction"):
		return "check --holdout-frac: it must be in [0, 1)"
	default:
		// The endpoint refusals: parse, scheme, host, plain HTTP, and the
		// private/link-local address families.
		return "check BRAINTRUST_API_BASE_URL, and opt into insecure or private " +
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
	case *langsmith.Evals, *langfuse.Evals, *braintrust.Evals:
		return "fix the reported dataset, endpoint, or example, then re-run"
	case *hf.Evals:
		return "fix the reported dataset, split, or row, then re-run"
	}
	return "fix the reported line, then re-run"
}

package cli

import (
	"fmt"
	"strings"

	poolcsv "github.com/knograph/kno/adapters/pool/csv"
	poolhf "github.com/knograph/kno/adapters/pool/hf"
	pooljsonl "github.com/knograph/kno/adapters/pool/jsonl"
	poolmarkdown "github.com/knograph/kno/adapters/pool/markdown"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
)

// The --pool grammar, pinned here and in the help-snapshot tests. A bare path
// still means a JSONL file; the prefixes select the other adapters. A file
// that happens to be named "csv:..." cannot be addressed — a name that looks
// like the grammar is the grammar, the same rule as --evals' langsmith:
// prefix.
const (
	poolCSVPrefix = "csv:"
	poolMDPrefix  = "md:"
	poolHFPrefix  = "hf:"
)

// resolvePool turns the --pool flag into a Pool source.
//
// The bare path is the jsonl adapter, unchanged. The csv: prefix selects the
// CSV adapter, whose file carries one Asset per row. The md: prefix selects
// the markdown adapter, reading a directory of .md files (or one file), split
// into `## ` sections when splitSections is set.
//
// The --split-sections refusal lives here so it fires before anything is
// read: a flag that silently does nothing for a source it cannot affect is a
// flag a user stops believing in.
func resolvePool(poolPath string, splitSections bool) (core.Pool, error) {
	switch {
	case strings.HasPrefix(poolPath, poolCSVPrefix):
		if splitSections {
			return nil, errs.ErrInvalidInput.WithFix(
				"--split-sections only applies to an md: pool; a csv pool is already one asset per row",
			).Wrap(fmt.Errorf("--split-sections with a csv: pool"))
		}
		p, err := poolcsv.New(poolcsv.Options{Path: strings.TrimPrefix(poolPath, poolCSVPrefix)})
		if err != nil {
			return nil, errs.ErrInvalidInput.WithFix("check --pool").Wrap(err)
		}
		return p, nil

	case strings.HasPrefix(poolPath, poolMDPrefix):
		p, err := poolmarkdown.New(poolmarkdown.Options{
			Path:          strings.TrimPrefix(poolPath, poolMDPrefix),
			SplitSections: splitSections,
		})
		if err != nil {
			return nil, errs.ErrInvalidInput.WithFix("check --pool").Wrap(err)
		}
		return p, nil

	case strings.HasPrefix(poolPath, poolHFPrefix):
		if splitSections {
			return nil, errs.ErrInvalidInput.WithFix(
				"--split-sections only applies to an md: pool",
			).Wrap(fmt.Errorf("--split-sections with an hf: pool"))
		}
		dataset, config, split, kind, err := parseHFPool(poolPath)
		if err != nil {
			return nil, errs.ErrInvalidInput.WithFix(poolHFFix(err)).Wrap(err)
		}
		p, err := poolhf.New(poolhf.Options{
			Dataset: dataset,
			Config:  config,
			Split:   split,
			Kind:    kind,
		})
		if err != nil {
			return nil, errs.ErrInvalidInput.WithFix(poolHFFix(err)).Wrap(err)
		}
		return p, nil

	default:
		if splitSections {
			return nil, errs.ErrInvalidInput.WithFix(
				"--split-sections only applies to an md: pool",
			).Wrap(fmt.Errorf("--split-sections with a jsonl pool"))
		}
		p, err := pooljsonl.New(pooljsonl.Options{Path: poolPath})
		if err != nil {
			return nil, errs.ErrInvalidInput.WithFix("check --pool").Wrap(err)
		}
		return p, nil
	}
}

// parseHFPool splits an hf: pool into its four segments and its declared
// kind.
//
// The kind is the part after the LAST colon: a config or split name cannot
// contain one, and the grammar is closed, so the last colon is unambiguous.
// A pool without a declared kind is refused here rather than defaulted — an
// Asset's kind is a routing decision, and routing by guess is how an Asset
// lands in the wrong destination.
func parseHFPool(path string) (dataset, config, split, kind string, err error) {
	rest := strings.TrimPrefix(path, poolHFPrefix)
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return "", "", "", "", fmt.Errorf("an hf: pool declares its kind — " +
			"hf:<org>/<name>/<config>/<split>:<kind>, e.g. hf:org/name/main/train:knowledge")
	}
	kind = rest[i+1:]
	if kind == "" {
		return "", "", "", "", fmt.Errorf("the kind after the final colon is empty; an hf: " +
			"pool is hf:<org>/<name>/<config>/<split>:knowledge or :behavior")
	}
	seg := strings.Split(rest[:i], "/")
	if len(seg) != 4 {
		return "", "", "", "", fmt.Errorf("an hf: pool is hf:<org>/<name>/<config>/<split>:<kind> "+
			"— four slash-separated segments before the kind; got %d", len(seg))
	}
	for i, s := range seg {
		if s == "" {
			return "", "", "", "", fmt.Errorf("segment %d of the hf: pool is empty; an hf: "+
				"pool is hf:<org>/<name>/<config>/<split>:<kind>", i+1)
		}
	}
	return seg[0] + "/" + seg[1], seg[2], seg[3], kind, nil
}

// poolHFFix picks the actionable fix for an hf pool refusal. The kind
// spellings are closed and exact; anything else points at the grammar.
func poolHFFix(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unknown kind"):
		return "declare the kind in --pool as hf:<org>/<name>/<config>/<split>:" +
			"knowledge or :behavior"
	default:
		return "check --pool: an hf pool is hf:<org>/<name>/<config>/<split>:<kind>"
	}
}

package cli

import (
	"fmt"
	"strings"

	poolcsv "github.com/knograph/kno/adapters/pool/csv"
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

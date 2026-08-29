// Package csv reads candidate Assets from a CSV file.
//
// The file's contract is a header row naming the columns, then one Asset per
// data row. `id` and `content` are required columns; `kind` and `tags` are
// optional (tags entries separated by semicolons, see TagsSeparator). The
// header is validated loudly: a missing id column, a missing content column,
// or an unknown column is a fatal, named load error — the column-name
// contract is documented and validated, and wrong columns are a load error,
// not a guess.
package csv

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"strconv"

	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Options configures a Pool source.
type Options struct {
	// Path to the .csv file.
	Path string
}

// Pool reads candidate Assets from a CSV file.
//
// It satisfies core.Pool. Memory is bounded by the largest single record:
// records stream from the file and content is never retained past its yield,
// but the set of Asset IDs seen so far is held in full — a pool far larger
// than RAM streams, but a pool of a million Assets does hold a million IDs.
// That is the price of refusing duplicates, and refusing them is not
// optional: two Assets sharing an ID are indistinguishable in every
// measurement row and every later report.
//
// Row numbers in errors and provenance count RECORDS, with the header as row
// 1 and the first data row as row 2. For the common single-line-per-record
// file that is the physical line number; a quoted field spanning lines shifts
// the physical line but not the record number.
type Pool struct {
	opts Options
}

// New returns a Pool reading from opts.Path.
//
// The file is not opened here. Opening happens per iteration so that each
// call to Assets gets an independent cursor, which is what lets the
// conformance harness run several iterations over one source and what lets a
// resumed run re-read from the start.
func New(opts Options) (*Pool, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("csv: no path given")
	}
	return &Pool{opts: opts}, nil
}

// Assets yields every Asset in the file.
//
// Contract, per core.Pool — identical to core.Evals.Cases:
//
//   - A yielded error is FATAL. This adapter does not skip malformed rows. A
//     file with a bad row is a file the user should fix, and dropping the row
//     instead would shrink the pool without anything showing it: the Asset
//     count is the denominator behind every later "N of M assets earned their
//     place", and if one adapter skipped while another halted, two runs
//     measured over different populations would look identical.
//   - Cleanup is deferred INSIDE the closure, so an early break still closes
//     the file.
//   - ctx is checked before each yield.
//   - Yielded Assets are borrowed for one iteration. Clone before retaining.
//
// An empty file — no header at all — yields nothing: an empty pool is a
// decision for the caller to refuse before it spends, not an error for the
// adapter to invent.
func (p *Pool) Assets(ctx context.Context) (iter.Seq2[*core.Asset, error], error) {
	f, err := os.Open(p.opts.Path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", p.opts.Path, err)
	}

	path := p.opts.Path

	return func(yield func(*core.Asset, error) bool) {
		// Inside the closure: an early break must still close the file, and a
		// deferred close registered outside this function would not run until
		// Assets' own return, which has already happened.
		// A close error on a read-only file cannot affect what was already read.
		defer func() { _ = f.Close() }()

		r := csv.NewReader(f)
		// FieldsPerRecord stays 0, so the header fixes the record width and a
		// row with a different width surfaces as a *csv.ParseError — fatal and
		// named, never truncated or padded into shape.

		header, err := r.Read()
		if errors.Is(err, io.EOF) {
			return // an empty file holds no Assets
		}
		if err != nil {
			yield(nil, fmt.Errorf("%s: reading the header: %w", path, err))
			return
		}

		cols, err := columnsOf(path, header)
		if err != nil {
			yield(nil, err)
			return
		}

		seen := make(map[string]struct{})
		row := 1 // the header; data rows count up from 2
		for {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}

			rec, err := r.Read()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				// The row exists but cannot be read — a quote error, a field
				// count that drifted from the header. Fatal, never skipped.
				yield(nil, fmt.Errorf("%s row %d: %w", path, row+1, err))
				return
			}
			row++

			a, err := p.assetAt(rec, cols, row, seen)
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(a, nil) {
				return
			}
		}
	}, nil
}

// assetAt turns one data row into an Asset, or reports why the file must be
// fixed.
//
// seen is updated in place; a duplicate ID is refused rather than tolerated.
func (p *Pool) assetAt(rec []string, cols map[string]int, row int, seen map[string]struct{}) (*core.Asset, error) {
	path := p.opts.Path

	id := rec[cols[colID]]
	if id == "" {
		// Fatal, not generated from the row number. Every measurement row,
		// every Valuation, and the resume that reads them are keyed on this
		// ID, so an ID derived from file POSITION would make an Asset's whole
		// measurement history depend on where its row happened to sit —
		// inserting one row above it would orphan everything already paid for.
		return nil, fmt.Errorf(
			"%s row %d: asset has no id, and every measurement is keyed on it; "+
				"give every asset a stable id", path, row,
		)
	}
	content := rec[cols[colContent]]
	if content == "" {
		// An Asset with no content cannot be injected and costs nothing to
		// carry, so it would rank at an undefined delta_per_cost — a zero
		// denominator sorts to the top of a greedy selection.
		return nil, fmt.Errorf("%s row %d: asset %q has no content", path, row, id)
	}
	if _, dup := seen[id]; dup {
		return nil, fmt.Errorf("%s row %d: duplicate asset id %q", path, row, id)
	}

	kindSpelling := ""
	if i, ok := cols[colKind]; ok {
		kindSpelling = rec[i]
	}
	kind, err := kindOf(kindSpelling)
	if err != nil {
		return nil, fmt.Errorf("%s row %d: asset %q: %w", path, row, id, err)
	}
	seen[id] = struct{}{}

	var tags []string
	if i, ok := cols[colTags]; ok {
		tags = parseTags(rec[i])
	}

	// A fresh []byte for the content: the record's strings are the reader's,
	// and while csv.Reader does not reuse the backing array the way a scanner
	// does, the borrow contract is the producer's to uphold — never a window
	// into state the producer may touch again.
	contentBytes := []byte(content)

	return &core.Asset{
		Id:      id,
		Content: contentBytes,
		Kind:    kind,
		Tags:    tags,
		// The file said which kind this is, so the report can tell an asserted
		// routing decision from a measured one.
		UserOverridden: kindSpelling != "",
		Cost:           &knov1.CostVector{ContextTokens: contextTokens(len(contentBytes))},
		Provenance: &knov1.Provenance{
			Source:    "csv",
			SourceRef: path + ":" + strconv.Itoa(row),
			// IngestedAt is deliberately unset: it would make two reads of an
			// unchanged file produce different Assets, so a pool that had not
			// moved would look changed on every read.
		},
	}, nil
}

// Compile-time proof that this adapter satisfies the Ring-0 contract.
var _ core.Pool = (*Pool)(nil)

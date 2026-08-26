// Package jsonl reads candidate Assets from a JSON Lines file.
package jsonl

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"iter"
	"math"
	"os"
	"strconv"
	"unicode/utf8"

	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// DefaultMaxRecordBytes caps one record when Options leaves it unset.
//
// Larger than the Evals adapter's cap, because the two hold different things:
// a Case is a prompt and an expectation, while an Asset may legitimately be a
// whole document. There is still a cap, and it is the same reason — without
// one, a malformed file (a single enormous line, a binary blob renamed .jsonl)
// reads itself into memory in full, defeating the streaming profile the rest
// of the pipeline depends on.
const DefaultMaxRecordBytes = 16 << 20 // 16 MiB

// initialBufBytes is the scanner's starting buffer. It grows on demand up to
// the cap, so the common pool of small Assets never allocates the maximum.
const initialBufBytes = 64 << 10

// Options configures a Pool source.
type Options struct {
	// Path to the .jsonl file.
	Path string

	// MaxRecordBytes caps one record. Zero means DefaultMaxRecordBytes.
	//
	// A knob rather than a constant because the right cap is a property of the
	// pool, and an error whose only fix is "edit kno's source" is not a fix.
	MaxRecordBytes int
}

func (o Options) maxRecordBytes() int {
	if o.MaxRecordBytes <= 0 {
		return DefaultMaxRecordBytes
	}
	return o.MaxRecordBytes
}

// Pool reads candidate Assets from a JSON Lines file.
//
// It satisfies core.Pool. Memory is bounded by the largest single record plus
// the set of Asset IDs seen so far — content is never retained past its yield,
// so a pool far larger than RAM streams, but a pool of a million Assets does
// hold a million IDs. That is the price of refusing duplicates, and refusing
// them is not optional: two Assets sharing an ID are indistinguishable in
// every measurement row and every later report.
type Pool struct {
	opts Options
}

// New returns a Pool reading from opts.Path.
//
// The file is not opened here. Opening happens per iteration so that each call
// to Assets gets an independent cursor, which is what lets the conformance
// harness run several iterations over one source and what lets a resumed run
// re-read from the start.
func New(opts Options) (*Pool, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("jsonl: no path given")
	}
	if opts.MaxRecordBytes < 0 {
		return nil, fmt.Errorf("jsonl: max record size %d must not be negative", opts.MaxRecordBytes)
	}
	return &Pool{opts: opts}, nil
}

// Assets yields every Asset in the file.
//
// Contract, per core.Pool — identical to core.Evals.Cases:
//
//   - A yielded error is FATAL. This adapter does not skip malformed records.
//     A file with a bad line is a file the user should fix, and dropping the
//     line instead would shrink the pool without anything showing it: the
//     Asset count is the denominator behind every later "N of M assets earned
//     their place", and if one adapter skipped while another halted, two runs
//     measured over different populations would look identical.
//   - Cleanup is deferred INSIDE the closure, so an early break still closes
//     the file.
//   - ctx is checked before each yield.
//   - Yielded Assets are borrowed for one iteration. Clone before retaining.
func (p *Pool) Assets(ctx context.Context) (iter.Seq2[*core.Asset, error], error) {
	f, err := os.Open(p.opts.Path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", p.opts.Path, err)
	}

	path := p.opts.Path
	maxBytes := p.opts.maxRecordBytes()

	return func(yield func(*core.Asset, error) bool) {
		// Inside the closure: an early break must still close the file, and a
		// deferred close registered outside this function would not run until
		// Assets' own return, which has already happened.
		// A close error on a read-only file cannot affect what was already read.
		defer func() { _ = f.Close() }()

		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, min(initialBufBytes, maxBytes)), maxBytes)

		seen := make(map[string]struct{})
		line := 0

		for sc.Scan() {
			line++
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}

			raw := sc.Bytes()
			if len(bytes.TrimSpace(raw)) == 0 {
				continue
			}

			a, err := p.assetAt(raw, line, seen)
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(a, nil) {
				return
			}
		}

		if err := sc.Err(); err != nil {
			// A record over the cap arrives here as bufio.ErrTooLong. Say so in
			// terms the user can act on rather than passing the raw error up.
			yield(nil, fmt.Errorf(
				"%s line %d: %w (records are capped at %d bytes; split the asset or raise MaxRecordBytes)",
				path, line+1, err, maxBytes))
		}
	}, nil
}

// assetAt decodes one record and turns it into an Asset, or reports why the
// file must be fixed.
//
// seen is updated in place; a duplicate ID is refused rather than tolerated.
func (p *Pool) assetAt(raw []byte, line int, seen map[string]struct{}) (*core.Asset, error) {
	// Validated on the RAW bytes, before decoding, and this ordering is the
	// whole point. encoding/json silently substitutes U+FFFD for any invalid
	// byte, lone surrogate, or CESU-8 sequence and returns no error — so by the
	// time a record is a Go string the corruption has already happened and
	// utf8.ValidString reports true on the damaged result. The adapters' own
	// non-UTF-8 refusal sits downstream of this and therefore cannot see it.
	//
	// The damage is not cosmetic: the Asset a run measures would differ from
	// the Asset on disk, with provenance still pointing at this line, and the
	// provider bills three bytes where the estimate counted one.
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf(
			"%s line %d: the record is not valid UTF-8, and decoding it would "+
				"silently replace the bad bytes rather than fail — so the Asset "+
				"measured would differ from the Asset on disk",
			p.opts.Path, line)
	}

	rec, err := decode(raw)
	if err != nil {
		return nil, fmt.Errorf("%s line %d: %w", p.opts.Path, line, err)
	}
	if rec.ID == "" {
		// Fatal, not generated from the line number. Every measurement row,
		// every Valuation, and the resume that reads them are keyed on this
		// ID, so an ID derived from file POSITION would make an Asset's whole
		// measurement history depend on where its line happened to sit —
		// inserting one line above it would orphan everything already paid for.
		return nil, fmt.Errorf(
			"%s line %d: asset has no id, and every measurement is keyed on it; "+
				"give every asset a stable id", p.opts.Path, line)
	}
	if rec.Content == "" {
		// An Asset with no content cannot be injected and costs nothing to
		// carry, so it would rank at an undefined delta_per_cost — a zero
		// denominator sorts to the top of a greedy selection.
		return nil, fmt.Errorf("%s line %d: asset %q has no content", p.opts.Path, line, rec.ID)
	}
	if _, dup := seen[rec.ID]; dup {
		return nil, fmt.Errorf("%s line %d: duplicate asset id %q", p.opts.Path, line, rec.ID)
	}
	kind, err := kindOf(rec.Kind)
	if err != nil {
		return nil, fmt.Errorf("%s line %d: asset %q: %w", p.opts.Path, line, rec.ID, err)
	}
	seen[rec.ID] = struct{}{}

	// A fresh []byte, never a window into the scanner's buffer: the scanner
	// reuses that buffer on the next line, so an aliased Content would rewrite
	// itself under any consumer that held it for even one more iteration.
	content := []byte(rec.Content)

	return &core.Asset{
		Id:      rec.ID,
		Content: content,
		Kind:    kind,
		Title:   rec.Title,
		Tags:    rec.Tags,
		// The file said which kind this is, so the report can tell an asserted
		// routing decision from a measured one.
		UserOverridden: rec.Kind != "",
		Cost:           rec.costVector(contextTokens(len(content))),
		Provenance: &knov1.Provenance{
			Source:    "jsonl",
			SourceRef: p.opts.Path + ":" + strconv.Itoa(line),
			// IngestedAt is deliberately unset: it would make two reads of an
			// unchanged file produce different Assets, so a pool that had not
			// moved would look changed on every read.
		},
	}, nil
}

// bytesPerToken is the divisor behind contextTokens. See its godoc for why it
// is this number and not the one the reservation path uses.
const bytesPerToken = 3.6

// contextTokens estimates what carrying this Asset adds to every request.
//
// This is a RANKING denominator, not a reservation, and the two want opposite
// errors. adapters/agent/pricing's countTokens deliberately reserves about 3x
// what English prose really costs, because under-reserving is what walks a run
// past its cap. Feeding that number into delta_per_cost would rank the
// portfolio by content type instead: measured against a real tokenizer its
// over-count is about 1.1x on base64 and about 2.7x on prose, so two Assets of
// identical true token cost differ by ~2.4x in the denominator
// (docs/debt.md#68). It also takes a MODEL argument, and an Asset's cost is
// read before any model is chosen — a denominator that moved with the run's
// model would make two pools' rankings incomparable.
//
// So: bytes over a fixed divisor, centered on the one measurement in this tree
// — countTokens' own tokenizer survey puts English prose at 3.6 bytes/token.
//
// The bias, stated because a reader deciding what delta_per_cost means is
// entitled to it: this UNDER-counts machine-shaped content, by roughly 1.7x on
// minified JSON and 2.4x on base64, and is roughly unbiased on prose. No fixed
// divisor does better — the ratio between content types is a property of the
// tokenizer, not of the constant, so the only choice a divisor makes is which
// content type it centers. A real BPE tokenizer is the unbiased answer and is
// refused here for the reason countTokens' godoc gives: a large dependency plus
// a per-model vocabulary that goes stale silently.
//
// Rounded up, so a non-empty Asset never costs zero tokens. delta_per_cost over
// a zero denominator is an infinity, and an infinity sorts to the top of a
// greedy ranking.
func contextTokens(sizeBytes int) int64 {
	return int64(math.Ceil(float64(sizeBytes) / bytesPerToken))
}

// Compile-time proof that this adapter satisfies the Ring-0 contract.
var _ core.Pool = (*Pool)(nil)

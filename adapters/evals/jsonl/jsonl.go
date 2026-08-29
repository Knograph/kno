// Package jsonl reads eval Cases from a JSON Lines file.
package jsonl

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"iter"
	"os"
	"strconv"

	"github.com/knograph/kno/adapters/evals/split"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// maxLineBytes caps one record.
//
// A Case is a prompt and an expectation, not a corpus. Without a cap, a
// malformed file — one enormous line, a binary blob renamed .jsonl — reads
// itself into memory in full, defeating the streaming profile the whole
// pipeline depends on.
const maxLineBytes = 4 << 20 // 4 MiB

// Options configures an Evals source.
type Options struct {
	// Path to the .jsonl file.
	Path string

	// HoldoutFrac is the share of Cases held back. Zero means
	// DefaultHoldoutFrac.
	HoldoutFrac float64

	// SplitSeed deliberately re-splits an eval set. Normally empty; recorded
	// on the Run and part of the resume fingerprint.
	SplitSeed string
}

func (o Options) holdoutFrac() float64 {
	if o.HoldoutFrac <= 0 {
		return DefaultHoldoutFrac
	}
	return o.HoldoutFrac
}

// Evals reads Cases from a JSON Lines file.
//
// It satisfies core.Evals. Callers that must not see the holdout wrap it with
// core.Seal, which is a distinct type — so a stage that forgets does not
// compile.
type Evals struct {
	opts Options
}

// New returns an Evals reading from opts.Path.
//
// The file is not opened here. Opening happens per iteration so that each call
// to Cases gets an independent cursor, which is what lets the conformance
// harness run several iterations over one source and what lets a resumed run
// re-read from the start.
func New(opts Options) (*Evals, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("jsonl: no path given")
	}
	if opts.HoldoutFrac < 0 || opts.HoldoutFrac >= 1 {
		return nil, fmt.Errorf("jsonl: holdout fraction %v must be in [0, 1)", opts.HoldoutFrac)
	}
	return &Evals{opts: opts}, nil
}

// Cases yields every Case in the file, each with its split assigned.
//
// Contract, per core.Evals:
//
//   - A yielded error is FATAL. This adapter does not skip malformed records —
//     a file with a bad line is a file the user should fix, and silently
//     dropping records would shrink the denominator behind every later delta
//     without anything showing it.
//   - Cleanup is deferred INSIDE the closure, so an early break still closes
//     the file.
//   - ctx is checked before each yield.
//   - Yielded Cases are borrowed for one iteration. The backing record is
//     reused; clone before retaining.
func (e *Evals) Cases(ctx context.Context) (iter.Seq2[*core.Case, error], error) {
	f, err := os.Open(e.opts.Path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", e.opts.Path, err)
	}

	frac := e.opts.holdoutFrac()
	seed := e.opts.SplitSeed

	return func(yield func(*core.Case, error) bool) {
		// Inside the closure: an early break must still close the file, and a
		// deferred close registered outside this function would not run until
		// Cases' own return, which has already happened.
		// A close error on a read-only file cannot affect what was already read.
		defer func() { _ = f.Close() }()

		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)

		seen := make(map[string]struct{})
		line := 0

		for sc.Scan() {
			line++
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}

			raw := sc.Bytes()
			if len(trimSpace(raw)) == 0 {
				continue
			}

			rec, err := decode(raw)
			if err != nil {
				yield(nil, fmt.Errorf("%s line %d: %w", e.opts.Path, line, err))
				return
			}
			if rec.ID == "" {
				// Fatal, not defaulted. An earlier version filled this in with
				// "path:line", which made the split depend on file POSITION —
				// so inserting a line, reordering, or renaming the file
				// silently reclassified every Case after it. A Case scored and
				// reported as dev on Monday would become untouched holdout on
				// Tuesday, which is the precise failure the split's own doc
				// comment rejects a per-run salt for.
				//
				// The format already treats a missing input as fatal. An ID is
				// no less load-bearing: the split is keyed on it.
				yield(nil, fmt.Errorf(
					"%s line %d: case has no id, and the dev/holdout split is keyed on it; "+
						"give every case a stable id", e.opts.Path, line,
				))
				return
			}
			if rec.Input == "" {
				yield(nil, fmt.Errorf("%s line %d: case %q has no input", e.opts.Path, line, rec.ID))
				return
			}
			if _, dup := seen[rec.ID]; dup {
				// Duplicate IDs are fatal rather than tolerated: the split is
				// keyed on the ID, so two Cases sharing one would land in the
				// same half and be indistinguishable in every later report.
				yield(nil, fmt.Errorf("%s line %d: duplicate case id %q", e.opts.Path, line, rec.ID))
				return
			}
			seen[rec.ID] = struct{}{}

			// The record's own provenance fields win over the positional
			// default. A mined record (`kno mine`) carries its transcript
			// source_ref and its derivation note, and clobbering them with
			// path:line would destroy exactly the audit trail Provenance
			// exists to carry. Files written before the fields existed keep
			// the old behavior byte-for-byte.
			sourceRef := rec.SourceRef
			if sourceRef == "" {
				sourceRef = e.opts.Path + ":" + strconv.Itoa(line)
			}

			c := &core.Case{
				Id:       rec.ID,
				Input:    rec.Input,
				Expected: rec.Expected,
				Rubric:   rec.Rubric,
				Tags:     rec.Tags,
				Split:    split.AssignSplit(rec.ID, seed, frac),
				Provenance: &knov1.Provenance{
					Source:         "jsonl",
					SourceRef:      sourceRef,
					Derived:        rec.Derived != nil && *rec.Derived,
					DerivationNote: rec.DerivationNote,
				},
			}
			if !yield(c, nil) {
				return
			}
		}

		if err := sc.Err(); err != nil {
			// A line over the cap arrives here as bufio.ErrTooLong. Say so in
			// terms the user can act on rather than passing the raw error up.
			yield(nil, fmt.Errorf("%s line %d: %w (records are capped at %d bytes)",
				e.opts.Path, line+1, err, maxLineBytes))
		}
	}, nil
}

// CountSplits reads the file and reports how it divides, without yielding
// Cases.
//
// Ingestion needs the counts up front: a zero-Case holdout must be refused
// before any money is spent, not discovered at Validate after a full run.
func (e *Evals) CountSplits(ctx context.Context) (SplitCounts, error) {
	seq, err := e.Cases(ctx)
	if err != nil {
		return SplitCounts{}, err
	}

	counts := SplitCounts{HoldoutFrac: e.opts.holdoutFrac()}
	for c, err := range seq {
		if err != nil {
			return SplitCounts{}, err
		}
		if c.GetProvenance().GetDerived() {
			// The weak-label count rides the same ingestion pass the split
			// counts do, so the Run can record it at close and a
			// weak-label eval cannot pass for a hand-authored one.
			counts.WeakLabelCases++
		}
		if c.GetSplit() == knov1.Split_SPLIT_HOLDOUT {
			counts.Holdout++
			continue
		}
		counts.Dev++
	}
	return counts, nil
}

// ContentHash is the eval source's content hash, for the Run's resume
// fingerprint.
//
// Content, not path and modification time. An in-place edit of the file must
// invalidate a resume, and a checkout that rewrites mtime without changing
// content must not.
func (e *Evals) ContentHash(ctx context.Context) (string, error) {
	f, err := os.Open(e.opts.Path)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", e.opts.Path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, contextReader{ctx: ctx, r: f}); err != nil {
		return "", fmt.Errorf("hashing %s: %w", e.opts.Path, err)
	}
	// The split configuration is part of the fingerprint, because changing
	// either input reclassifies Cases without the file itself changing.
	_, _ = h.Write(split.FingerprintSplit(e.opts.SplitSeed, e.opts.holdoutFrac()))
	// The path too. Case IDs are now required, so a rename no longer changes
	// any split — but a resume that silently switched to a different file of
	// identical content would still be measuring a different run than it
	// thinks, and saying so costs nothing.
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(e.opts.Path))

	return hex.EncodeToString(h.Sum(nil)), nil
}

// contextReader makes a long hash of a large file cancellable.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p) //nolint:wrapcheck // a pass-through reader must not reshape io.EOF
}

// trimSpace reports the record with surrounding ASCII whitespace removed, so a
// blank line is recognized without allocating.
func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && isSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// Compile-time proof that this adapter satisfies the Ring-0 contract.
var _ core.Evals = (*Evals)(nil)

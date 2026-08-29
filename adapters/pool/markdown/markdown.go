// Package markdown reads candidate Assets from Markdown files.
//
// The source is a directory of `.md` files (one Asset per file, recursively,
// in lexical order) or a single file. With SplitSections set, `## `-level
// sections become Assets instead — one doc is not one Asset. Each Asset's id
// is its file path; a section's id is the path plus SectionSeparator plus
// its heading. An optional `---` delimited front-matter block at the very top
// of a file carries its kind and tags (see parseFrontMatter).
package markdown

import (
	"context"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Options configures a Pool source.
type Options struct {
	// Path to a .md file, or to a directory of .md files.
	Path string

	// SplitSections makes each `## `-level section of the source's files an
	// Asset instead of the whole file. Sections are keyed by their heading
	// (escaped per SectionSeparator), so a duplicate heading in one file is
	// fatal — two Assets sharing an ID are indistinguishable in every
	// measurement row and every later report.
	SplitSections bool
}

// Pool reads candidate Assets from Markdown files.
//
// It satisfies core.Pool. Files stream: the directory is walked to a list of
// paths, and each file is read, one at a time, when its turn comes — never
// all files at once. Memory is bounded by the largest single file plus the
// file list.
//
// Only `.md` files are Assets. Everything else in the tree — other
// extensions, dot-directories — is passed over: a directory scan selects on
// extension, it does not validate the pool.
type Pool struct {
	opts Options
}

// New returns a Pool reading from opts.Path.
//
// The path is not opened here. Opening happens per iteration so that each
// call to Assets gets an independent cursor, which is what lets the
// conformance harness run several iterations over one source and what lets a
// resumed run re-read from the start. Whether the path names a file or a
// directory is decided at that time too.
func New(opts Options) (*Pool, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("markdown: no path given")
	}
	return &Pool{opts: opts}, nil
}

// Assets yields every Asset in the source.
//
// Contract, per core.Pool — identical to core.Evals.Cases:
//
//   - A yielded error is FATAL. This adapter does not skip malformed files or
//     duplicate section headings. A file with a bad front-matter block is a
//     file the user should fix, and dropping it instead would shrink the pool
//     without anything showing it: the Asset count is the denominator behind
//     every later "N of M assets earned their place".
//   - Cleanup is deferred INSIDE the closure, so an early break still closes
//     whatever is open.
//   - ctx is checked before each yield.
//   - Yielded Assets are borrowed for one iteration. Clone before retaining.
//
// An empty directory — no .md files at all — yields nothing: an empty pool is
// a decision for the caller to refuse before it spends, not an error for the
// adapter to invent.
func (p *Pool) Assets(ctx context.Context) (iter.Seq2[*core.Asset, error], error) {
	st, err := os.Stat(p.opts.Path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", p.opts.Path, err)
	}

	var files []string
	switch {
	case st.IsDir():
		files, err = markdownFiles(p.opts.Path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p.opts.Path, err)
		}
	case st.Mode().IsRegular():
		files = []string{p.opts.Path}
	default:
		return nil, fmt.Errorf("%s is neither a file nor a directory", p.opts.Path)
	}

	return func(yield func(*core.Asset, error) bool) {
		for _, path := range files {
			//nolint:gosec // G304: the path comes from the user's OWN --pool md:<dir> argument — reading exactly the directory the user named is the entire contract of this adapter; nothing here follows attacker-controlled input
			raw, err := os.ReadFile(path)
			if err != nil {
				yield(nil, fmt.Errorf("reading %s: %w", path, err))
				return
			}

			assets, err := p.assetAt(path, raw)
			if err != nil {
				yield(nil, err)
				return
			}
			for _, a := range assets {
				if err := ctx.Err(); err != nil {
					yield(nil, err)
					return
				}
				if !yield(a, nil) {
					return
				}
			}
		}
	}, nil
}

// markdownFiles lists every .md file under dir, recursively, in lexical
// order.
//
// Deterministic: the walk's order is what the consumer sees, and a pool whose
// Assets reordered themselves between reads would make every report that
// references an ordinal mislabel its rows.
func markdownFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller adds the directory context
	}
	sort.Strings(files)
	return files, nil
}

// assetAt turns one file's bytes into one or more Assets, or reports why the
// file must be fixed.
//
// In whole-file mode the Asset id is the file path. In split mode each
// `## `-level section is an Asset keyed on its heading, and a duplicate
// heading in the file is fatal — matching the jsonl adapter's duplicate-id
// refusal, because the heading IS the id.
func (p *Pool) assetAt(path string, raw []byte) ([]*core.Asset, error) {
	fm, err := parseFrontMatter(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	content := raw[fm.end:]

	kind, err := kindOf(fm.kind)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	if !p.opts.SplitSections {
		if len(strings.TrimSpace(string(content))) == 0 {
			// An Asset with no content cannot be injected and costs nothing to
			// carry, so it would rank at an undefined delta_per_cost — a zero
			// denominator sorts to the top of a greedy selection.
			return nil, fmt.Errorf("%s: asset %q has no content", path, path)
		}
		return []*core.Asset{p.asset(path, content, kind, fm, path)}, nil
	}

	sections := splitSections(content)
	if len(sections) == 1 && sections[0].heading == "" {
		// The file has no `## ` headings, so there is nothing to split: the
		// whole content is one Asset, id = path.
		if len(strings.TrimSpace(string(content))) == 0 {
			return nil, fmt.Errorf("%s: asset %q has no content", path, path)
		}
		return []*core.Asset{p.asset(path, content, kind, fm, path)}, nil
	}

	seen := make(map[string]struct{}, len(sections))
	var out []*core.Asset
	for _, s := range sections {
		if _, dup := seen[s.heading]; dup {
			return nil, fmt.Errorf(
				"%s: duplicate section heading %q (line %d); ids are keyed on the "+
					"heading, so two sections cannot share one", path, s.heading, s.line,
			)
		}
		seen[s.heading] = struct{}{}
		if len(strings.TrimSpace(string(s.body))) == 0 {
			return nil, fmt.Errorf("%s: section %q (line %d) has no content",
				path, s.heading, s.line)
		}

		id := path + SectionSeparator + escapeHeading(s.heading)
		out = append(out, p.asset(id, s.body, kind, fm,
			path+":"+strconv.Itoa(s.line)))
	}
	return out, nil
}

// asset builds one Asset from a file or section.
//
// content is a slice of raw, which is freshly read per file and never
// modified, so every Asset from one file can borrow it safely — the borrow
// contract is the producer's to uphold.
func (p *Pool) asset(id string, content []byte, kind knov1.Kind, fm frontMatter, sourceRef string) *core.Asset {
	return &core.Asset{
		Id:      id,
		Content: content,
		Kind:    kind,
		Tags:    fm.tags,
		// The file said which kind this is, so the report can tell an asserted
		// routing decision from a measured one.
		UserOverridden: fm.kind != "",
		Cost:           &knov1.CostVector{ContextTokens: contextTokens(len(content))},
		Provenance: &knov1.Provenance{
			Source:    "markdown",
			SourceRef: sourceRef,
			// IngestedAt is deliberately unset: it would make two reads of an
			// unchanged pool produce different Assets, so a pool that had not
			// moved would look changed on every read.
		},
	}
}

// Compile-time proof that this adapter satisfies the Ring-0 contract.
var _ core.Pool = (*Pool)(nil)

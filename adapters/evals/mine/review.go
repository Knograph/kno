package mine

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Review presents the unreviewed Cases for keep / edit / drop and records
// every decision in the manifest next to the output.
//
// Decisions are keyed by the id the SOURCE produces — the pre-edit id —
// so a decision made once is applied by every re-mine: a curated drop can
// never resurrect, and an edited expectation is re-applied when the
// transcripts grow. A case a prior review already decided is skipped: Mine
// already applied its decision. quit (Ctrl-D or `q`) keeps the remaining
// cases unreviewed and records no decisions for them.
//
// Callers must refuse to invoke Review without a terminal; the interactive
// loop reads commands from in and prints the prompts to out.
func Review(cases []Case, manifestPath string, in io.Reader, out io.Writer) ([]Case, *Manifest, error) {
	m, err := loadManifest(manifestPath)
	if err != nil {
		return nil, nil, err
	}

	sc := bufio.NewScanner(in)
	final := make([]Case, 0, len(cases))
	for _, c := range cases {
		key := c.ident.id()
		if d, ok := m.Decisions[key]; ok {
			// Already decided in a prior review; Mine already applied it.
			if d.Decision == "drop" {
				continue
			}
			final = append(final, c)
			continue
		}
		d, quit, err := promptOne(c, sc, out)
		if err != nil {
			return nil, nil, err
		}
		if quit {
			final = append(final, c)
			continue
		}
		m.Decisions[key] = d
		switch d.Decision {
		case "drop":
			continue
		case "edit":
			c.Expected = d.Expected
			c.reID()
			c.Note += "; edited in review"
			final = append(final, c)
		default: // keep
			final = append(final, c)
		}
	}
	if err := saveManifest(manifestPath, m); err != nil {
		return nil, nil, err
	}
	return final, m, nil
}

// promptOne prompts for one case's disposition.
//
// Commands: keep (or k, or empty), drop (d), edit (e, then the corrected
// expected on the next line), quit (q — the rest stay unreviewed). An
// unknown command keeps the case with a note, so a typo cannot silently
// drop anything. An empty edit keeps the case.
func promptOne(c Case, sc *bufio.Scanner, out io.Writer) (Decision, bool, error) {
	_, _ = fmt.Fprintf(out, "\n%s\n  input:    %s\n  expected: %s\n  from: %s\nkeep / edit / drop / quit [keep]: ",
		c.ID, c.Input, c.Expected, c.SourceRef)
	if !sc.Scan() {
		return Decision{}, true, sc.Err() // EOF reads as quit
	}
	line := strings.ToLower(strings.TrimSpace(sc.Text()))
	switch line {
	case "", "k", "keep":
		return Decision{Decision: "keep"}, false, nil
	case "d", "drop":
		return Decision{Decision: "drop"}, false, nil
	case "q", "quit", "exit":
		return Decision{}, true, nil
	case "e", "edit":
		_, _ = fmt.Fprintf(out, "  expected: ")
		if !sc.Scan() {
			return Decision{Decision: "keep"}, false, sc.Err() // EOF mid-edit keeps as-is
		}
		expected := strings.TrimSpace(sc.Text())
		if expected == "" {
			return Decision{Decision: "keep"}, false, nil
		}
		return Decision{Decision: "edit", Expected: expected}, false, nil
	default:
		_, _ = fmt.Fprintf(out, "  (unknown command %q; keeping)\n", line)
		return Decision{Decision: "keep"}, false, nil
	}
}

// loadManifest reads the review manifest, or returns an empty one when it
// has never been written.
func loadManifest(path string) (*Manifest, error) {
	if path == "" {
		return &Manifest{Decisions: map[string]Decision{}}, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // the manifest is an explicit user-supplied path beside the output: reading it is the contract
	if errors.Is(err, fs.ErrNotExist) {
		return &Manifest{Decisions: map[string]Decision{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading review manifest %s: %w", path, err)
	}
	m, err := decodeManifest(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m.Decisions == nil {
		m.Decisions = map[string]Decision{}
	}
	return &m, nil
}

// saveManifest writes the review manifest atomically: a torn manifest on a
// crashed run would re-id the review's whole history, and a rename makes
// that failure impossible.
func saveManifest(path string, m *Manifest) error {
	buf, err := encodeManifest(*m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".review-*")
	if err != nil {
		return fmt.Errorf("writing review manifest %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(buf); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing review manifest %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing review manifest %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("writing review manifest %s: %w", path, err)
	}
	return nil
}

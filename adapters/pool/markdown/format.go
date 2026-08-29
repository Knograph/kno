// This file holds the on-disk format: front matter, section splitting, the
// id separator rule, and the kind spelling, all pinned here so the iteration
// code in markdown.go can read them without knowing the format's details.

package markdown

import (
	"bytes"
	"fmt"
	"math"
	"strings"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// SectionSeparator joins a file path and a section heading into an Asset id.
//
// A heading containing the separator is escaped by DOUBLING it, so the map
// from (path, heading) to id is injective: two headings that differ produce
// two ids that differ, which is what makes duplicate-heading detection sound
// and what keeps ids stable while other parts of the file are edited. The
// id is never parsed back — it is compared and stored — so only injectivity
// is required of the escaping, and doubling provides it.
const SectionSeparator = "::"

// escapeHeading doubles every separator inside a heading. See
// SectionSeparator for why doubling is the escaping rule.
func escapeHeading(h string) string {
	return strings.ReplaceAll(h, SectionSeparator, SectionSeparator+SectionSeparator)
}

// frontMatter is the optional metadata block at the top of a file.
type frontMatter struct {
	kind string   // "" when absent
	tags []string // nil when absent
	// end is the byte offset just past the closing "---", i.e. where the
	// document's content begins.
	end int
}

// parseFrontMatter reads the optional `---` delimited block at the very top
// of the file, if the file starts with one.
//
// Deliberately minimal — "YAML-ish", not YAML: the block is a sequence of
// `key: value` lines, with blank lines and `#` comment lines allowed, and the
// only recognized keys are kind and tags (tags entries separated by
// semicolons, matching the csv adapter's TagsSeparator). An unknown key, a
// duplicated key, or a line that is not `key: value` is refused loudly rather
// than ignored — the same fail-closed contract as the jsonl adapter's
// unknown-field refusal: a key that decodes into nothing gives its author no
// error, no warning, and no data. A file whose first line is not `---` has no
// front matter at all, and its whole content is the document.
func parseFrontMatter(raw []byte) (frontMatter, error) {
	var fm frontMatter

	// The opener must be the file's first line.
	nl := bytes.IndexByte(raw, '\n')
	var first []byte
	if nl < 0 {
		first = raw
	} else {
		first = raw[:nl]
	}
	if !isDelim(first) {
		return fm, nil // no front matter
	}

	offset := nl + 1
	lineNo := 2
	seen := make(map[string]struct{})
	for {
		if offset >= len(raw) {
			return frontMatter{}, fmt.Errorf(
				"front matter is unterminated: the block starting at line 1 has no closing --- line",
			)
		}
		nl := bytes.IndexByte(raw[offset:], '\n')
		var line, rest []byte
		if nl < 0 {
			line, rest = raw[offset:], nil
		} else {
			line, rest = raw[offset:offset+nl], raw[offset+nl+1:]
		}

		trimmed := bytes.TrimSpace(line)
		if isDelim(trimmed) {
			if rest == nil {
				fm.end = len(raw)
			} else {
				fm.end = len(raw) - len(rest)
			}
			return fm, nil
		}
		if len(trimmed) == 0 || trimmed[0] == '#' {
			offset, lineNo = offset+len(line)+1, lineNo+1
			continue
		}

		key, val, ok := bytes.Cut(trimmed, []byte(":"))
		if !ok {
			return frontMatter{}, fmt.Errorf(
				"front matter line %d: %q is not a `key: value` line", lineNo, trimmed,
			)
		}
		k := string(bytes.TrimSpace(key))
		if k == "" {
			return frontMatter{}, fmt.Errorf("front matter line %d: empty key", lineNo)
		}
		if _, dup := seen[k]; dup {
			return frontMatter{}, fmt.Errorf("front matter line %d: duplicate key %q", lineNo, k)
		}
		seen[k] = struct{}{}

		v := string(bytes.TrimSpace(val))
		switch k {
		case "kind":
			fm.kind = v
		case "tags":
			fm.tags = parseTags(v)
		default:
			return frontMatter{}, fmt.Errorf(
				"front matter line %d: unknown key %q; the contract is kind and tags", lineNo, k,
			)
		}
		offset, lineNo = offset+len(line)+1, lineNo+1
	}
}

// isDelim reports whether a line is a front-matter delimiter. The opener and
// the closer follow the same rule, and surrounding whitespace is tolerated on
// both, so a `--- ` line does not silently stop being a delimiter.
func isDelim(line []byte) bool {
	return bytes.Equal(bytes.TrimSpace(line), []byte("---"))
}

// parseTags splits a tags value on semicolons.
//
// Entries are trimmed of surrounding whitespace, and empty entries (a
// trailing separator, or two separators in a row) are dropped rather than
// tagging an Asset with the empty string. The delimiter matches the csv
// adapter's TagsSeparator, so a pool split across both formats carries its
// tags the same way.
func parseTags(v string) []string {
	var tags []string
	for _, t := range strings.Split(v, ";") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// section is one `## `-level heading and its body.
type section struct {
	heading string // heading text, unescaped
	line    int    // 1-based physical line of the heading
	body    []byte // content lines after the heading, up to the next heading
}

// splitSections splits content on `## `-level ATX headings.
//
// A section begins at a line starting with exactly two hashes and a space
// (`## `). Deeper levels (`### `) and shallower ones (`# `) are content.
// Content before the first heading — the document's preamble — is not an
// Asset: it is the document's own introduction, not a candidate for a
// portfolio. Content after the last heading belongs to that section. A file
// with no `## ` headings yields one section whose heading is empty and whose
// body is the whole content.
func splitSections(content []byte) []section {
	var sections []section
	offset := 0
	lineNo := 1
	curHeading := ""
	curLine := 0
	bodyStart := -1 // the body begins just past the heading's newline

	for offset < len(content) {
		nl := bytes.IndexByte(content[offset:], '\n')
		var line []byte
		next := len(content)
		if nl < 0 {
			line = content[offset:]
		} else {
			line = content[offset : offset+nl]
			next = offset + nl + 1
		}

		if heading, ok := headingOf(line); ok {
			if bodyStart >= 0 {
				sections = append(sections, section{curHeading, curLine, content[bodyStart:offset]})
			}
			curHeading, curLine = heading, lineNo
			bodyStart = next
		}
		offset, lineNo = next, lineNo+1
	}
	if bodyStart >= 0 {
		sections = append(sections, section{curHeading, curLine, content[bodyStart:]})
	} else {
		sections = append(sections, section{"", 1, content})
	}
	return sections
}

// headingOf reports whether line begins a `## `-level section and returns the
// heading text (the rest of the line, trimmed).
func headingOf(line []byte) (string, bool) {
	if !bytes.HasPrefix(line, []byte("## ")) {
		return "", false
	}
	return strings.TrimSpace(string(line[3:])), true
}

// kindOf maps the file's spelling of a Kind onto the enum.
//
// Exact, lower-case, and closed, with the same contract as the jsonl
// adapter's: an unrecognized spelling is refused rather than defaulted,
// because the default is KIND_UNSPECIFIED and that enum's own proto comment
// warns a silent zero "would read as knowledge and route the Asset to the
// wrong destination". Quoted spellings (`kind: "knowledge"`) are not
// unquoted — this is a minimal parse, not YAML, and the refusal is loud.
func kindOf(s string) (knov1.Kind, error) {
	switch s {
	case "":
		return knov1.Kind_KIND_UNSPECIFIED, nil
	case "knowledge":
		return knov1.Kind_KIND_KNOWLEDGE, nil
	case "behavior":
		return knov1.Kind_KIND_BEHAVIOR, nil
	default:
		return knov1.Kind_KIND_UNSPECIFIED, fmt.Errorf(
			`unknown kind %q; write "knowledge" or "behavior", or omit the field `+
				`and let routing judge it`, s,
		)
	}
}

// bytesPerToken is the divisor behind contextTokens. See its godoc for why it
// is this number and not the one the reservation path uses.
const bytesPerToken = 3.6

// contextTokens estimates what carrying this Asset adds to every request.
//
// This is the RANKING denominator of delta_per_cost, and it must not be the
// reservation path's countTokens (adapters/agent/pricing): that deliberately
// over-counts by about 3x on prose and takes a model argument, so feeding it
// in here would rank the portfolio by content type instead of by value
// (docs/debt.md#68). Bytes over a fixed divisor, centered on the one
// measurement in this tree — English prose at 3.6 bytes/token. Rounded up, so
// a non-empty Asset never costs zero tokens: delta_per_cost over a zero
// denominator is an infinity, and an infinity sorts to the top of a greedy
// ranking.
func contextTokens(sizeBytes int) int64 {
	return int64(math.Ceil(float64(sizeBytes) / bytesPerToken))
}

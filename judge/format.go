// This file is the only place in judge/ that encodes or decodes JSON, and the
// only reason the package carries an encoding/json depguard exemption.
//
// The exemption is the same one adapters/**/format.go holds, for the same
// reason: these are hand-written on-disk shapes decoded into plain Go structs,
// no kno.v1 type is ever handed to encoding/json, and protojson would force
// the file format to mirror proto field names and presence rules. The
// conversion into core.Case and core.Response happens below, after decoding,
// so a proto message never crosses the marshaler.

package judge

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// manifestFile is <set>/manifest.json.
type manifestFile struct {
	Name          string   `json:"name"`
	Version       int      `json:"version"`
	ContentSHA256 string   `json:"content_sha256"`
	Labelers      []string `json:"labelers"`
	Description   string   `json:"description"`
	ClassBalance  string   `json:"class_balance"`
	License       string   `json:"license"`
}

// recordFile is one line of <set>/records.jsonl.
type recordFile struct {
	ID          string         `json:"id"`
	Case        caseFile       `json:"case"`
	Response    responseFile   `json:"response"`
	Labels      []labelFile    `json:"labels"`
	Adjudicated labelFile      `json:"adjudicated"`
	Provenance  provenanceFile `json:"provenance"`
}

type caseFile struct {
	Input    string   `json:"input"`
	Expected string   `json:"expected,omitempty"`
	Rubric   string   `json:"rubric,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

type responseFile struct {
	Output string `json:"output"`
}

type labelFile struct {
	LabelerID string  `json:"labeler_id"`
	Value     float64 `json:"value"`
	Passed    bool    `json:"passed"`
	Note      string  `json:"note,omitempty"`
}

type provenanceFile struct {
	Source  string `json:"source"`
	License string `json:"license,omitempty"`
	Note    string `json:"note,omitempty"`
}

// fixtureFile is one recorded judge response, keyed by prompt sha and record.
type fixtureFile struct {
	RecordID   string  `json:"record_id"`
	PromptSHA  string  `json:"prompt_sha"`
	JudgeModel string  `json:"judge_model"`
	Value      float64 `json:"value"`
	Passed     bool    `json:"passed"`
	Rationale  string  `json:"rationale,omitempty"`
	Error      string  `json:"error,omitempty"`
}

// baselineFile is judge/calibration.baseline.json.
type baselineFile struct {
	Note    string              `json:"_note"`
	Entries []baselineEntryFile `json:"entries"`
}

type baselineEntryFile struct {
	SetName       string  `json:"set_name"`
	SetVersion    int     `json:"set_version"`
	ContentSHA256 string  `json:"content_sha256"`
	GoalName      string  `json:"goal_name"`
	PromptSHA     string  `json:"prompt_sha"`
	JudgeModel    string  `json:"judge_model,omitempty"`
	Kappa         float64 `json:"kappa"`
	NRecords      int     `json:"n_records"`

	// Verdicts is one character per record, in the set's file order: '1' for
	// a pass, '0' for a fail, '-' for a record the judge errored on.
	//
	// The per-record vector, not just the scalar kappa, because the ratchet is
	// PAIRED: both runs judge the identical records, and an unpaired
	// comparison of two independent intervals throws that away and is far too
	// permissive. A scalar cannot be paired with anything.
	Verdicts string `json:"verdicts"`
}

// decodeManifest reads a manifest.
func decodeManifest(b []byte) (manifestFile, error) {
	var m manifestFile
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("parsing manifest.json: %w", err)
	}
	return m, nil
}

// decodeRecords reads records.jsonl and returns the raw lines' hash alongside.
//
// The hash is computed over the bytes as read rather than over a re-encoding,
// so a whitespace edit that changes nothing semantically still changes the
// hash — which is the point: the manifest attests to the FILE, and a gate that
// only noticed semantic edits could be routed around by a formatter.
func decodeRecords(r io.Reader) ([]recordFile, string, error) {
	sum := sha256.New()
	sc := bufio.NewScanner(io.TeeReader(r, sum))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var out []recordFile
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "//") {
			continue
		}
		var rec recordFile
		if err := json.Unmarshal([]byte(text), &rec); err != nil {
			return nil, "", fmt.Errorf("records.jsonl line %d: %w", line, err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, "", fmt.Errorf("reading records.jsonl: %w", err)
	}
	return out, hex.EncodeToString(sum.Sum(nil)), nil
}

// toRecord converts a decoded line into the in-memory record.
func toRecord(f recordFile) Record {
	labels := make([]HumanLabel, 0, len(f.Labels))
	for _, l := range f.Labels {
		labels = append(labels, toLabel(l))
	}
	return Record{
		ID: f.ID,
		Case: &knov1.Case{
			Id:       f.ID,
			Input:    f.Case.Input,
			Expected: f.Case.Expected,
			Rubric:   f.Case.Rubric,
			Tags:     f.Case.Tags,
		},
		Response: &knov1.Response{
			CaseId: f.ID,
			Output: f.Response.Output,
		},
		Labels:      labels,
		Adjudicated: toLabel(f.Adjudicated),
		Provenance: Provenance{
			Source:  f.Provenance.Source,
			License: f.Provenance.License,
			Note:    f.Provenance.Note,
		},
	}
}

// toLabel converts the on-disk label into the in-memory one.
//
// A CONVERSION, not a field-by-field copy: the two types are field-identical
// today, and the day the file format grows a field the domain type does not
// want, this stops compiling — which is the signal. The types stay separate so
// the format can diverge; the conversion is what makes the divergence loud.
func toLabel(l labelFile) HumanLabel {
	return HumanLabel(l)
}

// decodeFixture reads one recorded judge response.
func decodeFixture(b []byte) (fixtureFile, error) {
	var f fixtureFile
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("parsing fixture: %w", err)
	}
	return f, nil
}

// decodeBaseline reads the committed baseline.
func decodeBaseline(b []byte) (baselineFile, error) {
	var f baselineFile
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("parsing the calibration baseline: %w", err)
	}
	return f, nil
}

// EncodeBaseline renders a baseline file.
//
// Exported because `make record-calibration` regenerates it and the diff is
// reviewed like code — which requires the encoding to be stable and indented,
// not compact.
func EncodeBaseline(entries []BaselineEntry) ([]byte, error) {
	f := baselineFile{
		Note: "Regenerate with `make record-calibration`. Review the diff like code: " +
			"lowering a recorded kappa is a deliberate trade and the PR must say what was traded.",
		Entries: make([]baselineEntryFile, 0, len(entries)),
	}
	for _, e := range entries {
		f.Entries = append(f.Entries, baselineEntryFile(e))
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding the calibration baseline: %w", err)
	}
	return append(b, '\n'), nil
}

// readFile reads one path out of an fs.FS with a useful error.
func readFile(fsys fs.FS, dir, name string) ([]byte, error) {
	b, err := fs.ReadFile(fsys, path.Join(dir, name))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path.Join(dir, name), err)
	}
	return b, nil
}

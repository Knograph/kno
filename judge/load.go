package judge

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/knograph/kno/core/errs"
)

// The set invariants. Each one is refused at LOAD time rather than reported
// afterwards, because a statistic computed over a set that breaks one of them
// is a number nobody should see: it would be read as an answer.
const (
	// MinMinorityShare is the balance invariant: the smaller class must be at
	// least this fraction of the set.
	//
	// This is the mitigation for the kappa paradox, applied at its cause. The
	// alternative — reporting a prevalence-adjusted statistic on a lopsided
	// set — produces a flattering number and removes the pressure to fix the
	// set. Balance is a property we control at authoring time, so it is
	// enforced at authoring time. See docs/what-the-numbers-mean.md for the
	// table showing what the identity the floor rests on costs as balance
	// degrades.
	MinMinorityShare = 0.40

	// MinLabelsPerRecord is how many independent human labels a record needs.
	MinLabelsPerRecord = 2

	// MinRecords is the smallest set an interval can be computed over.
	MinRecords = 2
)

// Load reads a calibration set from a directory on disk.
func Load(dir string) (*Set, error) {
	clean := filepath.Clean(dir)
	parent, base := filepath.Dir(clean), filepath.Base(clean)
	set, err := LoadFS(os.DirFS(parent), base)
	if err != nil {
		return nil, err
	}
	set.Source = clean
	return set, nil
}

// LoadFS reads a calibration set from dir inside fsys.
//
// Both entry points exist because the committed set is EMBEDDED in the binary
// — a contributor with no checkout still gets `kno judge calibrate` — while a
// set under review is a directory. One loader, so an embedded set and a
// working-tree set cannot be validated by two different rules.
func LoadFS(fsys fs.FS, dir string) (*Set, error) {
	manifestBytes, err := readFile(fsys, dir, "manifest.json")
	if err != nil {
		return nil, errs.ErrInvalidInput.
			WithFix("a calibration set is a directory holding manifest.json and " +
				"records.jsonl; check the path passed to --set").
			Wrap(err)
	}
	m, err := decodeManifest(manifestBytes)
	if err != nil {
		return nil, errs.ErrInvalidInput.WithFix("fix the JSON in manifest.json").Wrap(err)
	}

	recordBytes, err := readFile(fsys, dir, "records.jsonl")
	if err != nil {
		return nil, errs.ErrInvalidInput.
			WithFix("a calibration set needs records.jsonl beside its manifest").
			Wrap(err)
	}
	lines, sum, err := decodeRecords(bytes.NewReader(recordBytes))
	if err != nil {
		return nil, errs.ErrInvalidInput.WithFix("fix the malformed line").Wrap(err)
	}

	set := &Set{
		Name:          m.Name,
		Version:       m.Version,
		ContentSHA256: sum,
		Labelers:      m.Labelers,
		Description:   m.Description,
		Source:        path.Join(dir),
	}
	for _, l := range lines {
		set.Records = append(set.Records, toRecord(l))
	}

	if err := validateSet(set, m); err != nil {
		return nil, err
	}
	return set, nil
}

// validateSet enforces every invariant, in the order a reader would want them:
// the file first, then the records, then the shape of the set as a whole.
func validateSet(set *Set, m manifestFile) error {
	if m.Name == "" || m.Version < 1 {
		return errs.ErrInvalidInput.
			WithFix("manifest.json needs a name and a version >= 1: the baseline is keyed " +
				"(set_name, set_version, goal_name) so a set edit and a prompt edit " +
				"cannot be confused for each other").
			Wrap(fmt.Errorf("manifest names %q at version %d", m.Name, m.Version))
	}
	if m.ContentSHA256 != set.ContentSHA256 {
		return errs.ErrInvalidInput.
			WithFix("regenerate the manifest's content_sha256 in the same commit that " +
				"edits records.jsonl: `make update-calibration-manifest`").
			Wrap(fmt.Errorf("records.jsonl hashes to %s; manifest.json attests to %s",
				short(set.ContentSHA256), short(m.ContentSHA256)))
	}
	if len(set.Records) < MinRecords {
		return errs.ErrInvalidInput.
			WithFix(fmt.Sprintf("a calibration set needs at least %d records; "+
				"no interval is computable over fewer", MinRecords)).
			Wrap(fmt.Errorf("set %q holds %d record(s)", set.Name, len(set.Records)))
	}
	if len(set.Labelers) < MinLabelsPerRecord {
		return errs.ErrInvalidInput.
			WithFix("manifest.json must list the labeler roster; the inter-human ceiling " +
				"is only readable if a reader can see how many people it rests on").
			Wrap(fmt.Errorf("set %q lists %d labeler(s)", set.Name, len(set.Labelers)))
	}

	seen := map[string]struct{}{}
	for i, r := range set.Records {
		if err := validateRecord(i, r, seen); err != nil {
			return err
		}
	}

	if share := set.MinorityShare(); share < MinMinorityShare {
		return errs.ErrInvalidInput.
			WithFix(fmt.Sprintf("balance the set: the minority class must be at least "+
				"%.0f%% of records. Kappa is depressed by extreme prevalence, and this "+
				"set is corrected at authoring time rather than compensated for "+
				"afterwards", MinMinorityShare*100)).
			Wrap(fmt.Errorf("set %q has a minority class of %.1f%% over %d records",
				set.Name, share*100, len(set.Records)))
	}
	return nil
}

// validateRecord enforces the per-record invariants.
func validateRecord(i int, r Record, seen map[string]struct{}) error {
	where := fmt.Sprintf("record %d", i)
	if r.ID == "" {
		return errs.ErrInvalidInput.
			WithFix("every record needs a stable id: the baseline's verdict vector, the " +
				"disagreement table and every issue about this set reference it").
			Wrap(fmt.Errorf("%s has no id", where))
	}
	where = "record " + r.ID
	if _, dup := seen[r.ID]; dup {
		return errs.ErrInvalidInput.
			WithFix("record ids must be unique; a duplicate is counted twice by every statistic").
			Wrap(fmt.Errorf("%s appears more than once", where))
	}
	seen[r.ID] = struct{}{}

	switch r.Provenance.Source {
	case SourceAuthored, SourceSynthetic:
	default:
		return errs.ErrInvalidInput.
			WithFix("provenance.source must be \"authored\" or \"synthetic\". The set is " +
				"public and permanent, and traces are customer data: a record derived " +
				"from a real deployment has no spelling here").
			Wrap(fmt.Errorf("%s declares provenance %q", where, r.Provenance.Source))
	}
	if len(r.Labels) < MinLabelsPerRecord {
		return errs.ErrInvalidInput.
			WithFix(fmt.Sprintf("every record needs at least %d independent labels; a "+
				"record with one is a record labeled by one person's judgement, which "+
				"is exactly what the set exists to hold a judge to", MinLabelsPerRecord)).
			Wrap(fmt.Errorf("%s carries %d label(s)", where, len(r.Labels)))
	}
	labelers := map[string]struct{}{}
	for _, l := range r.Labels {
		if l.LabelerID == "" {
			return errs.ErrInvalidInput.
				WithFix("every label names its labeler, pseudonymously").
				Wrap(fmt.Errorf("%s has an unattributed label", where))
		}
		if _, dup := labelers[l.LabelerID]; dup {
			return errs.ErrInvalidInput.
				WithFix("labels must be INDEPENDENT: two labels from one person are one " +
					"opinion counted twice, and they inflate the inter-human ceiling " +
					"the judge is measured against").
				Wrap(fmt.Errorf("%s carries two labels from %s", where, l.LabelerID))
		}
		labelers[l.LabelerID] = struct{}{}
	}
	if r.Adjudicated.LabelerID == "" {
		return errs.ErrInvalidInput.
			WithFix("every record needs an adjudicated reference verdict. The set may not " +
				"contain an unresolved disagreement: a judge cannot be measured against " +
				"labels that do not say what is true").
			Wrap(fmt.Errorf("%s has no adjudicated verdict", where))
	}
	if r.Response.GetOutput() == "" {
		return errs.ErrInvalidInput.
			WithFix("every record carries the agent output being judged; Goal.Score takes " +
				"a Case AND a Response").
			Wrap(fmt.Errorf("%s has no response output", where))
	}
	return nil
}

func short(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

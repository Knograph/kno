package judge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
)

// NoPromptSHA is the prompt hash of a Goal that has no prompt.
//
// A sentinel rather than the hash of the empty string, so a reader of the
// baseline file can tell "this Goal is arithmetic" from "this Goal's prompts
// hashed to something and the something is short".
const NoPromptSHA = "no-prompt"

// PromptSource is one file of prompt text a judged Goal is assembled from.
type PromptSource struct {
	// Name is a stable identifier — usually the file's path relative to the
	// prompt directory. It is hashed alongside the text so that renaming a
	// prompt file changes the sha.
	Name string

	// Text is the prompt as sent.
	Text []byte
}

// Prompted is a Goal assembled from prompt text.
//
// Optional, like every capability in ring 0. A Goal that does not implement it
// has no prompt to change, so nothing about it can regress by a prompt edit
// and the replay gate has nothing to key on.
type Prompted interface {
	// PromptSources returns every piece of text the Goal's judgement depends
	// on. Order does not matter; the hash sorts.
	PromptSources() []PromptSource
}

// Costed is a Goal that can say what scoring one Case will cost.
//
// A Goal implementing this is a SPEND PATH, and `kno judge calibrate --live`
// runs every Score through the budget guard on the strength of it. A Goal that
// spends and does not implement it cannot be calibrated live — which is the
// same posture core.Estimator takes, and for the same reason: a cost the guard
// learns at settlement is a cap discovered after the money is gone.
type Costed interface {
	// EstimateScore reports the most one Score of c could cost. It must be
	// local arithmetic over a price table and must not call the provider.
	EstimateScore(ctx context.Context, c *core.Case) (budget.Estimate, error)
}

// PromptSHA is the identity of a Goal's prompts.
//
// A prompt change is detected BY HASH, not by path. Path-based detection fires
// on a whitespace edit to a file the prompt does not use and misses a prompt
// assembled from a file outside judge/ — both of which are how a gate comes to
// be ignored.
func PromptSHA(g core.Goal) string {
	p, ok := g.(Prompted)
	if !ok {
		return NoPromptSHA
	}
	sources := p.PromptSources()
	if len(sources) == 0 {
		return NoPromptSHA
	}
	sorted := make([]PromptSource, len(sources))
	copy(sorted, sources)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	h := sha256.New()
	for _, s := range sorted {
		// Length-prefixed so that two sources cannot be concatenated into a
		// third that hashes the same.
		// hash.Hash never returns an error, and its own godoc says so.
		fmt.Fprintf(h, "%d:%s\n%d:", len(s.Name), s.Name, len(s.Text)) //nolint:errcheck // hash.Hash never errors
		h.Write(s.Text)
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FixtureStore reads recorded judge responses.
//
// PR CI never calls a provider — `make test-integration` hard-fails when
// KNO_LIVE_TESTS is set — so calibration in CI is a replay against these.
type FixtureStore struct {
	fsys fs.FS
	root string
}

// NewFixtureStore reads fixtures from a directory on disk.
func NewFixtureStore(dir string) *FixtureStore {
	clean := filepath.Clean(dir)
	return &FixtureStore{fsys: os.DirFS(clean), root: clean}
}

// Score returns the recorded judgement for one record.
//
// A MISSING fixture is an error naming the record, never a skip. A partial
// replay would compute kappa over a subset chosen by which fixtures happened
// to exist, and the number would look exactly like a real one.
func (s *FixtureStore) Score(goalName, promptSHA, recordID string) (*core.Score, error) {
	dir := path.Join(goalName, promptSHA)
	b, err := fs.ReadFile(s.fsys, path.Join(dir, recordID+".json"))
	if err != nil {
		if _, statErr := fs.Stat(s.fsys, dir); statErr != nil {
			return nil, errs.ErrInvalidInput.
				WithFix("run `make record-calibration` to record this prompt's judge " +
					"responses, then commit them with the regenerated baseline").
				Wrap(fmt.Errorf("no recorded judge responses for prompt %s", short(promptSHA)))
		}
		return nil, errs.ErrInvalidInput.
			WithFix("run `make record-calibration`: a partial replay would compute a " +
				"statistic over whichever records happened to have fixtures").
			Wrap(fmt.Errorf("no recorded judge response for record %s at prompt %s",
				recordID, short(promptSHA)))
	}
	f, err := decodeFixture(b)
	if err != nil {
		return nil, errs.ErrInvalidInput.WithFix("re-record this fixture").Wrap(err)
	}
	if f.Error != "" {
		return nil, fmt.Errorf("recorded judge error for %s: %s", recordID, f.Error)
	}
	return &knov1.Score{
		CaseId:     recordID,
		Value:      f.Value,
		Passed:     f.Passed,
		Rationale:  f.Rationale,
		JudgeModel: f.JudgeModel,
	}, nil
}

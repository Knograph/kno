package core_test

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// fakeEvals yields a fixed set of Cases, whatever their split.
type fakeEvals struct {
	cases   []*core.Case
	openErr error
	yieldAt int // yield a fatal error at this index; -1 for never
}

func (f *fakeEvals) Cases(ctx context.Context) (iter.Seq2[*core.Case, error], error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return func(yield func(*core.Case, error) bool) {
		for i, c := range f.cases {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			if i == f.yieldAt {
				yield(nil, errFatal)
				return
			}
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}

var errFatal = errors.New("fake: fatal read error")

func mixedCases() []*core.Case {
	return []*core.Case{
		{Id: "dev-1", Split: knov1.Split_SPLIT_DEV},
		{Id: "holdout-1", Split: knov1.Split_SPLIT_HOLDOUT},
		{Id: "dev-2", Split: knov1.Split_SPLIT_DEV},
		{Id: "holdout-2", Split: knov1.Split_SPLIT_HOLDOUT},
		{Id: "unassigned", Split: knov1.Split_SPLIT_UNSPECIFIED},
	}
}

// TestSealYieldsOnlyDevCases is the executable form of prime directive 5.
//
// Every statistical claim this tool makes rests on nothing reading a holdout
// Case before Validate. The type makes a bypass a compile error; this proves
// the filtering itself is right.
func TestSealYieldsOnlyDevCases(t *testing.T) {
	t.Parallel()

	sealed := core.Seal(&fakeEvals{cases: mixedCases(), yieldAt: -1})
	seq, err := sealed.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}

	var got []string
	for c, err := range seq {
		if err != nil {
			t.Fatalf("iterating: %v", err)
		}
		if c.GetSplit() == knov1.Split_SPLIT_HOLDOUT {
			t.Errorf("HOLDOUT LEAK: case %s reached a sealed consumer", c.GetId())
		}
		got = append(got, c.GetId())
	}

	want := []string{"dev-1", "dev-2"}
	if len(got) != len(want) {
		t.Fatalf("yielded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("yielded %v, want %v", got, want)
			break
		}
	}
}

// TestUnassignedSplitIsNotTreatedAsDev covers the quiet direction of the leak.
//
// A Case with no split has not been through ingestion. Treating "unknown" as
// "dev" is how a holdout leaks one Case at a time — and always in the direction
// that inflates the reported gain, because the extra Cases are the ones nobody
// decided to hold back.
func TestUnassignedSplitIsNotTreatedAsDev(t *testing.T) {
	t.Parallel()

	sealed := core.Seal(&fakeEvals{
		cases:   []*core.Case{{Id: "unassigned", Split: knov1.Split_SPLIT_UNSPECIFIED}},
		yieldAt: -1,
	})
	seq, err := sealed.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}

	for c, err := range seq {
		if err != nil {
			t.Fatalf("iterating: %v", err)
		}
		t.Errorf("a Case with no assigned split was yielded as dev: %s", c.GetId())
	}
}

// TestSealPreservesTheFatalErrorContract: filtering must not swallow an error
// or continue past one.
func TestSealPreservesTheFatalErrorContract(t *testing.T) {
	t.Parallel()

	sealed := core.Seal(&fakeEvals{cases: mixedCases(), yieldAt: 2})
	seq, err := sealed.Cases(context.Background())
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}

	var seen int
	var gotErr error
	for _, err := range seq {
		if err != nil {
			gotErr = err
			break
		}
		seen++
	}

	if !errors.Is(gotErr, errFatal) {
		t.Errorf("error = %v, want the underlying fatal error to pass through", gotErr)
	}
	if seen != 1 {
		t.Errorf("yielded %d cases before the error, want 1 (dev-1)", seen)
	}
}

// TestSealPropagatesOpenErrors: a source that cannot be opened must fail when
// the iterator is requested, not silently yield nothing — which would look
// exactly like an eval set with no dev Cases.
func TestSealPropagatesOpenErrors(t *testing.T) {
	t.Parallel()

	want := errors.New("cannot open")
	sealed := core.Seal(&fakeEvals{openErr: want, yieldAt: -1})

	if _, err := sealed.Cases(context.Background()); !errors.Is(err, want) {
		t.Errorf("error = %v, want the open error", err)
	}
}

// TestSealHonorsCancellation keeps a cancelled run from continuing to read.
func TestSealHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sealed := core.Seal(&fakeEvals{cases: mixedCases(), yieldAt: -1})
	seq, err := sealed.Cases(ctx)
	if err != nil {
		t.Fatalf("Cases: %v", err)
	}

	for c, err := range seq {
		if err == nil {
			t.Errorf("yielded case %s after the context was cancelled", c.GetId())
		}
		break
	}
}

// TestSealedEvalsSatisfiesEvals means a sealed source composes anywhere a
// plain one does — while the reverse stays impossible, which is the asymmetry
// the whole design rests on.
func TestSealedEvalsSatisfiesEvals(t *testing.T) {
	t.Parallel()

	var e core.Evals = core.Seal(&fakeEvals{cases: mixedCases(), yieldAt: -1})
	if _, err := e.Cases(context.Background()); err != nil {
		t.Fatalf("a sealed source did not work as an Evals: %v", err)
	}
}

// TestNilSourceIsRefused rather than panicking deep inside a run.
func TestNilSourceIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := core.Seal(nil).Cases(context.Background()); err == nil {
		t.Error("a seal over a nil source was accepted")
	}
}

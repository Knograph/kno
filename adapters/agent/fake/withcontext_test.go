package fake_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/fake"
	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
)

// TestWithContextRefusesAnEmptyAsset closes the asymmetry that let a real
// defect ship.
//
// WithContextSet has always refused an empty set. WithContext accepted an
// empty Asset, which made this the only ContextInjector in the tree that
// would — every real one refuses it, because an Agent carrying no content IS
// the control arm, and measuring it as the treatment arm reports a paired
// difference of exactly zero that is not a measurement.
//
// The gap was load-bearing rather than cosmetic. core/value_loop.go built its
// treatment arm from a content-free &Asset{Id: ...} and shipped that way,
// because the only adapter its tests ran against was the permissive one. A
// test double more permissive than every adapter it stands in for cannot fail
// where they would.
func TestWithContextRefusesAnEmptyAsset(t *testing.T) {
	t.Parallel()

	a := fake.New(fake.Options{})

	for _, tc := range []struct {
		name  string
		asset *core.Asset
	}{
		{"no content field at all", &core.Asset{Id: "a1"}},
		{"explicitly empty content", &core.Asset{Id: "a1", Content: []byte{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := a.WithContext(tc.asset)
			if err == nil {
				t.Fatalf("WithContext accepted an Asset with no content and returned %v; "+
					"the treatment arm it builds is byte-identical to the control's, so "+
					"every paired difference is exactly zero and the report reads "+
					"\"measured, and inert\" for an Asset that was never measured", got)
			}
			if !errors.Is(err, errs.ErrInvalidInput) {
				t.Errorf("the refusal is not an Actionable: %v", err)
			}
			if !strings.Contains(err.Error(), "a1") {
				t.Errorf("the refusal does not name the Asset: %v", err)
			}
		})
	}

	// A guard that always fires is not a guard.
	t.Run("an Asset that carries content is accepted", func(t *testing.T) {
		t.Parallel()

		if _, err := a.WithContext(&core.Asset{Id: "a1", Content: []byte("real")}); err != nil {
			t.Errorf("WithContext refused an Asset that carries content: %v", err)
		}
	})
}

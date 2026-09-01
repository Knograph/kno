package judge

import (
	"embed"
	"io/fs"
	"path"
	"sort"
)

// The committed calibration sets, embedded so `kno judge calibrate` works from
// an installed binary with no checkout, no API key and no network. That is the
// contributor on-ramp this command exists to open: a person who has never
// cloned the repository can still see what a judge gets wrong.
//
// They live under testdata/ rather than beside the code because that is where
// this repository keeps deterministic inputs, and because `make check` reaches
// them there. They deliberately do NOT live in examples/: DESIGN.md said they
// would, but examples/ is a sibling repository, and a gate whose input lives
// in another repository cannot fail a pull request here.
//
//go:embed testdata/calibration
var builtinFS embed.FS

const builtinRoot = "testdata/calibration"

// DefaultSetName is the set `kno judge calibrate` uses when none is named.
const DefaultSetName = "starter"

// BuiltinSets lists the embedded calibration sets by name.
func BuiltinSets() []string {
	entries, err := fs.ReadDir(builtinFS, builtinRoot)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Builtin loads an embedded calibration set by name.
//
// It goes through the same LoadFS as a set on disk, so the committed set is
// validated by exactly the rules a contributed one is. A built binary carrying
// a set that would be refused from a directory is a gate that passes because
// of where its input lives.
func Builtin(name string) (*Set, error) {
	set, err := LoadFS(builtinFS, path.Join(builtinRoot, name))
	if err != nil {
		return nil, err
	}
	set.Source = "embedded:" + path.Join(builtinRoot, name)
	return set, nil
}

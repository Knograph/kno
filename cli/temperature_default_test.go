package cli_test

import (
	"math"
	"testing"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/cli"
)

// TestUnsetTemperatureIsUnsetEverywhere is a cross-stage consistency test, and
// it exists because the stages disagreed.
//
// `--temperature`'s help says "unset leaves the provider default" on every
// command that has it. That was true only of baseline, which defaulted the flag
// to NaN; value and validate defaulted it to 0, and `optionalFloat` returns nil
// only for NaN — so both sent an explicit temperature=0 on every run.
//
// Two consequences, and the second is the one that matters. Visibly, a model
// that rejects sampling parameters became unusable in value and validate, with
// a refusal telling the user to "drop --temperature" — a flag they never
// passed. Silently, and much worse: baseline measured a model at the provider's
// default temperature while value and validate measured the SAME model at 0.
// Baseline is the reference every later measurement is compared against, so a
// delta between them carried a sampling difference attributed to the Asset.
//
// Asserted over every command that declares the flag rather than the three
// known today, so a fourth stage cannot reintroduce the divergence.
func TestUnsetTemperatureIsUnsetEverywhere(t *testing.T) {
	t.Parallel()

	root := cli.NewRootCmd()

	var checked int
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if f := c.Flags().Lookup("temperature"); f != nil {
			checked++
			got, err := c.Flags().GetFloat64("temperature")
			if err != nil {
				t.Fatalf("%s: reading --temperature: %v", c.Name(), err)
			}
			if !math.IsNaN(got) {
				t.Errorf("%s: --temperature defaults to %v, not NaN. The help says "+
					"\"unset leaves the provider default\", and optionalFloat treats "+
					"only NaN as unset — so this command sends an explicit "+
					"temperature the user never asked for, and measures the model "+
					"differently from the stages that do not",
					c.Name(), got)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)

	if checked == 0 {
		t.Fatal("no command declared --temperature; this test is asserting nothing")
	}
}

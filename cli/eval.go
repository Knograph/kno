package cli

import (
	"github.com/spf13/cobra"
)

// `kno eval` is the namespace for commands that read an eval set without
// measuring anything with it.
//
// The first two-level command in the tree, and chosen deliberately rather than
// by drift. docs/evaluation-design.md already published the name
// `kno eval inspect` in its closing section, and a shipped doc naming a
// command is a promise in the same way ErrCapabilityUnsupported's fix line
// was a promise that `kno doctor` had to keep. The namespace also has obvious
// future members — `kno eval split`, `kno eval diff` — so `kno inspect` at the
// root would be the shape that needs renaming later.
//
// The parent runs nothing. A bare `kno eval` prints its help and exits 0,
// which is cobra's default for a command with subcommands and no Run.

// newEvalCmd builds the eval namespace.
func newEvalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Read and check an eval set without spending anything",
		Long: `Commands that read an eval set and report on it.

Nothing here calls a model, creates a Run, or writes to the database. A
remote eval source still reaches its vendor's API with the vendor's
credentials, because reading the dataset is the job — "costs nothing" is a
claim about LLM spend.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newEvalInspectCmd())
	return cmd
}

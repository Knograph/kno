package cli

import "github.com/spf13/cobra"

// `kno eval` is the eval-set namespace: commands that read an Evals source and
// say something about it, without running an agent against it.
//
// The first two-level command in the tree, and chosen deliberately.
// docs/evaluation-design.md published the name `kno eval inspect` before the
// command existed, and a shipped doc naming a command is a promise in the same
// way errs.ErrCapabilityUnsupported's fix line was a promise `kno doctor` had
// to keep. The namespace also has obvious future members — `kno eval split`,
// `kno eval diff` — so `kno inspect` at the root would be the shape that needs
// renaming later.

// newEvalCmd builds the eval namespace and its children.
func newEvalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Read an eval set and report what it can support",
		Long: `Commands that read an Evals source without running anything against it.

Nothing under ` + "`kno eval`" + ` invokes an agent, scores a Case, or creates a Run.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		// A parent with no RunE prints its own help, which is the right
		// answer for a namespace: `kno eval` alone names no operation.
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newEvalInspectCmd())
	return cmd
}

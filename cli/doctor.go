package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/knograph/kno/adapters/agent/agentref"
	"github.com/knograph/kno/adapters/agent/pricing"
)

// `kno doctor` prints what this build can actually do.
//
// The name is not a choice. errs.ErrCapabilityUnsupported's fix line has always
// said "run `kno doctor` to print the capability matrix", and no such command
// existed — so every capability refusal pointed the user at nothing. A fix line
// is a promise.
//
// Read-only and free: it constructs no transport, resolves no credential, and
// makes no request. Someone diagnosing a misconfiguration should not have to
// risk a bill to ask what is supported.

// newDoctorCmd builds the diagnostic command.
// doctorVersion is what `doctor --json` reports.
//
// A named function rather than an inline field so the jq contract has something
// to assert against — see TestDoctorReportsTheBareVersionNotTheHumanString. The
// BARE version: `kno --version` is where the commit and date belong, and a
// parenthetical here breaks every consumer that pins or greps this field.
func doctorVersion() string { return identity().Version }

func newDoctorCmd() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Print what this build supports",
		Long: `Print the adapters, goals, and generation parameters this build supports,
and where its price table came from.

Nothing here contacts a provider or reads a credential, so it is safe to run
while diagnosing a run that failed.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd.OutOrStdout(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "machine-readable output")
	return cmd
}

// adapterFact is one row of the matrix.
//
// Hand-written, for the reason adapterFacts explains: building an Agent to ask
// it requires a credential and a destination, which are the two things a user
// running `kno doctor` is usually missing. The cost of that is drift, and the
// mitigation is the test that checks each claim against the adapters.
type adapterFact struct {
	Scheme           string `json:"scheme"`
	Available        bool   `json:"available"`
	GenerationParams string `json:"generation_params"`
	TokenCounts      bool   `json:"token_counts"`
	Spends           bool   `json:"spends"`
	Note             string `json:"note,omitempty"`
}

// adapterFacts describes every scheme the agent-ref parser accepts.
//
// Written here rather than read from each Agent's Capabilities(), because
// building an Agent to ask it requires a credential and a destination — the
// two things a user running `kno doctor` is usually missing. core/ring0.go's
// godoc says capabilities "are checked BEFORE work is scheduled" and nothing
// calls Capabilities() anywhere; the refusals that exist happen at adapter
// construction, which is still before any spend but is not the ring-0 check
// advertised. Tracked as docs/debt.md#58, and when that lands this table reads
// from it instead.
//
// Unavailable schemes are listed rather than omitted, because "kno does not
// know that word" and "kno knows it and this build cannot serve it" are
// different problems with different fixes, and a matrix that hides the second
// makes them look identical.
func adapterFacts() []adapterFact {
	facts := []adapterFact{
		{
			Scheme: agentref.SchemeFake, Available: true,
			GenerationParams: "n/a", TokenCounts: true, Spends: false,
			Note: "local, deterministic, makes no network call",
		},
		{
			Scheme: agentref.SchemeOpenAI, Available: true,
			GenerationParams: "per model (override with --generation-params)",
			TokenCounts:      true, Spends: true,
			Note: "OpenAI and any compatible endpoint via --base-url",
		},
		{
			Scheme: agentref.SchemeAnthropic, Available: true,
			GenerationParams: "per model", TokenCounts: true, Spends: true,
			Note: "requires --max-output-tokens",
		},
		{
			Scheme: agentref.SchemeBedrock, Available: true,
			GenerationParams: "per model", TokenCounts: true, Spends: true,
			Note: "Converse; requires --max-output-tokens; AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION",
		},
		{
			Scheme: agentref.SchemeVertex, Available: true,
			GenerationParams: "per model", TokenCounts: true, Spends: true,
			Note: "requires --max-output-tokens; GOOGLE_APPLICATION_CREDENTIALS, or GOOGLE_CLOUD_PROJECT + GOOGLE_CLOUD_REGION",
		},
		{
			Scheme: agentref.SchemeExec, Available: true,
			GenerationParams: "n/a", TokenCounts: false, Spends: false,
			Note: "runs a local command per Case; free unless --cost-per-call-usd is set",
		},
		{
			Scheme: agentref.SchemeTuned, Available: false,
			GenerationParams: "n/a",
			Note:             "fine-tuned outputs land with the tuner",
		},
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].Scheme < facts[j].Scheme })
	return facts
}

// doctorReport is the --json shape.
//
// Hand-written, like the baseline report and for the same reason: this is a CLI
// contract aimed at a person's jq pipeline, and it must not shift underneath
// them when a proto message gains a field (ADR-0001).
type doctorReport struct {
	Version      string        `json:"version"`
	Adapters     []adapterFact `json:"adapters"`
	Goals        []string      `json:"goals"`
	PriceTable   string        `json:"price_table"`
	PricedModels struct {
		OpenAI    []string `json:"openai"`
		Anthropic []string `json:"anthropic"`
		Bedrock   []string `json:"bedrock"`
		Vertex    []string `json:"vertex"`
	} `json:"priced_models"`
}

// doctorGoals is every Goal this build can score against.
//
// Extracted from runDoctor so docs/status.json reports the same list rather
// than a second copy of it (cli/status.go). One list, two readers.
func doctorGoals() []string {
	return []string{"exact-match"}
}

// runDoctor renders the matrix.
func runDoctor(out io.Writer, jsonOut bool) error {
	rep := doctorReport{
		Version:    doctorVersion(),
		Adapters:   adapterFacts(),
		Goals:      doctorGoals(),
		PriceTable: pricing.Version,
	}
	rep.PricedModels.OpenAI = pricing.Models(agentref.SchemeOpenAI)
	rep.PricedModels.Anthropic = pricing.Models(agentref.SchemeAnthropic)
	rep.PricedModels.Bedrock = pricing.Models(agentref.SchemeBedrock)
	rep.PricedModels.Vertex = pricing.Models(agentref.SchemeVertex)

	if jsonOut {
		return writeJSON(out, rep)
	}
	return renderDoctor(out, rep)
}

// renderDoctor writes the human matrix.
func renderDoctor(out io.Writer, rep doctorReport) error {
	var b strings.Builder

	fmt.Fprintf(&b, "kno %s\n\n", rep.Version)

	b.WriteString("Agents\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	// Writes to a strings.Builder through a tabwriter cannot fail — Builder's
	// Write never returns an error — so the checked one is Flush, which can.
	fmt.Fprintln(tw, "  scheme\tstatus\tcost\tgeneration params\tnotes") //nolint:errcheck // strings.Builder cannot fail
	for _, a := range rep.Adapters {
		status := "available"
		if !a.Available {
			status = "not in this build"
		}
		cost := "free"
		if a.Spends {
			cost = "spends"
		}
		if !a.Available {
			cost = "—"
		}
		fmt.Fprintf(tw, "  %s:\t%s\t%s\t%s\t%s\n", //nolint:errcheck // strings.Builder cannot fail
			a.Scheme, status, cost, a.GenerationParams, a.Note)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("rendering the agent matrix: %w", err)
	}

	fmt.Fprintf(&b, "\nGoals\n  %s\n", strings.Join(rep.Goals, ", "))

	// Counts, not the whole list: a user asking "is my model priced" is
	// answered by --json, and a wall of model names buries the two lines above
	// it that most diagnoses actually need.
	fmt.Fprintf(&b, "\nPrices  %s (%d openai, %d anthropic, %d bedrock, %d vertex models)\n",
		rep.PriceTable, len(rep.PricedModels.OpenAI), len(rep.PricedModels.Anthropic),
		len(rep.PricedModels.Bedrock), len(rep.PricedModels.Vertex))
	b.WriteString("        an unpriced model needs --price-input-per-mtok and " +
		"--price-output-per-mtok under a cost cap\n")

	_, err := io.WriteString(out, b.String())
	return err
}

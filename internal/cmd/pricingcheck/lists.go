package main

// suppression records a model whose sources are KNOWN to disagree, with the
// price-of-record page trusted over OpenRouter. The model is skipped by the
// agreement check so the known divergence does not page every run. The
// lifecycle rule is the point: a suppression whose sources have converged is
// dead and FAILS the check, so a list entry can never silently outlive the
// disagreement it exists for.
type suppression struct {
	scheme string
	model  string // raw id as the sources spell it
	reason string
}

// suppressions is the committed suppression list, seeded from the live
// capture of 2026-08-28.
var suppressions = []suppression{
	{
		scheme: "openai", model: "gpt-5.6-sol",
		reason: "OpenRouter lists this model at half of OpenAI's page price (both captured 2026-08-28); trust the price-of-record page",
	},
}

// exclusion records a model the detector deliberately does not flag. The
// deliberate ones cover models the table will never price; the pending ones
// cover models that carry their own price but whose rows are owed — the
// detector's pending list is the input docs/debt.md#46 promises. Lifecycle:
// an exclusion whose model gains a table row is dead and FAILS the check.
type exclusion struct {
	scheme  string
	model   string // raw id as the sources spell it
	reason  string
	ledger  string // docs/debt.md#N, empty for deliberate exclusions
	pending bool
}

// deliberateExclusions: models the table will not price, by design.
//
//	claude-mythos-5         invitation-only; pricing it would price a model
//	                        most runs cannot call
//	claude-mythos-preview   named in estimate.go's newTokenizerModels but not
//	                        generally available
var deliberateExclusions = []exclusion{
	{scheme: "anthropic", model: "claude-mythos-5", reason: "invitation-only; a price on a model most runs cannot call would mislead the table's readers"},
	{scheme: "anthropic", model: "claude-mythos-preview", reason: "named in adapters/agent/pricing/estimate.go's newTokenizerModels but not generally available"},
	{scheme: "anthropic", model: "claude-opus-4-1", reason: "retired (except on Bedrock and Google Cloud); not served by the M2 adapters"},
	{scheme: "anthropic", model: "claude-opus-4", reason: "retired (except on Google Cloud); not served by the M2 adapters"},
	{scheme: "anthropic", model: "claude-sonnet-4", reason: "retired (except on Bedrock and Google Cloud); not served by the M2 adapters"},
	{scheme: "anthropic", model: "claude-haiku-3-5", reason: "retired (except on Bedrock and Google Cloud); not served by the M2 adapters"},
}

// pendingExclusions: prefix-resolving variants discovered on the live sources
// (2026-08-28) that carry their own price and therefore must NOT inherit the
// base row. Their table rows are owed before 0.1.0 — docs/debt.md#46 — and
// the detector reports them every run, gated or not, until the debt is paid.
var pendingExclusions = []exclusion{
	{scheme: "anthropic", model: "claude-opus-5-fast", reason: "fast mode carries its own price (2x base on 2026-08-28)", ledger: "docs/debt.md#46", pending: true},
	{scheme: "anthropic", model: "claude-opus-5:batch", reason: "batch mode carries its own price", ledger: "docs/debt.md#46", pending: true},
	{scheme: "anthropic", model: "claude-sonnet-5:batch", reason: "batch mode carries its own price", ledger: "docs/debt.md#46", pending: true},
	{scheme: "anthropic", model: "claude-fable-5:batch", reason: "batch mode carries its own price", ledger: "docs/debt.md#46", pending: true},
	{scheme: "anthropic", model: "claude-opus-4-8-fast", reason: "fast mode carries its own price", ledger: "docs/debt.md#46", pending: true},
	{scheme: "anthropic", model: "claude-opus-4-8:batch", reason: "batch mode carries its own price", ledger: "docs/debt.md#46", pending: true},
	{scheme: "anthropic", model: "claude-opus-4-7-fast", reason: "fast mode carries its own price", ledger: "docs/debt.md#46", pending: true},
	{scheme: "anthropic", model: "claude-opus-4-7:batch", reason: "batch mode carries its own price", ledger: "docs/debt.md#46", pending: true},
	{scheme: "anthropic", model: "claude-opus-4-6:batch", reason: "batch mode carries its own price", ledger: "docs/debt.md#46", pending: true},
	{scheme: "anthropic", model: "claude-opus-4-5:batch", reason: "batch mode carries its own price", ledger: "docs/debt.md#46", pending: true},
	{scheme: "anthropic", model: "claude-sonnet-4-6:batch", reason: "batch mode carries its own price", ledger: "docs/debt.md#46", pending: true},
	{scheme: "anthropic", model: "claude-sonnet-4-5:batch", reason: "batch mode carries its own price", ledger: "docs/debt.md#46", pending: true},
	{scheme: "anthropic", model: "claude-haiku-4-5:batch", reason: "batch mode carries its own price", ledger: "docs/debt.md#46", pending: true},
	{scheme: "openai", model: "gpt-5.6-sol-pro", reason: "pro variant carries its own price", ledger: "docs/debt.md#46", pending: true},
	{scheme: "openai", model: "gpt-5.6-terra-pro", reason: "pro variant carries its own price", ledger: "docs/debt.md#46", pending: true},
	{scheme: "openai", model: "gpt-5.6-luna-pro", reason: "pro variant carries its own price", ledger: "docs/debt.md#46", pending: true},
}

// matchSuppression finds the suppression covering a row, by scheme and by id.
// The page spells anthropic names in prose ("Claude Opus 5") where the list
// spells them canonically, so both the raw spelling and the canonical one are
// compared.
func matchSuppression(list []suppression, scheme, raw, canonical string) *suppression {
	for i := range list {
		if list[i].scheme == scheme && (list[i].model == raw || canonicalModel(list[i].model) == canonical) {
			return &list[i]
		}
	}
	return nil
}

// matchExclusion finds the exclusion covering a row.
func matchExclusion(list []exclusion, scheme, raw, canonical string) *exclusion {
	for i := range list {
		if list[i].scheme == scheme && (list[i].model == raw || canonicalModel(list[i].model) == canonical) {
			return &list[i]
		}
	}
	return nil
}

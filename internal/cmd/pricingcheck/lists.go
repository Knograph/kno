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

// exclusion records a model the detector deliberately does not flag — a
// model the table will not price, with the reason why a row would be wrong.
// Lifecycle: an exclusion whose model gains a table row is dead and FAILS
// the check, so the list cannot outlive its reasons. pageAbsent marks an
// exclusion whose reason is that NO price-of-record page publishes the model:
// the day a parsed page row matches it, that reason is dead too, and the
// check fails instead of silently skipping.
type exclusion struct {
	scheme     string
	model      string // raw id as the sources spell it
	reason     string
	pageAbsent bool
}

// deliberateExclusions: models the table will not price, by design. Each
// entry's reason is the disposition: docs/debt.md#46 was repaid 2026-08-28
// partly by ROWS (the fast variants, which the provider's own page prices)
// and partly by these exclusions, each naming why a row would be wrong.
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
	// Batch variants. Anthropic publishes batch rates (50% of base) on its
	// own page, but no adapter speaks the batch endpoint: pricing these rows
	// would authorize a capped run that the Messages endpoint rejects per
	// Case, which is strictly worse than today's one-time refusal naming
	// pricing. The rows land with batch-mode support; until then a batch id
	// stays unpriced and refused, and the detector keeps the page's batch
	// table in view through the pending → deliberate move (docs/debt.md#46,
	// repaid 2026-08-28 with this disposition).
	{scheme: "anthropic", model: "claude-opus-5:batch", reason: "batch mode: rates published (50% base) but no adapter speaks the batch endpoint; row lands with batch-mode support"},
	{scheme: "anthropic", model: "claude-sonnet-5:batch", reason: "batch mode: rates published (50% base) but no adapter speaks the batch endpoint; row lands with batch-mode support"},
	{scheme: "anthropic", model: "claude-fable-5:batch", reason: "batch mode: rates published (50% base) but no adapter speaks the batch endpoint; row lands with batch-mode support"},
	{scheme: "anthropic", model: "claude-opus-4-8:batch", reason: "batch mode: rates published (50% base) but no adapter speaks the batch endpoint; row lands with batch-mode support"},
	{scheme: "anthropic", model: "claude-opus-4-7:batch", reason: "batch mode: rates published (50% base) but no adapter speaks the batch endpoint; row lands with batch-mode support"},
	{scheme: "anthropic", model: "claude-opus-4-6:batch", reason: "batch mode: rates published (50% base) but no adapter speaks the batch endpoint; row lands with batch-mode support"},
	{scheme: "anthropic", model: "claude-opus-4-5:batch", reason: "batch mode: rates published (50% base) but no adapter speaks the batch endpoint; row lands with batch-mode support"},
	{scheme: "anthropic", model: "claude-sonnet-4-6:batch", reason: "batch mode: rates published (50% base) but no adapter speaks the batch endpoint; row lands with batch-mode support"},
	{scheme: "anthropic", model: "claude-sonnet-4-5:batch", reason: "batch mode: rates published (50% base) but no adapter speaks the batch endpoint; row lands with batch-mode support"},
	{scheme: "anthropic", model: "claude-haiku-4-5:batch", reason: "batch mode: rates published (50% base) but no adapter speaks the batch endpoint; row lands with batch-mode support"},
	// OpenRouter-listed variants with NO price-of-record page. The ledger's
	// own standard (docs/debt.md#46) refuses a non-price-of-record source for
	// a spend-authorizing row, so these are deliberate exclusions rather than
	// rows — and the dead-exclusion check fails the run the day a
	// price-of-record page starts publishing them, which is the signal to
	// revisit.
	{scheme: "anthropic", model: "claude-opus-4-7-fast", reason: "OpenRouter-listed; Anthropic's page prices fast mode for Opus 5 and 4.8 only — no price-of-record exists", pageAbsent: true},
	{scheme: "openai", model: "gpt-5.6-sol-pro", reason: "OpenRouter-listed; OpenAI's model page publishes no pro-tier rates — no price-of-record exists", pageAbsent: true},
	{scheme: "openai", model: "gpt-5.6-terra-pro", reason: "OpenRouter-listed; OpenAI's model page publishes no pro-tier rates — no price-of-record exists", pageAbsent: true},
	{scheme: "openai", model: "gpt-5.6-luna-pro", reason: "OpenRouter-listed; OpenAI's model page publishes no pro-tier rates — no price-of-record exists", pageAbsent: true},
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

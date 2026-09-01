# Cookbook

Task-shaped recipes. Each one is a thing you might actually want to do, start to finish.

**The recipes live in [`uknoAI/kno-examples`](https://github.com/uknoAI/kno-examples).** They
moved so that something could run them. Every page there declares, in front matter, how far it
has actually been verified — and for eleven of them the answer is now "CI ran these exact
commands against the released binary and compared the result to a committed expectation", which
was true of nothing in this repository before. The rest say plainly that they are not verified
end to end, and say it in the same neutral register whether the reason is a vendor credential or
a vendor console.

`docs/cookbook/` keeps this index and one line per moved recipe, so a link anyone already has
still resolves. It does not keep a second copy of any recipe: a second copy is what this move
was for. The README quickstart above stays here, in full and self-contained — the front door must
not depend on a second repository being reachable.

## Core recipes

| Recipe | What it covers |
|---|---|
| [Calibrate a judge](calibrate-a-judge.md) | `kno judge calibrate` — what a judge's kappa claims, the 0.60 floor, and how to fix a prompt against the records it gets wrong |
| [Check whether your evals can attribute anything](https://github.com/uknoAI/kno-examples/blob/main/recipes/check-your-evals.md) | `kno eval inspect` before you spend — behaviors, separable effects, and the five checks |
| [Score your agent for the first time](https://github.com/uknoAI/kno-examples/blob/main/recipes/first-baseline.md) | Getting from an eval file to a baseline, and reading what comes back |
| [Point Kno at your own provider](https://github.com/uknoAI/kno-examples/blob/main/recipes/your-own-provider.md) | Keys, cost caps, local model servers, and the first run that can bill you |
| [Score your agent against Claude](https://github.com/uknoAI/kno-examples/blob/main/recipes/anthropic.md) | The Anthropic agent — `ANTHROPIC_API_KEY`, the priced models, and a complete baseline-to-report run |
| [Score your agent on Bedrock](https://github.com/uknoAI/kno-examples/blob/main/recipes/bedrock.md) | The AWS agent — env-only credentials, regional pricing, and the cross-region profile refusal |
| [Score your agent on Vertex](https://github.com/uknoAI/kno-examples/blob/main/recipes/vertex.md) | The Google Cloud agent — service-account JWT exchange, regional pricing, and the cross-region profile refusal |
| [Gate a deploy on Kno in CI](https://github.com/uknoAI/kno-examples/blob/main/recipes/ci-gate.md) | Exit codes, `--json`, and what to fail the build on |
| [Value a pool of assets](https://github.com/uknoAI/kno-examples/blob/main/recipes/value-a-pool.md) | Deltas with their intervals, the control's harm bound, and what `underpowered` means |
| [Choose a portfolio under budget](https://github.com/uknoAI/kno-examples/blob/main/recipes/select-a-portfolio.md) | Which assets earn their place, what the corrected intervals mean, and why the rejection log is a deliverable |
| [Export a tuning set](https://github.com/uknoAI/kno-examples/blob/main/recipes/export-a-tuning-set.md) | The destination grammar, the overwrite refusal, and the byte-identical re-export contract |
| [Read the whole story with `kno report`](https://github.com/uknoAI/kno-examples/blob/main/recipes/read-the-whole-story.md) | One page across the stages — what each section means, what "no cluster data" says, and why the holdout caveat is mandatory |
| [Delete stored conversation content](https://github.com/uknoAI/kno-examples/blob/main/recipes/retention.md) | What Kno keeps, what `kno purge` removes, and why it keeps the rest |

`calibrate-a-judge` is the one recipe still held here in full. A page is held here only while
its commands are unreleased, because `kno-examples` checks every command on every page against
the binary you can download and a page about an unreleased command can carry no honest tier
there.

**That reason has expired for this page.** v0.1.5 ships `kno judge calibrate`, so it is here now
only because it has not been migrated yet — and the migration is worth doing rather than
deferring: the command defaults to `--replay`, calls no model, and prints a deterministic kappa
with a PASS/FAIL gate, so the page can be verified end to end rather than by hand.

`check-your-evals` was the other one, and left when v0.1.4 shipped `kno eval inspect`.

## Vendor recipes

Every vendor recipe is the same shape — candidate content as Assets, real questions with vetted answers as Cases, baseline, value, then act on the table — transplanted to a vendor's own data. The [Zendesk recipe](https://github.com/uknoAI/kno-examples/blob/main/recipes/zendesk.md) carries the vendor-swap table and the general explanation; the rest are the vendor-specific export commands and read-back decisions.

None of them is verified end to end, and each page says so on itself. What CI checks nightly is
that every `kno` command still exists with the flags the page names; what checks the vendor half
is a human with credentials on an approval-gated run, with the date written back by machine.

| Scenario | Vendor recipe |
|---|---|
| Support | [Zendesk](https://github.com/uknoAI/kno-examples/blob/main/recipes/zendesk.md) · [HubSpot](https://github.com/uknoAI/kno-examples/blob/main/recipes/hubspot.md) · [Salesforce](https://github.com/uknoAI/kno-examples/blob/main/recipes/salesforce.md) |
| Coding agent | [GitHub](https://github.com/uknoAI/kno-examples/blob/main/recipes/github.md) · [Jira + Confluence](https://github.com/uknoAI/kno-examples/blob/main/recipes/jira.md) · [Confluence](https://github.com/uknoAI/kno-examples/blob/main/recipes/confluence.md) |
| Eval platforms | [LangSmith](https://github.com/uknoAI/kno-examples/blob/main/recipes/langsmith.md) · [Langfuse](https://github.com/uknoAI/kno-examples/blob/main/recipes/langfuse.md) · [Braintrust](https://github.com/uknoAI/kno-examples/blob/main/recipes/braintrust.md) · [Hugging Face](https://github.com/uknoAI/kno-examples/blob/main/recipes/huggingface.md) — datasets as first-class Evals, no export step |
| E-commerce | [Shopify](https://github.com/uknoAI/kno-examples/blob/main/recipes/shopify.md) |
| Payments | [Stripe](https://github.com/uknoAI/kno-examples/blob/main/recipes/stripe.md) |
| Internal knowledge | [Notion](https://github.com/uknoAI/kno-examples/blob/main/recipes/notion.md) |
| Workflow automation | [n8n](https://github.com/uknoAI/kno-examples/blob/main/recipes/n8n.md) — scheduled valuation with alerts, on the exit-code contract |

More arrive with the stages that need them: validating on the holdout, bringing your own judge, and writing a pool plugin.

## One thing this page cannot do

`make docs` checks that every *relative* link in this repository resolves, and skips `https://`
targets by construction. Every link above is now an `https://` one, so this page can point at a
recipe that no longer exists and nothing here will notice. That asymmetry is real and is
[ledgered](../debt.md), not hidden: the repayment is an external-link check in `kno-examples`'
nightly, validating this page from the other side.

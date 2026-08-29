# Cookbook

Task-shaped recipes. Each one is a thing you might actually want to do, start to finish.

## Core recipes

| Recipe | What it covers |
|---|---|
| [Score your agent for the first time](first-baseline.md) | Getting from an eval file to a baseline, and reading what comes back |
| [Point Kno at your own provider](your-own-provider.md) | Keys, cost caps, local model servers, and the first run that can bill you |
| [Score your agent against Claude](anthropic.md) | The Anthropic agent — `ANTHROPIC_API_KEY`, the priced models, and a complete baseline-to-report run |
| [Score your agent on Bedrock](bedrock.md) | The AWS agent — env-only credentials, regional pricing, and the cross-region profile refusal |
| [Score your agent on Vertex](vertex.md) | The Google Cloud agent — service-account JWT exchange, regional pricing, and the cross-region profile refusal |
| [Gate a deploy on Kno in CI](ci-gate.md) | Exit codes, `--json`, and what to fail the build on |
| [Choose a portfolio under budget](select-a-portfolio.md) | Which assets earn their place, what the corrected intervals mean, and why the rejection log is a deliverable |
| [Export a tuning set](export-a-tuning-set.md) | The destination grammar, the overwrite refusal, and the byte-identical re-export contract |
| [Read the whole story with `kno report`](read-the-whole-story.md) | One page across the stages — what each section means, what "no cluster data" says, and why the holdout caveat is mandatory |
| [Delete stored conversation content](retention.md) | What Kno keeps, what `kno purge` removes, and why it keeps the rest |

## Vendor recipes

Every vendor recipe is the same shape — candidate content as Assets, real questions with vetted answers as Cases, baseline, value, then act on the table — transplanted to a vendor's own data. The [Zendesk recipe](zendesk.md) carries the vendor-swap table and the general explanation; the rest are the vendor-specific export commands and read-back decisions.

| Scenario | Vendor recipe |
|---|---|
| Support | [Zendesk](zendesk.md) · [HubSpot](hubspot.md) · [Salesforce](salesforce.md) |
| Coding agent | [GitHub](github.md) · [Jira + Confluence](jira.md) · [Confluence](confluence.md) |
| Eval platforms | [LangSmith](langsmith.md) · [Langfuse](langfuse.md) · [Braintrust](braintrust.md) · [Hugging Face](huggingface.md) — datasets as first-class Evals, no export step |
| E-commerce | [Shopify](shopify.md) |
| Payments | [Stripe](stripe.md) |
| Internal knowledge | [Notion](notion.md) |
| Workflow automation | [n8n](n8n.md) — scheduled valuation with alerts, on the exit-code contract |

More arrive with the stages that need them: validating on the holdout, bringing your own judge, and writing a pool plugin.

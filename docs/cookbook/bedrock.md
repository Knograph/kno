# Score your agent against Claude on Bedrock

What you're asking: **measure your knowledge assets against a Claude model behind AWS Bedrock** — the same baseline, value, select, export, report loop as everywhere else, with Bedrock's regional pricing doing the arithmetic and a consent prompt standing between you and the first call that can bill you.

## The short version

```bash
export AWS_ACCESS_KEY_ID=AKIA...
export AWS_SECRET_ACCESS_KEY=...
export AWS_REGION=us-east-1

kno baseline \
  --evals cases.jsonl \
  --agent bedrock:anthropic.claude-sonnet-4-5-20250929-v1:0 \
  --max-output-tokens 1024 \
  --max-cost-usd 2.00
```

That is a complete, capped run. Kno prices every Case from its own table, so you do not have to tell it what a call costs. Credentials are environment-only: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and optionally `AWS_SESSION_TOKEN`, plus `AWS_REGION` for the endpoint. None of them ever goes in kno.yaml, and `kno doctor` tells you which are missing.

## The agent reference

`bedrock:` is one of the agent schemes, and it names a Bedrock model id:

| Scheme | Meaning |
|---|---|
| `fake:` | local, deterministic, costs nothing |
| `openai:` | OpenAI models and any compatible endpoint |
| `anthropic:` | Claude models on Anthropic's API |
| `bedrock:` | Claude models on AWS Bedrock, Converse API |
| `vertex:` | Claude models on Google Vertex AI |
| `exec:` | a local command per Case |

The reference is `scheme:target`, split at the **first** colon. The scheme is whatever precedes it; everything after is the model id, verbatim. `kno init`'s wizard accepts it, and so do `--agent` and kno.yaml's `agent:`.

The endpoint is fixed — Bedrock's Converse API at the region in `AWS_REGION` — and deliberate: a regional endpoint has one price of record, so a base URL is refused at parse rather than stored and never read. The insecure-base-url, private-address, and seed flags do not apply to this scheme.

## `--max-output-tokens` is required

The Converse API requires `max_tokens` on every request, and a cost cap cannot bound an output term that has no ceiling. Kno refuses before any Case runs:

```
bedrock: no output ceiling
fix: set --max-output-tokens; Converse requires maxTokens and a cost cap cannot bound an unbounded output term
```

## What a call costs

Bedrock publishes per-region prices, and Kno's table keys them by the Bedrock model id (`anthropic.claude-...`) with the same rates as the Anthropic API, plus a **regional multiplier**: 1.10x in Europe, 1.00x in the US. The multiplier is a committed constant (`pricing.RegionalMultiplierPct`) applied by the estimate, the worst-case estimate, and settlement — before the budget guard settles, so the cap's arithmetic and the consent figure both use the regional rate. The committed 110% is confirmed on every pricingcheck run against AWS's own machine-readable Bedrock price list (check 6); Vertex has no machine-readable source and is reported every run.

Regional prices as of the table's date, USD per million tokens:

| Model | Input | Output | eu region |
|---|---|---|---|
| `anthropic.claude-sonnet-4-5-...:0` | 3.00 | 15.00 | 3.30 in / 16.50 out |

`kno doctor` prints the table's coverage and date, now with all four schemes: `Prices <date> (3 openai, 12 anthropic, 12 bedrock, 12 vertex models)`.

The rule behind the table: **a version suffix inherits, a variant does not.** A dated Bedrock id prices like its base model — but a `us.`- or `eu.`-prefixed **cross-region inference profile** id bills at the destination region's price, which no row claims, so it is refused until priced. The refusal names the class so you do not chase a typo:

```
the us.-prefixed id names a cross-region inference profile, which bills at the destination region's price; no row claims that rate, so it is refused until one exists — pass explicit prices with --price-input-per-mtok and --price-output-per-mtok if you know the destination rate
```

## If your model has no price

The table does not cover every Bedrock id, and Kno deliberately refuses to guess. Under a cost cap, an unpriced model is refused **once**, before any Case runs. Two paths onward:

```bash
# the model's real rates, from the provider's page:
--price-input-per-mtok 3.00 --price-output-per-mtok 15.00   # both required

# or your own expected cost per call, for a gateway you know:
--cost-per-call-usd 0.002
```

Half a price produces an estimate that is wrong in the direction that under-reserves, which is a cap that does not bind — hence "both required".

## The consent prompt

Above **$1.00** estimated, Kno asks before starting: `[y]es`, `[n]o`, or `[c]hange the cap`. On a non-TTY run it prints the figure and `Re-run with --yes to proceed.`, then exits `2` with nothing spent. A scheduled run needs `--yes` up front — and `--yes` still prints what it is agreeing to.

## The whole run

Baseline the agent, value the pool, choose the portfolio, read the story:

```bash
# 1. Baseline: how well does Claude answer with no grounding?
kno baseline --evals cases.jsonl \
  --agent bedrock:anthropic.claude-sonnet-4-5-20250929-v1:0 \
  --max-output-tokens 1024 --max-cost-usd 2.00 --yes

# 2. Value: what does each asset add, with its confidence interval?
kno value --evals cases.jsonl --pool pool.jsonl \
  --baseline-run-id <run id> \
  --agent bedrock:anthropic.claude-sonnet-4-5-20250929-v1:0 \
  --max-output-tokens 1024 --max-cost-usd 5.00 --yes

# 3. Portfolio: which assets earn their place, and what is the gain?
kno select --value-run-id <run id>

# 4. The story, one page:
kno report
```

A baseline recorded against `bedrock:` cannot be re-measured against `vertex:` mid-run: the model gate arms on the first process and refuses an alias re-pointing, so a swap is a new baseline, not a resumed one.

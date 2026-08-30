# Score your agent against Claude on Vertex AI

What you're asking: **measure your knowledge assets against a Claude model behind Google Vertex AI** — the same baseline, value, select, export, report loop as everywhere else, with Vertex's regional pricing doing the arithmetic and a consent prompt standing between you and the first call that can bill you.

## The short version

```bash
export GOOGLE_APPLICATION_CREDENTIALS=/secrets/service-account.json
export GOOGLE_CLOUD_REGION=us-central1

kno baseline \
  --evals cases.jsonl \
  --agent vertex:claude-sonnet-4-5 \
  --max-output-tokens 1024 \
  --max-cost-usd 2.00
```

That is a complete, capped run. Kno prices every Case from its own table, so you do not have to tell it what a call costs.

Credentials are environment-only, and there are two ways to supply them:

- `GOOGLE_APPLICATION_CREDENTIALS` — the path to a service-account JSON file (the file's `private_key` and `client_email` are read for the JWT-to-access-token exchange; nothing else is parsed, and the values are never logged)
- or `GOOGLE_CLOUD_PROJECT` + `GOOGLE_CLOUD_REGION` for the metadata server's default credentials (compute environments)

`kno doctor` tells you which are missing. The JWT exchange is a stdlib implementation — no Google SDK, no ambient credentials, and the token is cached with a two-minute expiry margin so a long run re-fetches before a 401.

## The agent reference

`vertex:` is one of the agent schemes, and it names a model:

| Scheme | Meaning |
|---|---|
| `fake:` | local, deterministic, costs nothing |
| `openai:` | OpenAI models and any compatible endpoint |
| `anthropic:` | Claude models on Anthropic's API |
| `bedrock:` | Claude models on AWS Bedrock, Converse API |
| `vertex:` | Claude models on Google Vertex AI, `:rawPredict` |
| `exec:` | a local command per Case |

The reference is `scheme:target`, split at the **first** colon. The scheme is whatever precedes it; everything after is the model id, verbatim. A dated pin (`claude-3-5-sonnet@20240620`) is accepted and inherits its base row.

The endpoint is fixed — Vertex's regional `:rawPredict` at the region in `GOOGLE_CLOUD_REGION` — and deliberate: a regional endpoint has one price of record, so a base URL is refused at parse rather than stored and never read. The insecure-base-url, private-address, and seed flags do not apply to this scheme.

## `--max-output-tokens` is required

`:rawPredict` requires `max_tokens` on every request, and a cost cap cannot bound an output term that has no ceiling. Kno refuses before any Case runs:

```
vertex: no output ceiling
fix: set --max-output-tokens; :rawPredict requires max_tokens and a cost cap cannot bound an unbounded output term
```

## What a call costs

Vertex prices Claude at the vendor's own per-token rates, plus a **regional multiplier**: 1.10x in Europe, 1.00x in the US. The multiplier is a committed constant (`pricing.RegionalMultiplierPct`) applied by the estimate, the worst-case estimate, and settlement — before the budget guard settles, so the cap's arithmetic and the consent figure both use the regional rate. Google publishes no machine-readable price list, so the constant cannot be confirmed by a live source the way Bedrock's is: pricingcheck reports that standing obligation on every run, and a human reviews the table's date.

Rates as of the table's date, USD per million tokens:

| Model | Input | Output | eu region |
|---|---|---|---|
| `claude-sonnet-4-5` | 3.00 | 15.00 | 3.30 in / 16.50 out |
| `claude-opus-5` | 5.00 | 25.00 | 5.50 in / 27.50 out |

`kno doctor` prints the table's coverage and date, now with all four schemes: `Prices <date> (3 openai, 12 anthropic, 12 bedrock, 12 vertex models)`.

The rule behind the table: **a version suffix inherits, a variant does not.** A dated pin prices like its base model — but a `us.`- or `eu.`-prefixed **cross-region inference profile** id bills at the destination region's price, which no row claims, so it is refused until priced. The refusal names the class so you do not chase a typo:

```
the us.-prefixed id names a cross-region inference profile, which bills at the destination region's price; no row claims that rate, so it is refused until one exists — pass explicit prices with --price-input-per-mtok and --price-output-per-mtok if you know the destination rate
```

## If your model has no price

The table does not cover every Vertex model, and Kno deliberately refuses to guess. Under a cost cap, an unpriced model is refused **once**, before any Case runs. Two paths onward:

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
  --agent vertex:claude-sonnet-4-5 \
  --max-output-tokens 1024 --max-cost-usd 2.00 --yes

# 2. Value: what does each asset add, with its confidence interval?
kno value --evals cases.jsonl --pool pool.jsonl \
  --baseline-run-id <run id> \
  --agent vertex:claude-sonnet-4-5 \
  --max-output-tokens 1024 --max-cost-usd 5.00 --yes

# 3. Portfolio: which assets earn their place, and what is the gain?
kno select --value-run-id <run id>

# 4. The story, one page:
kno report
```

A baseline recorded against `vertex:` cannot be re-measured against `bedrock:` mid-run: the model gate arms on the first process and refuses an alias re-pointing, so a swap is a new baseline, not a resumed one.

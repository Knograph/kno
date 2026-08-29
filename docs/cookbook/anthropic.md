# Score your agent against Claude

What you're asking: **measure your knowledge assets against a Claude model on Anthropic's API** — the same baseline, value, select, export, report loop as everywhere else, with Anthropic's published rates doing the arithmetic and a consent prompt standing between you and the first call that can bill you.

## The short version

```bash
export ANTHROPIC_API_KEY=sk-ant-...

kno baseline \
  --evals cases.jsonl \
  --agent anthropic:claude-opus-5 \
  --max-output-tokens 1024 \
  --max-cost-usd 2.00
```

That is a complete, capped run. Kno prices every Case from its own table, so you do not have to tell it what a call costs. The one flag you must supply on your own — `--max-output-tokens` — is required by the Messages API itself, not by Kno (below).

## The agent reference

`anthropic:` is one of the agent schemes, and it names a model:

| Scheme | Meaning |
|---|---|
| `fake:` | local, deterministic, costs nothing |
| `openai:` | OpenAI models and any compatible endpoint |
| `anthropic:` | Claude models on Anthropic's API |
| `exec:` | a local command per Case |

The reference is `scheme:target[@base-url]`, split at the **first** colon — so the scheme is whatever precedes it and everything after is the model, verbatim:

```bash
--agent anthropic:claude-opus-5
--agent anthropic:claude-opus-5@https://gateway.example.com/v1
```

The `@base-url` form is the same thing as `--base-url` on the command line; passing both is refused as two ways of saying the same thing, disagreeing. The URL must be an endpoint root with a scheme, and never carries credentials — a base URL is recorded on the Run and printed in `--json`, so a key placed there ends up in several durable places at once.

## The key

`ANTHROPIC_API_KEY`, from the environment, never from a flag — there is no `--api-key`, and a key on a command line is written to your shell history and shown in `ps` output.

The variable is bound to Anthropic's own host and to no other. A run that needs it and does not have it is refused before any request:

```
fix: export ANTHROPIC_API_KEY; it is bound to api.anthropic.com but is empty
```

For any **other** host you say which variable holds its key, the same way every other scheme does:

```bash
export MY_GATEWAY_KEY=...

kno baseline --evals cases.jsonl \
  --agent anthropic:claude-opus-5@https://gateway.example.com/v1 \
  --key-env gateway.example.com=MY_GATEWAY_KEY
```

Kno will not fall back to `ANTHROPIC_API_KEY` for a host that is not Anthropic's — that would mail your Anthropic key to whoever you pointed the endpoint at. A plain-HTTP or private-address gateway needs the same waivers a local model server does: `--allow-insecure-base-url` and `--allow-private-address`, each a separate opt-in. And Kno never follows redirects: Anthropic authenticates with `x-api-key`, which Go's HTTP client forwards on a cross-domain redirect, so a base URL that redirects would leak the key verbatim.

## `--max-output-tokens` is required

The Messages API requires `max_tokens` on every request, and a cost cap cannot bound an output term that has no ceiling. Kno refuses before any Case runs:

```
anthropic: no output ceiling
fix: set --max-output-tokens; the Messages API requires max_tokens and a cost cap cannot bound an unbounded output term
```

Every command that calls the model — `kno baseline` and `kno value` — carries it. `select` and `export` do not call the model, so they do not need it.

## What a call costs

Kno prices Anthropic models from its own static table, dated **2026-08-28** — never fetched at runtime, because a pricing endpoint that is down or wrong is a spend path with no ceiling. Rates per million tokens, US dollars:

| Model | Input | Cached read | Cache write | Output |
|---|---|---|---|---|
| `claude-opus-5` | 5.00 | 0.50 | 6.25 | 25.00 |
| `claude-opus-4-8` · `4-7` · `4-6` · `4-5` | 5.00 | 0.50 | 6.25 | 25.00 |
| `claude-sonnet-5` | 2.00 | 0.20 | 2.50 | 10.00 |
| `claude-sonnet-4-6` · `4-5` | 3.00 | 0.30 | 3.75 | 15.00 |
| `claude-haiku-4-5` | 1.00 | 0.10 | 1.25 | 5.00 |
| `claude-fable-5` | 10.00 | 1.00 | 12.50 | 50.00 |
| `claude-opus-5-fast` · `4-8-fast` | 10.00 | — | — | 50.00 |

The fast rows bill input and output only — Anthropic publishes no cache rates for fast mode, and "not billed separately" is not the same as "free", so those cells are absent rather than zero. (Before the fast rows existed, a capped run on fast mode was refused up front naming pricing; now it is authorized at the published rate.)

The rule behind the table: **a version suffix inherits, a variant does not.** `claude-opus-5-20260821` prices like `claude-opus-5` — a dated pin is the same model, and so is `latest` — but `claude-opus-5-fast` is its own product with its own rate, and prefix matching it against the base row once authorized a run at a fraction of its true rate with no signal at all. That failure is why variant words never inherit.

`kno doctor` prints the table's coverage and date: `Prices 2026-08-28 (3 openai, 12 anthropic models)`.

## If your model has no price

The table does not cover every model Anthropic has shipped, and Kno deliberately refuses to guess. Under a cost cap, an unpriced model is refused **once**, before any Case runs, with the fix spelled out:

```
the per-Case cost of this run is unknown
fix: pass --cost-per-call-usd with your expected per-call cost, or --accept-unknown-cost to run anyway
```

Two paths onward:

```bash
# the model's real rates, from the provider's page:
--price-input-per-mtok 3.00 --price-output-per-mtok 15.00   # both required

# or your own expected cost per call, for a gateway you know:
--cost-per-call-usd 0.002
```

Half a price produces an estimate that is wrong in the direction that under-reserves, which is a cap that does not bind — hence "both required". `--accept-unknown-cost` runs without a price: no estimate, so no consent figure, and no ceiling. It is the explicit "I know what this costs" claim, and like the security waivers it is a flag, never a config file value.

## The consent prompt

Above **$1.00** estimated, Kno asks before starting. On a terminal the dialog prints the bounded figure and takes one of three answers: `[y]es`, `[n]o`, or `[c]hange the cap` — changing the cap re-quotes the same figure against the new headroom, and agreeing to it is the consent. On a non-TTY run there is nobody to ask: it prints the figure and `Re-run with --yes to proceed.`, then exits `2` with nothing spent. In `--json` mode it refuses the same way at exit `1` (`--json cannot prompt; pass --yes to proceed`). A scheduled run needs `--yes` up front.

`--yes` still prints what it is agreeing to, so the number is in your scrollback and your CI log:

```
Proceeding with --yes: this run would spend about $5.00 across 13 Cases.
```

## The whole run

Baseline the agent, value the pool, choose the portfolio, export the tuning set, read the story:

```bash
# 1. Baseline: how well does Claude answer with no grounding?
kno baseline --evals cases.jsonl \
  --agent anthropic:claude-opus-5 --max-output-tokens 1024 \
  --max-cost-usd 2.00 --yes

# 2. Value: what does each asset add, with its confidence interval?
kno value --evals cases.jsonl --pool pool.jsonl \
  --baseline-run-id <the run id from step 1> \
  --agent anthropic:claude-opus-5 --max-output-tokens 1024 \
  --max-cost-usd 10.00 --yes

# 3. Select: which assets earn their place under a budget?
kno select --value-run-id <the run id from step 2> \
  --max-cost-usd 5.00 --yes

# 4. Export: the chosen set as a tuning artifact.
kno export --select-run-id <the run id from step 3> \
  --pool pool.jsonl --destination tuning_set \
  --out tuning-set.jsonl

# 5. The whole story on one page.
kno report --value-run-id <the run id from step 2> \
  --select-run-id <the run id from step 3>
```

Two details worth the ink:

- **Value must name the same agent as the baseline.** It defaults to `fake:` when you do not name one, and the deltas would be about a different agent than the one you ship. The same `--max-output-tokens` is needed because it is the same adapter.
- **Select and export do not call the model.** `--yes` on select is for the run's own spend contract (it has a budget cap and no model calls, so nothing crosses the threshold); export writes a file and refuses to overwrite an existing `--out` without `--force`. Both are explained in their own recipes.

An exit code `2` anywhere in the loop is a budget stop, not a failure — `--resume` continues from where the run stopped without paying for anything twice.

## See also

- [Point Kno at your own provider](your-own-provider.md) — keys, caps, and consent for every scheme, including the `anthropic:` short form
- [Score your agent for the first time](first-baseline.md) — the `fake:` walkthrough this builds on, with the Cases and Pool files
- [Read the whole story with `kno report`](read-the-whole-story.md) — what the report's sections mean, and the holdout caveat
- [What the numbers mean](../what-the-numbers-mean.md) — what a delta and its interval do and do not claim

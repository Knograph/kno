# Evaluate against Hugging Face datasets

What you're asking: **measure your agent against the eval sets you already keep on the Hugging Face Hub**, and value a pool straight from a dataset split — no download, no export that goes stale the moment the dataset changes.

## What maps where

| Hugging Face thing | Kno thing | How |
|---|---|---|
| Dataset config/split | Evals source | `--evals hf:<org>/<name>/<config>/<split>` |
| Row | Case | `input`/`prompt`/`question` (first present) → the input; `expected`/`completion`/`answer` (first present) → the expected answer; `row_idx` → the Case id |
| The dataset's git commit | Run resume fingerprint | The `x-revision` response header, folded into the Run's ContentHash — a dataset edit moves the fingerprint, so a resume against the changed split refuses rather than mixes populations |

The mapping is decided once per split from the first row, not guessed per row: a split whose rows disagree about their own shape is refused, naming the columns it actually has. A row with a null or missing input is fatal, naming the row — the denominator behind every later delta must not shrink silently. If several expected-like columns exist, the first one present wins and the others are dropped; if the split has no expectation column at all, those Cases score against rubric alone.

For pools the kind is part of the address: `hf:<org>/<name>/<config>/<split>:knowledge` or `:behavior`. An hf pool without a declared kind is refused — the kind routes the Asset to a destination, and a routing decision is not something a dataset can make for you.

## 1. Point the credential at the environment

```bash
export HF_TOKEN=hf_...
```

Only needed for gated or private datasets — a public one needs no token. The token comes from the environment, never from a flag or a config file. If the dataset does not exist *or* is gated, the API answers 401, and the refusal offers both remedies: check the name, and check the token.

## 2. Baseline against the dataset

```bash
kno baseline --evals hf:datasets-benchmarks/legal-bench/main/train \
  --agent openai:gpt-4.1 --max-cost-usd 5.00 --yes
```

`--yes` answers the spend confirmation — above a $1.00 estimate Kno asks on a terminal, and a non-TTY run exits `2` without it.

Kno streams the rows (paged), maps each row to a Case, assigns the dev/holdout split with the same hash every other source uses, and seals the holdout. Every page's `x-revision` must match the first page's — a split that changes mid-read is a different object, and that is fatal, not absorbed.

## 3. Value a pool against the same dataset

```bash
kno value --evals hf:datasets-benchmarks/legal-bench/main/train \
  --pool hf:datasets-benchmarks/legal-corpus/main/train:knowledge \
  --baseline-run-id <the run id from step 2> \
  --agent openai:gpt-4.1 --max-cost-usd 10.00 --yes
```

A row's text-bearing columns — the ones whose values are JSON strings — become the Asset content, one `name: value` line per column, sorted by column name so the bytes are deterministic whatever order the server emits keys in. Each Asset's id is `<dataset>/<config>/<split>@<row_idx>`, the server's own addressing, stable across re-reads.

## Reading the results

The output is the standard table: each Asset's Δgoal, `delta_per_cost`, and the cost of carrying it. Two numbers deserve their own page — [what-the-numbers-mean](../what-the-numbers-mean.md) — and one of them is the bias all three content-type pools share: `context_tokens` is estimated from bytes, so two Assets of equal real token cost can differ in `delta_per_cost` by content type alone. It is [ledgered debt](../debt.md#68), acknowledged on the field and in the numbers page rather than hidden.

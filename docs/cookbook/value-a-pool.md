# Value a pool of assets

What you're asking: **does this asset earn its place?** `kno value` answers it per asset — how much it moves your Goal, how sure Kno is, and what it cost to find out.

## Before you start

You need two files and one recorded run:

- **Cases** — the same eval file you scored the baseline against.
- **Assets** — a JSONL pool, one asset per line:
  ```json
  {"id":"refund-policy-faq","content":"Returns are accepted within 30 days...","tags":["billing"]}
  {"id":"pricing-tier-table","content":"Pro: $29/mo...","tags":["billing"]}
  ```
- **A baseline** — from `kno baseline` (same `--db`), whose recorded scores every delta pairs against.

## Run it

```sh
kno value --evals cases.jsonl --pool assets.jsonl \
  --baseline-run-id <id-from-baseline> \
  --agent fake: --yes
```

The run routes each asset to the Cases it could plausibly affect (by tag overlap with the baseline's failures), injects it into the agent's context for the treatment arm, re-runs the same Cases without it for the control, and records one Valuation per asset.

## Read the report

```
ASSET          DELTA (95% CI)                CONTROL           NOTE
refund-faq     +0.2123  [+0.08, +0.34]       low -0.03         —
pricing-tier   —                               —                 routed to nothing
```

- **DELTA** is the mean change on the Cases the asset was routed to, with its interval. A delta without an interval is never printed — if the sample is too small or too ragged to form one, the row says so.
- **CONTROL** is the one-sided harm bound over the untouched reserved slice: how much damage could be hiding in "no regression". Read it as a limit, not a score — see [what the numbers mean](../what-the-numbers-mean.md).
- **routed to nothing** is a real answer: the asset matches no failure cluster, so it costs nothing and changes nothing.
- If a row says `underpowered`, the control sample could not see a regression as small as the harm margin. The number travels first; the flag travels beside it.

## When the budget stops the run

A run that hits `--max-cost-usd` stops resumably: everything measured stays recorded, the unfinished asset is marked `budget exhausted mid-measurement`, and

```sh
kno value --evals cases.jsonl --pool assets.jsonl \
  --baseline-run-id <id> --agent fake: --resume --yes
```

continues from exactly where it stopped — without paying for anything twice.

## What to watch for

- **The dropped-pairs count.** A Case whose treatment arm errored is dropped, and dropped Cases are exactly the ones where the asset was most harmful (a long injected context that times out). The delta drifts upward when this happens, and the report says how many pairs went missing.
- **Your own conditioning.** If you tagged Cases or wrote assets after reading the baseline's failures, Kno cannot see that — and the deltas can be biased by how often the agent gets those Cases right on a re-run. [ADR-0005](../adr/0005-value-cannot-see-user-side-conditioning.md) says why, and `validate` is the stage that catches it.

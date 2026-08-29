# Choose a portfolio under budget

What you're asking: **which assets earn their place?** `kno value` measures each asset; `kno select` decides — in precedence order — which ones survive your budget, and records the Portfolio with a rejection log that says why every exclusion happened.

## Before you start

You need one recorded run and a budget:

- **A Value run** — from `kno value` (same `--db`). Select reads ONLY the recorded Valuations; it makes no LLM calls, reads no evals, and never touches the holdout.
- **A budget** — the carrying cost of the *selected* set, not what measurement cost. Name at least one cap:
  - `--max-context-tokens` — what the selected context may add per call
  - `--max-training-examples` — how many examples the tuning set may hold
  - `--max-cost-usd` — acquisition dollars for the selected assets
- **A pool** (optional but recommended) — the same `assets.jsonl` you valued. Without it, the content rules (`redundant`, `wrong-mechanism`) cannot run, and the report says they degraded rather than hiding it.

## Run it

```sh
kno select --value-run-id <id-from-value> \
  --pool assets.jsonl \
  --max-context-tokens 10000 --max-cost-usd 5.00
```

## Read the report

```
Select run <id> (completed)
  source    20260828T091515-ffc3097d49da (completed)
  budget    context ≤ 10000 tokens; cost ≤ $5.00

Selected 2 — greedy on delta-per-cost, no approximation guarantee; keep/reject decisions used Bonferroni-corrected intervals
  RANK  ASSET          DESTINATION    DELTA (95% CI)
  1     refund-policy  context        +0.4000  [-0.1000, 0.9000]
  2     pricing-tier   context        +0.3000  [-0.0500, 0.6500]

  dev-estimated gain +0.7000 [-0.1500, 1.5500] (single corrected claim, shared-draw)
  this is a selection-time estimate, inflated by the winner's curse — the honest number is the validate report's holdout gain

Rejected 1
  old-faq       redundant   duplicates refund-policy
```

- **The construction is greedy, and says so.** Feasible, deterministic, and reproducible — no approximation guarantee. A later, cheaper asset can still fit where an earlier one did not.
- **Every decision ran at a corrected interval.** The 95% label on an individual asset is the family-wise error rate over everything screened, not a per-row check.
- **The gain line is a selection-time estimate.** `dev_estimated_gain` is winner's-curse inflated — the selection used up that information. The honest number arrives with `validate`, against the untouched holdout.
- **Rejections are a deliverable.** The reason an asset was excluded is on the record: `regression` (harm), `no-effect` (corrected interval crosses zero), `redundant` (duplicates an already-selected asset), `cost-dominated` (does not fit), `wrong-mechanism` (real effect, wrong vehicle — e.g. knowledge destined for the tuning set).

## If the source run did not complete

`kno select` refuses a budget-stopped or interrupted Value run: ranking an incomplete measurement set as if it were the whole answer would mislead. Either finish the Value run, or pass `--allow-partial` to build from the recorded Valuations anyway — the source's status travels with the Portfolio either way, so a reader cannot mistake it for a completed measurement.

## Keep the run ID

The Portfolio is recorded under the Select run ID, and `kno export` is the next step:

```sh
kno select --json   # machine-readable: run_id, budget, selected, rejected, total_cost
kno export --select-run-id <id-from-select> ...
```

## Next

- [Export a tuning set](export-a-tuning-set.md) — turn the Portfolio into a file the Tuner adapters will parse.

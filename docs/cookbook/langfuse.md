# Measure a Langfuse dataset

What you're asking: **score your agent against the eval sets you already keep in Langfuse**, without a manual export that goes stale the moment the dataset changes.

## What maps where

| Langfuse thing | Kno thing | How |
|---|---|---|
| Dataset | Evals source | `--evals langfuse:<dataset-name>` |
| Dataset item | Case | `input` → the prompt; `expectedOutput` → the expected answer |
| Dataset `updatedAt` | Run resume fingerprint | Folded into the Run's ContentHash, so a re-recorded dataset restarts the run |

An item's `id` is the Case id — stable, deduplicated, and the identity the dev/holdout split is keyed on. The resume fingerprint is the dataset's metadata, not its item ids.

## 1. Point the credentials at the environment

```bash
export LANGFUSE_PUBLIC_KEY=pk-...
export LANGFUSE_SECRET_KEY=sk-...
# self-hosted? also:
export LANGFUSE_HOST=https://langfuse.acme.internal
```

The keys come from the environment, never from a flag or a config file — and Langfuse authenticates every request with *both* of them, as HTTP basic auth. A plain-HTTP or private-address endpoint is refused by default, for the same reason the agent adapters refuse them: the keys ride the connection (basic auth is base64, not encryption). A self-hosted deployment on a private network is reachable with the same two flags a local model server needs, `--allow-insecure-base-url` and `--allow-private-address`.

## 2. Baseline against the dataset

```bash
kno baseline --evals langfuse:my-support-dataset --agent openai:gpt-4.1 --max-cost-usd 5.00
```

Kno resolves the dataset by name (a typo is refused loudly, before anything is fetched), streams the items (paged), maps each row to a Case, assigns the dev/holdout split with the same hash every other source uses, and seals the holdout. The same run records the dataset identity, so a resume against a different version refuses rather than mixes populations.

## 3. Value a pool against the same dataset

```bash
kno value --evals langfuse:my-support-dataset --pool pool.jsonl \
  --baseline-run-id <the run id from step 2> --max-cost-usd 10.00
```

The recipe from there is the [Zendesk one](zendesk.md#5-read-it-back-into-zendesk-decisions): read the table, act on the rows.

## What the mapping does, exactly

- `input` and `expectedOutput` are kept as **canonical JSON**: object keys sorted, numbers preserved literally — the same row always maps to the same Case, which matters when the split hash and the run iterate separately.
- An item whose `expectedOutput` is `null` maps to an **empty expected**, named in the provenance: a judge Goal scores without it, and absence must not become a silent skip.
- An item whose `input` is a message array is kept as canonical JSON rather than turn-mapped — the divergence from the LangSmith adapter is accepted in writing in the adapter's plan.
- An item harvested from a trace (its `sourceObservationId` or `sourceTraceId` set) is marked **derived**, and counts toward the run's weak-label number; hand-authored items are not. See *What the numbers mean* for how this differs from LangSmith.
- A row Kno cannot map, an item without an id, or a duplicate item id mid-stream is **fatal, named** — never skipped, or the denominator behind every delta would silently shrink.
- **ARCHIVED items are filtered out** client-side (the API has no status query parameter).

## Notes

- **Hand-authored fixtures**: the adapter's test fixtures were hand-built against Langfuse's documented schema (this repo's build machine has no live keys). When you run with `KNO_LIVE_TESTS=1`, live tests and fixture re-recording arm.
- **Datasets are not frozen**: an edit between your baseline and your value run changes the value run's set. New items have no baseline score and are dropped from pairs; the run's fingerprint catches version changes, not mid-run edits.

*The general recipe and the vendor table: [Value your Zendesk knowledge](zendesk.md#same-recipe-different-source).*

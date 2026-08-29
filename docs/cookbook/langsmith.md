# Value your LangSmith datasets directly

What you're asking: **measure your agent against the eval sets you already keep in LangSmith**, without a manual export that goes stale the moment the dataset changes.

## What maps where

| LangSmith thing | Kno thing | How |
|---|---|---|
| Dataset | Evals source | `--evals langsmith:<dataset-name>` |
| Example row | Case | `inputs` → the question; `outputs` → the expected answer |
| Dataset version / commit | Run resume fingerprint | Folded into the Run's ContentHash, so a re-recorded dataset restarts the run |

An example's `id` is the Case id — stable, deduplicated, and the identity the dev/holdout split is keyed on. The resume fingerprint is the dataset's metadata, not its case ids.

## 1. Point the credential at the environment

```bash
export LANGSMITH_API_KEY=lsv2_...
# self-hosted? also:
export LANGSMITH_ENDPOINT=https://langsmith.acme.internal
```

The key comes from the environment, never from a flag or a config file. A plain-HTTP or private-address endpoint is refused by default, for the same reason the agent adapters refuse them: the key rides the connection.

## 2. Baseline against the dataset

```bash
kno baseline --evals langsmith:my-support-dataset --agent openai:gpt-4.1 --max-cost-usd 5.00 --yes
```

`--yes` answers the spend confirmation — above a $1.00 estimate Kno asks on a terminal, and a non-TTY run exits `2` without it.

Kno streams the examples (paged), maps each row to a Case, assigns the dev/holdout split with the same hash every other source uses, and seals the holdout. The same run records the dataset identity, so a resume against a different version refuses rather than mixes populations.

## 3. Value a pool against the same dataset

```bash
kno value --evals langsmith:my-support-dataset --pool pool.jsonl \
  --baseline-run-id <the run id from step 2> \
  --agent openai:gpt-4.1 --max-cost-usd 10.00 --yes
```

Value must name the same `--agent` as the baseline — it defaults to `fake:` otherwise, and the deltas would be about a different agent than the one you ship.

The recipe from there is the [Zendesk one](zendesk.md#5-read-it-back-into-zendesk-decisions): read the table, act on the rows. Once `select` and `export` have run, [read the whole story](read-the-whole-story.md).

## What the mapping does, exactly

- `inputs` and `outputs` are decoded as **ordered** JSON: named keys first (`question`, `input`, `answer`, `output`), then document order — the same row always maps to the same Case, which matters when the split hash and the run iterate separately.
- Chat-format datasets: `inputs.messages` concatenated as the question; the expected answer from `outputs.answer`, else the last assistant message.
- A row Kno cannot map is **fatal, named** — never skipped, or the denominator behind every delta would silently shrink.
- A duplicate example id mid-stream is fatal too: it means the dataset was edited under the run, and re-running against a clean version is the fix.

## Notes

- **Hand-authored fixtures**: the adapter's test fixtures were hand-built against LangSmith's documented schema (this repo's build machine has no live key). When you run with `KNO_LIVE_TESTS=1`, live tests and fixture re-recording arm.
- **Datasets are not frozen**: an edit between your baseline and your value run changes the value run's set. New examples have no baseline score and are dropped from pairs; the run's fingerprint catches version changes, not mid-run edits.

*The general recipe and the vendor table: [Value your Zendesk knowledge](zendesk.md#same-recipe-different-source).*

# Measure a Braintrust dataset

What you're asking: **score your agent against the eval sets you already keep in Braintrust**, without a manual export that goes stale the moment the dataset changes.

## What maps where

| Braintrust thing | Kno thing | How |
|---|---|---|
| Dataset | Evals source | `--evals braintrust:<dataset-name>` |
| Dataset event | Case | `input` → the prompt; `expected` → the expected answer |
| Dataset id + newest event's `_xact_id` | Run resume fingerprint | Folded into the Run's ContentHash, so an edited dataset restarts the run |

An event's `id` is the Case id — stable, deduplicated, and the identity the dev/holdout split is keyed on. The resume fingerprint is the dataset's id, its name, and the version counter of its newest event, read with a one-event fetch; Braintrust's dataset object carries no revision field to lean on.

## 1. Point the credentials at the environment

```bash
export BRAINTRUST_API_KEY=bt-...
# self-hosted? also:
export BRAINTRUST_API_BASE_URL=https://braintrust.acme.internal
# key spans orgs? optionally:
export BRAINTRUST_ORG_NAME=acme
```

The key comes from the environment, never from a flag or a config file — a key on a command line lands in shell history, `ps` output, and CI logs. Every request authenticates with it as a Bearer token, which is exactly why a plain-HTTP or private-address endpoint is refused by default, the same policy the agent adapters enforce. A self-hosted deployment on a private network is reachable with the same two flags a local model server needs, `--allow-insecure-base-url` and `--allow-private-address`.

## 2. Baseline against the dataset

```bash
kno baseline --evals braintrust:my-support-dataset --agent openai:gpt-4.1 --max-cost-usd 5.00 --yes
```

`--yes` answers the spend confirmation — above a $1.00 estimate Kno asks on a terminal, and a non-TTY run exits `2` without it.

Kno resolves the dataset by name (a typo is refused loudly, before anything is fetched), streams the events (paged), maps each row to a Case, assigns the dev/holdout split with the same hash every other source uses, and seals the holdout. The same run records the dataset identity, so a resume against a different version refuses rather than mixes populations. Duplicate ids from the pagination's version-history walk — an edit mid-stream re-exposes an older version of a row — are merged, keeping the newest version, never fatal.

## 3. Value a pool against the same dataset

```bash
kno value --evals braintrust:my-support-dataset --pool pool.jsonl \
  --baseline-run-id <the run id from step 2> \
  --agent openai:gpt-4.1 --max-cost-usd 10.00 --yes
```

Value must name the same `--agent` as the baseline — it defaults to `fake:` otherwise, and the deltas would be about a different agent than the one you ship.

The recipe from there is the [Zendesk one](zendesk.md#5-read-it-back-into-zendesk-decisions): read the table, act on the rows. Once `select` and `export` have run, [read the whole story](read-the-whole-story.md).

## What the mapping does, exactly

- `input` and `expected` are kept as **canonical JSON**: object keys sorted, numbers preserved literally — the same row always maps to the same Case, which matters when the split hash and the run iterate separately.
- An event whose `expected` is `null` maps to an **empty expected**, named in the provenance: a judge Goal scores without it, and absence must not become a silent skip.
- An event copied from another object (its `origin` set — Braintrust's record of "copied from an experiment, span, or eval result") is marked **derived**, and counts toward the run's weak-label number; hand-authored events are not. See *What the numbers mean* for how this compares with the other sources.
- A row Kno cannot map, an event with a null `input`, an event without an id or a `_xact_id`, or an oversized row is **fatal, named** — never skipped, or the denominator behind every delta would silently shrink.

## Notes

- **Hand-authored fixtures**: the adapter's test fixtures were hand-built against Braintrust's documented schema (this repo's build machine has no live keys). When you run with `KNO_LIVE_TESTS=1`, live tests and fixture re-recording arm.
- **Datasets are not frozen**: an edit between your baseline and your value run changes the value run's set. New events have no baseline score and are dropped from pairs; the run's fingerprint catches version changes, not mid-run edits.

*The general recipe and the vendor table: [Value your Zendesk knowledge](zendesk.md#same-recipe-different-source).*

# Export a tuning set

What you're asking: **turn the Portfolio into files a downstream pipeline can consume.** `kno select` chose the assets; `kno export` renders them into the destination grammar you name, atomically and idempotently.

## Before you start

You need one recorded run and the pool:

- **A Select run** — from `kno select` (same `--db`), whose Portfolio holds the selected assets and their measurements.
- **A pool** — the same `assets.jsonl` you valued and selected. The Portfolio carries measurements, not content; the pool is the only place content lives, so export cannot render without it.
- **A destination** — one of the three the design ships:
  - `context` — a context-pack manifest plus the rendered pack
  - `knowledge_base` — a manifest plus a human-readable instruction list (writable knowledge-base adapters arrive with v0.2; the manifest says so)
  - `tuning_set` — **OpenAI chat format JSONL**, the exact shape the Tuner adapters will parse

## Run it

```sh
kno export --select-run-id <id-from-select> \
  --pool assets.jsonl \
  --destination tuning_set \
  --out tuning.jsonl
```

## Read the report

```
Export run <id> (completed)
  destination  tuning_set
  wrote        tuning.jsonl (2 assets, 1024 bytes)
  manifest     tuning.jsonl.manifest.md

The artifact is a pure function of the Portfolio and the pool: re-exporting is byte-identical, and export never mutates a destination.
```

## The contract

- **Refusal, not overwrite.** An existing file at `--out` is refused unless you pass `--force`. Nothing is ever silently mutated.
- **Atomic writes.** The artifact (and the manifest beside it at `<out>.manifest.md`) is written temp-then-rename, so a crash mid-export leaves no partial file.
- **Idempotent.** Re-exporting the same Portfolio produces byte-identical files — safe to rerun in a scheduled job, and safe to diff.
- **Derived, never authoritative.** The artifact is a pure function of the Portfolio and the pool; export never changes the Portfolio. The record is the source.

## Machine-readable

```sh
kno export --select-run-id <id> --pool assets.jsonl \
  --destination context --out pack.md --json
```

prints `run_id`, `select_run_id`, `destination`, `asset_count`, `bytes_written`, `path`, and `manifest_path` — the fields a CI step or a `jq` pipeline branches on.

## Next

- [Choose a portfolio under budget](select-a-portfolio.md) — the step before this one.
- [Read the whole story with `kno report`](read-the-whole-story.md) — the export page folds the gaps record into the report.
- Validate (upcoming) measures the Portfolio as a set against the untouched holdout — the honest number that selection-time estimates point at.

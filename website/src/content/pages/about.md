---
title: About Kno
description: >-
  Why Kno exists, what it is deliberately not, and where it is going.
order: 3
---

## Why Kno exists

Agent teams have lots of data and very little evidence about which of it
actually helps. The usual approach is some combination of "put it in RAG",
"stuff it into context", or "fine-tune on it". Kno asks a different
question:

> Did this data actually improve the outcome enough to justify including it?

Traditional evals answer "did my agent get better?" Kno answers "which data
made it better?" — treating data as an experimental variable and measuring
its marginal contribution.

## What Kno is not

- **Not an eval framework.** Kno does not replace your evals; it uses them. Evals judge the agent; Kno judges the data feeding it.
- **Not an observability tool.** It records enough to make its numbers auditable — scores, traces, costs — and nothing more.
- **Not a vector database or ingestion pipeline.** It decides what belongs where; your stack does the moving.
- **Not a hosted service.** A single Go binary, state in a local SQLite file. Nothing leaves your machine except the calls you point at a provider.

## The philosophy, compressed

Every piece of agent data should earn its place. Deltas without intervals
are not reported. Holdouts stay sealed. Cost is part of value. Interrupting
is boring — nothing spends your money silently, and nothing pays twice.

The full argument is in
[DESIGN.md](https://github.com/knograph/kno/blob/main/DESIGN.md) and the
[mental model](https://github.com/knograph/kno/blob/main/docs/mental-model.md).

## Status

Kno is early. `baseline`, `value`, `select`, `export`, `report`, and
`mine` ship; `validate` is next. The full status table lives in the
[README](https://github.com/knograph/kno#status).

## License

Apache-2.0. [Full text](https://github.com/knograph/kno/blob/main/LICENSE).

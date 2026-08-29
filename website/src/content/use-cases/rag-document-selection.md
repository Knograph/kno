---
title: RAG document selection
question: Which documents should go into my RAG system?
summary: >-
  Score each candidate document against your own evals before it reaches the
  knowledge base, and export the keepers as a manifest.
problem: >-
  A RAG index is easy to fill and expensive to correct. Help-center pages,
  policies, and internal docs pile in without evidence — and each wrong
  document costs retrieval quality on every query it matches.
workflow:
  - step: Baseline
    detail: Run your agent over your RAG eval set and record the score.
  - step: Value
    detail: >-
      Each candidate document is injected into the slices it could affect and
      re-measured against fresh controls, producing a delta with a confidence
      interval.
  - step: Select
    detail: >-
      Kno builds a portfolio under your budget and rejects what does not pay
      for itself — no-effect, redundant, and harmful documents get a
      rejection reason instead of a slot.
  - step: Export
    detail: Render the keepers as a knowledge-base manifest.
example: |-
  new_refund_policy.md   +18%   keep → knowledge base
  example_42.json         +7%   keep → context
  example_91.json         +1%   reject
  old_refund_policy.md    -9%   reject → harmful
stages:
  - baseline
  - value
  - select
  - export
ctaLabel: Try it
ctaUrl: '#quickstart'
seoTitle: RAG document selection — measure documents before they enter your index
seoDescription: >-
  Evaluate which documents actually improve your RAG agent before they reach
  the knowledge base, with measured deltas and confidence intervals.
---

## The recipe

Feed the pool from the documents you already have — or from a source you want
to test, like a help center, with the
[Zendesk cookbook recipe](https://github.com/knograph/kno/blob/main/docs/cookbook/zendesk.md).
The pool adapter reads JSONL, CSV, or Markdown, so exporting from any system
is enough to start.

## What you get

A ranked decision per document — keep, reject, or move — with the measured
delta behind it. Selected documents export as a knowledge-base manifest your
ingestion pipeline can consume directly.

## Related reading

- [The mental model](https://github.com/knograph/kno/blob/main/docs/mental-model.md)
- [What the numbers mean](https://github.com/knograph/kno/blob/main/docs/what-the-numbers-mean.md)

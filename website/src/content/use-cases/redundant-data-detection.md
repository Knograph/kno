---
title: Redundant data detection
question: Which data is redundant?
summary: >-
  Duplicate and overlapping knowledge bloat context and confuse retrieval.
  Kno flags redundancy explicitly as a rejection reason, with the asset it
  duplicates.
problem: >-
  Every knowledge base accretes near-duplicates: the FAQ page that restates
  the policy, the example that repeats an older one. Redundancy steals
  budget from data that earns it — and no one notices because no one
  measures.
workflow:
  - step: Value
    detail: >-
      Each asset is measured on its own; near-identical assets produce
      overlapping effects.
  - step: Select
    detail: >-
      The selection pass rejects redundant assets with an explicit reason —
      e.g. `redundant — duplicates refund-policy` — not silently.
  - step: Export
    detail: >-
      Export only the non-redundant portfolio, keeping the rejection log for
      your records.
example: |-
  Rejected 1
    old-faq   redundant   duplicates refund-policy
stages:
  - value
  - select
  - export
ctaLabel: Try it
ctaUrl: '#quickstart'
seoTitle: Redundant data detection — find the knowledge that duplicates itself
seoDescription: >-
  Kno flags redundant data explicitly during selection, with the asset it
  duplicates, so your knowledge base stops paying twice.
---

## The recipe

Feed the whole candidate pool at once — Kno's selection stage compares assets
against each other, not just against the baseline, and records the rejection
log with reasons.

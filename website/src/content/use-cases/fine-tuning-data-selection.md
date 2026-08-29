---
title: Fine-tuning data selection
question: Which conversations should I fine-tune on?
summary: >-
  Fine-tuning amplifies whatever you feed it — including mistakes. Measure
  which examples actually improve the agent before they become weights.
problem: >-
  Fine-tuning runs cost money and bake data in. Teams pick training examples
  by feel — every bad example becomes a learned behavior that is expensive to
  unlearn.
workflow:
  - step: Baseline
    detail: Score the agent over your eval set before any tuning.
  - step: Value
    detail: >-
      Measure each candidate example's marginal effect with a confidence
      interval, against fresh controls.
  - step: Select
    detail: >-
      Build the training portfolio under budget; regressions and redundant
      examples are rejected with a reason.
  - step: Export
    detail: >-
      Render the selected examples as a tuning-set JSONL — re-exporting is
      byte-identical, and export never mutates a destination.
example: |-
  Export run 20260828T233415-8f3a1b2c4d5e (completed)
    destination  tuning_set
    wrote        tuning.jsonl (2 assets, 1024 bytes)
    manifest     tuning.jsonl.manifest.md
stages:
  - baseline
  - value
  - select
  - export
ctaLabel: Try it
ctaUrl: '#quickstart'
seoTitle: Fine-tuning data selection — measure examples before they become weights
seoDescription: >-
  Evaluate which conversations and examples actually improve your agent
  before a fine-tuning run, and export a measured tuning set.
---

## The recipe

Turn production transcripts into an eval set with
[`kno mine`](https://github.com/uknoAI/kno/blob/main/docs/plans/2026-08-29-mine.md),
then value candidate training examples against it the same way as any other
asset.

## What you get

A tuning-set JSONL of measured keepers with a manifest. Note: the `tuned:`
agent adapter is planned — today Kno exports the set, and your existing
training pipeline consumes it.

## Related reading

- [Export a tuning set](https://github.com/uknoAI/kno/blob/main/docs/cookbook/export-a-tuning-set.md)

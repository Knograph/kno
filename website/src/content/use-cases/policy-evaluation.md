---
title: Policy evaluation
question: Is this policy actually improving my agent?
summary: >-
  Policies shape every answer an agent gives. Measure a proposed policy
  change against real cases before — and after — it goes live.
problem: >-
  Policies are cheap to write and expensive when wrong: one bad policy
  degrades every answer in its domain. Teams ship policy changes on
  judgment because there was no fast way to measure them.
workflow:
  - step: Baseline
    detail: Score the agent under the current policy.
  - step: Value
    detail: >-
      Inject the proposed policy into the cases it affects and measure the
      delta with a confidence interval.
  - step: Select
    detail: >-
      Keep it if it pays at a Bonferroni-corrected interval; reject with a
      reason if it regresses or does nothing.
  - step: Export
    detail: Ship the kept policy as part of a context pack.
example: |-
  ASSET              DELTA (95% CI)             CONTROL    NOTE
  refund-policy-v3   +0.4000 [-0.1000, 0.9000]  low 0.0000
stages:
  - baseline
  - value
  - select
  - export
ctaLabel: Try it
ctaUrl: '#quickstart'
seoTitle: Policy evaluation — measure a policy change before it ships
seoDescription: >-
  Inject a proposed policy into the cases it affects and measure the delta
  with confidence intervals, instead of shipping on judgment.
---

## The recipe

Put the proposed policy and the current one in the same pool. Kno measures
each against the same baseline; the comparison falls out of the value table.

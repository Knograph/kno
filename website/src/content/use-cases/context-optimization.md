---
title: Context optimization
question: Which examples deserve space in my context window?
summary: >-
  Context windows are a budget, not a box. Kno ranks candidate examples and
  policies by improvement per token, then selects the portfolio that fits.
problem: >-
  Every few-shot example and policy in your context window costs tokens on
  every single call. Teams add them one at a time and never measure whether
  they pay rent.
workflow:
  - step: Baseline
    detail: Measure the agent without the candidates.
  - step: Value
    detail: >-
      Inject each candidate and record its delta with a confidence interval —
      plus delta-per-cost, so a tiny example beating a giant document is
      visible.
  - step: Select
    detail: >-
      Build a portfolio under a context budget (tokens) and a cost budget,
      greedy on delta-per-cost, with every decision at a Bonferroni-corrected
      interval.
  - step: Export
    detail: Render the selected examples as a context pack.
example: |-
  budget    context ≤ 10000 tokens; cost ≤ $5.00

  RANK  ASSET          DESTINATION  DELTA (95% CI)
  1     refund-policy  context      +0.4000 [-0.1000, 0.9000]
  2     pricing-tier   context      +0.3000 [-0.0500, 0.6500]
stages:
  - baseline
  - value
  - select
  - export
ctaLabel: Try it
ctaUrl: '#quickstart'
seoTitle: Context optimization — which examples earn their token cost
seoDescription: >-
  Rank prompt examples and policies by measured improvement per token, and
  select the context portfolio that fits your budget.
---

## The recipe

Put every candidate example, instruction, and policy in a pool. Kno routes
each one to the cases it could plausibly affect, re-measures against fresh
controls, and reports the delta with its interval.

## What you get

A portfolio under your token budget, ranked by delta-per-cost, with a
rejection log explaining why the rest did not make it.

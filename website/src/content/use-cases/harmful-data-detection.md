---
title: Harmful-data detection
question: Which data is actively hurting my agent?
summary: >-
  Some data makes the agent worse — contradictory policies, outdated
  examples, mismatched tone. Kno measures regression separately from
  improvement, so harm shows up even when gains exist elsewhere.
problem: >-
  A data change that improves one class of requests can break another, and
  most evaluation setups never look for that. Harmful data ships quietly
  and shows up later as support tickets.
workflow:
  - step: Value
    detail: >-
      Every asset is measured against controls, and regression is measured
      separately — an improvement in one slice does not mask harm in
      another.
  - step: Select
    detail: >-
      Assets that hurt get rejected with a `regression` reason and never
      reach the portfolio.
  - step: Report
    detail: >-
      The report records verdicts, the portfolio, the gaps, and the caveat
      that nothing is validated on holdout yet.
example: |-
  old_refund_policy.md   -9%   reject → harmful
stages:
  - baseline
  - value
  - select
  - report
ctaLabel: Try it
ctaUrl: '#quickstart'
seoTitle: Harmful-data detection — find the data that makes your agent worse
seoDescription: >-
  Kno measures regression separately from improvement, so data that hurts
  one slice of requests is caught instead of shipped.
---

## The recipe

Include your outgoing candidates — the policy being replaced, the examples
being retired — in the pool alongside the new ones. Kno values them all the
same way, and the controls catch what plain A/B scoring misses.

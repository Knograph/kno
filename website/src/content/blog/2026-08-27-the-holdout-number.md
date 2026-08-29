---
title: The holdout number is the one you may put in a slide
description: >-
  Why Kno seals a holdout at baseline, reports intervals instead of point
  estimates, and flags small samples as underpowered instead of dressing
  them up as answers.
pubDate: 2026-08-27
author: Kno
tags: [statistics, methodology]
---

Every number Kno reports is built to survive contact with a skeptical
reader. Three rules do most of the work.

## 1. Deltas carry intervals, or they are not reported

A delta without its confidence interval is a point estimate pretending to be
truth. Kno reports 95% intervals, paired per case, and if a sample is too
small to produce a meaningful one — below 20 holdout cases — the run says
**underpowered** and reports no delta at all. A blank is a better answer
than a bad number.

## 2. The holdout stays sealed

A third of your dev set is reserved as a control reserve before routing, and
the holdout is sealed at ingestion — nothing reads it, not valuation, not
selection, until `validate`. Selection is free to overfit the dev set;
that is what selection does. The holdout is the one number that cannot have
been peeked at, and it is the number you may put in a slide.

## 3. The winner's curse is named, not hidden

When you select the best of many candidates, the best looks better than it
is. Kno corrects for the multiplicity — every keep/reject decision uses
Bonferroni-corrected intervals — and then says the quiet part out loud: the
portfolio interval is still inflated by the selection effect. The honest
number is the validate report's holdout gain.

The full treatment — what each number claims and what it does not — is in
[What the numbers mean](https://github.com/knograph/kno/blob/main/docs/what-the-numbers-mean.md).

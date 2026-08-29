---
title: Support-agent knowledge
question: Which knowledge sources actually help my support agent?
summary: >-
  Mine your real support transcripts into an eval set, then measure which
  help-center pages, policies, and canned answers move real cases.
problem: >-
  Support stacks accumulate knowledge — help-center pages, macros, policies —
  and nobody knows which of it actually resolves tickets. The closest proxy
  is usually a hunch.
workflow:
  - step: Mine
    detail: >-
      Turn production transcripts into an eval set. Weak labels are marked
      as derived, content-hash ids keep it stable, and PII warnings keep you
      honest.
  - step: Baseline
    detail: Measure the agent against the mined cases.
  - step: Value
    detail: >-
      Value each knowledge source against real cases with controls and
      confidence intervals.
  - step: Select
    detail: >-
      Keep what pays, reject what is redundant or harmful, and export the
      keepers to a knowledge-base manifest.
example: |-
  # Mine real support transcripts into an eval set
  kno mine --input transcripts.jsonl --mode immediate

  # Then value candidate knowledge against real cases
  kno value --evals mined.jsonl --pool help-center.jsonl \
    --baseline-run-id <run id>
stages:
  - mine
  - baseline
  - value
  - select
  - export
ctaLabel: Try it
ctaUrl: '#quickstart'
seoTitle: Support-agent knowledge — measure which help content resolves real tickets
seoDescription: >-
  Mine support transcripts into evals with kno mine, then measure which
  help-center pages and policies actually improve resolution.
---

## The recipe

There is a full walkthrough in the cookbook:
[Value your Zendesk knowledge](https://github.com/knograph/kno/blob/main/docs/cookbook/zendesk.md) —
the pattern works for any support stack that can export transcripts and
help-center content.

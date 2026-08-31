# Check whether your evals can attribute anything

What you're asking: **before I pay for a baseline and a value run, is my eval set granular enough for Kno to attribute anything to it?** `kno eval inspect` reads an Evals source and reports what routing and the power arithmetic will actually see. It constructs no agent, makes no LLM call, creates no run, and writes nothing.

```sh
kno eval inspect --evals cases.jsonl
```

It exits `0` whatever it finds — this is a diagnostic, not a gate — so it belongs in a pre-commit hook or beside your linter.

> A remote source (`langsmith:`, `langfuse:`, `braintrust:`, `hf:`) does call the vendor's API with the vendor's credentials, because reading the dataset is the job. "Costs nothing" is a claim about LLM spend.

## Reading the page

```text
Evals  cases.jsonl
  60 Cases — 48 dev, 12 held back
  5 distinct behaviors (tags), 3 spellings collapsed into "refunds"

  Everything below reads your tags as behaviors, because that is what routing
  does. If these tags name something else — priority, source, a date — the
  per-tag numbers and suggestions below do not apply to them. Kno cannot tell
  the difference.

BEHAVIOR         DEV CASES  SEPARABLE EFFECT (two-sided 95%)  STATUS
overall_quality         42                              0.22  ok
billing                  6                              0.74  ok
refunds                  6                              0.74  ok
shipping                 3                              1.76  underpowered
tool_use                 3                              1.76  underpowered
```

**The paragraph above the table is not boilerplate.** A behavior, to Kno, is a normalized tag — because that is what the engine clusters and attributes by. It cannot tell a behavior tag from `p0`, `regression-2024`, or `source:zendesk`, and it will report those as distinct behaviors with confident-looking numbers beside them. Everything per-tag below that paragraph is conditional on your tags naming behaviors you would fix separately.

**`SEPARABLE EFFECT` is a bound, not a measurement.** It is the smallest effect that many dev Cases could separate from zero, computed from the sample size and the worst-case paired-binary standard deviation — no data required, which is why it prints before you have any. Three Cases can separate `1.76`, which on a binary Goal means nothing at all. Ten Cases buy you `0.51`. [What a separable effect claims](../what-the-numbers-mean.md#what-a-separable-effect-claims).

**`DEV CASES` is dev-only**, not the behavior's true size. Routing, clustering and every power decision operate on the dev split, so that is the number the arithmetic uses. At the default holdout fraction the behavior is roughly 25% larger than the column says.

## The findings block

```text
  · 25% of dev Cases carry more than one behavior tag — a failure in those
    Cases testifies about every tag it carries, so per-behavior attribution is
    shared.
  ! "overall_quality" is carried by 88% of dev Cases — a catch-all tag under
    which nothing can be attributed.
  ! 2 behaviors have fewer than 5 dev Cases, the minimum a measurement needs
    before it may testify about a behavior at all (core.MinClusterCases).
  ! the holdout has 12 Cases (20 is the minimum for a meaningful interval at
    validate, split.MinHoldout)

3 of 5 checks flagged.
```

| Marker | Meaning |
|---|---|
| `!` | flagged |
| `✓` | ok |
| `?` | unknown — the check needs something it was not given |
| `·` | reported and **never** flagged: there is no principled threshold for it |

The multi-behavior share carries `·` deliberately. It is a real measurement — `cluster()` puts a multi-tagged failed Case into *every* one of its clusters, so the share is exactly how much of the cluster structure is shared — but nothing in Kno anchors a cut-off for it, and a tool built to refuse invented thresholds cannot flag on one of its own.

The headline is a **count**, never a grade. Five checks with five different fixes do not collapse into one word without becoming the "one giant score" this whole page argues against.

## What a run adds

```sh
kno eval inspect --evals cases.jsonl --value-run-id <id>
```

With a recorded Value run, `inspect` adds a fifth answer: the routing mode the run actually used, the control arm's size and its one-sided minimum detectable harm, and one row per failure cluster with the verdict `kno export` computed for it. Without it, that check reports `unknown` — never `ok`. The other four never change: a static property of the eval file must not depend on whether a run happens to exist.

If the eval file has changed since the run, the observed section is withheld and the check says so, rather than joining today's tags to a stale plan.

> The `min_detectable_harm` in that block is **one-sided**; every behavior's `SEPARABLE EFFECT` is **two-sided**. They answer different questions and will not agree. Both are labeled at every appearance.

## Gating CI on it

`inspect` never fails the build on its own. If you want a gate, read the count:

```sh
flagged=$(kno eval inspect --evals cases.jsonl --json | jq .checks_flagged)
[ "$flagged" -le 1 ] || { echo "eval set regressed"; exit 1; }
```

The `checks` array's `name` values (`behaviors_declared`, `behaviors_powered`, `behavior_concentration`, `holdout_powered`, `attribution_observed`) are a stable contract — pin them.

## What it will not tell you

- **Whether a tag is a behavior.** Stated above, and stated in the output.
- **How your score decomposes.** `"overall_quality" accounts for 62% of total score` is not computable in this build: no shipped Goal populates `Score.components`. Case concentration measures the same anti-pattern from data that exists.
- **A grade.** There is no `Attribution quality: MODERATE`, on purpose.

The design guide behind all of it: **[Evaluation design](../evaluation-design.md)**.

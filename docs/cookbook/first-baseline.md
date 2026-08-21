# Score your agent for the first time

Goal: get from nothing to a baseline you can trust, and understand what it says.

## 1. Write some Cases

A Case is one scoreable interaction: an input, and what a correct answer looks like. One JSON object per line.

```jsonl
{"id":"refund-01","input":"How do I get a refund?","expected":"Refunds are processed within 5 business days."}
{"id":"refund-02","input":"Where's my money?","expected":"Refunds are processed within 5 business days."}
{"id":"ship-01","input":"When does my order ship?","expected":"Orders ship within 1 business day."}
```

Every Case needs a stable `id`. That isn't bookkeeping: the dev/holdout split is keyed on it, so a Case without a stable id would have its half decided by its position in the file, and inserting a line above it would silently move it between halves.

Kno refuses a file with a missing id rather than guessing one.

**How many?** Enough that a fifth of them is a meaningful holdout. Below ~100 Cases the holdout starts producing intervals too wide to conclude much from — Kno will run anyway and tell you it's underpowered.

## 2. Run it

```bash
kno baseline --evals cases.jsonl
```

The default agent is `fake:`, which answers deterministically and costs nothing. That's on purpose: you should be able to see the whole loop work before pointing it at something that bills you.

## 3. Read the output

```
Baseline 20260821T083017-d2dfc5377255
  cases      44 scored, 0 errored (of 44 dev; 6 held back)
  score      1.000
  spent      $0.00 over 44 call(s)
  status     completed

  warning: the holdout has only 6 cases, too few for a meaningful confidence interval at validate

Next: `kno value` to measure which of your assets earn their place.
```

- **`6 held back`** — never scored, and nothing will read them until `validate`. This is what makes the eventual number honest.
- **`0 errored`** — Cases where the agent didn't answer at all. Counted separately and excluded from the score, because a 500 isn't a wrong answer.
- **`score 1.000`** — the mean over scored Cases. The fake echoes the expected answer, so of course it's perfect. Your real agent will not be.
- **The warning** — read it. A six-case holdout can't support a meaningful interval.

## 4. Keep the run ID

```bash
kno baseline --evals cases.jsonl --run-id nightly-2026-08-21
```

Runs are the correlation key for every trace, score, and event. Naming them yourself makes a resume — and later, a comparison — straightforward.

## Common problems

**"case has no id"** — every Case needs one. See step 1.

**"all N Cases landed in dev, leaving no holdout"** — your eval set is too small for the configured fraction. Add Cases, or raise `--holdout-frac`. Kno refuses here rather than at `validate`, so you find out before spending anything.

**"duplicate case id"** — two Cases share an id. Since the split is keyed on id they'd land in the same half and be indistinguishable in every later report, so this is fatal rather than tolerated.

**The score looks too good** — check `errored`. If most Cases failed to answer, the score is over the handful that succeeded, and Kno will have marked the run unusable as a reference.

## Next

- [What the numbers mean](../what-the-numbers-mean.md) — before you act on a score.
- [Gate a deploy on Kno in CI](ci-gate.md).

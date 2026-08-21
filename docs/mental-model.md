# The mental model

One page. Read it and the rest of Kno should be obvious.

## The question Kno answers

Not *did my agent fail* — plenty of tools tell you that. The question is economic:

> **Which data assets are actually worth including — in the context, in the knowledge base, in the fine-tuning set — to move the outcome I care about, and which are dead weight or actively harmful?**

Everything below exists to answer that honestly.

## Ten words

Used identically in the CLI, the API, the SDKs, the code, and these docs. If you learn these, you can read anything in the project.

| Term | Meaning |
|---|---|
| **Case** | One scoreable eval interaction — an input plus an expected outcome or a rubric |
| **Evals** | The set of Cases. Split **dev/holdout** at ingestion; the holdout is untouched until Validate |
| **Asset** | One candidate data unit — an example, document, fact, or feature — carrying its own cost |
| **Pool** | The collection of candidate Assets under consideration |
| **Goal** | The outcome metric, with a direction. Composable and weighted |
| **Valuation** | An Asset's measured record: Δgoal with a confidence interval, control Δ, cost, injection mode |
| **Portfolio** | The selected subset of Assets, chosen under budget, with a rejection log |
| **Destination** | Where an Asset belongs: `context`, `knowledge_base`, or `tuning_set` |
| **Bridge** | The measurement funnel connecting in-context valuations to fine-tuning outcomes |
| **Holdout** | The untouched eval slice that produces the only number you may put in a slide |

Two of these are worth pausing on.

**Evals and Pool are different things.** The exam and the study material. Cases are what you test on; Assets are what you might add. They come from different places and have different adapters, which is why `--evals` and `--pool` are separate flags and never interchangeable.

**Holdout is not a validation set in the loose sense.** It is a slice that nothing reads — not the baseline, not valuation, not selection — until the Validate stage. That constraint is the reason any later number means anything.

## The five stages

```
             ┌──────────┐   ┌───────────┐   ┌──────────┐   ┌──────────┐   ┌──────────┐
 Evals   ──▶ │ BASELINE │──▶│   VALUE   │──▶│  SELECT  │──▶│ VALIDATE │──▶│  EXPORT  │
 Agent   ──▶ │ run+score│   │ route,    │   │ portfolio│   │ portfolio│   │ JSONL +  │
 Goal    ──▶ │ all cases│   │ marginal  │   │ under    │   │ as a SET │   │ report + │
 Pool    ──▶ │ (dev/    │   │ Δ & cost  │   │ budget   │   │ on       │   │ gaps     │
             │ holdout) │   │ per asset │   │          │   │ holdout  │   │          │
             └──────────┘   └───────────┘   └──────────┘   └──────────┘   └──────────┘
```

**Baseline** — run the agent on the Cases, score against the Goal, persist every trace. This is the reference every later delta is measured against.

**Value** — for each Asset: route it to the failure slices it could plausibly affect, classify it as knowledge or behavior, inject it, re-run the affected slices *plus untouched controls*, and record the delta with a confidence interval. Ranked by **Δgoal per unit cost**, not raw Δgoal — a mediocre 200-token asset can and should out-rank a strong 8,000-token one.

**Select** — build the Portfolio under budget: greedy on Δ-per-cost with redundancy penalties. Outputs the selection *and* the rejection log. "Include nothing new" is a legal, first-class answer.

**Validate** — the Portfolio ships as a set, so it is measured as a set, against the holdout. Two individually-helpful documents can be jointly contradictory. This produces the honest number.

**Export** — the training set, the report, and the gaps: failure clusters that *no Asset in your pool* improved, which is the tool telling you what to start collecting.

Today only Baseline is implemented.

## Why dev/holdout exists

Suppose you measure 500 assets against 400 cases and keep the 30 that helped most.

Some of those 30 helped because they genuinely help. Others helped because 400 cases is a finite sample and noise has to land somewhere. You cannot tell which is which **from the data you selected on** — the selection already used up that information. This is the winner's curse, and it is not a small effect: the more assets you screen, the more the top of your ranking is inflated by luck.

The holdout is the fix. A slice is separated at ingestion, nothing reads it during measurement or selection, and the Portfolio meets it exactly once, at Validate. The number that comes out is the only one that hasn't been optimized against.

Kno enforces this rather than documenting it:

- The split happens at ingestion, keyed on Case ID, so it is stable across runs and adding cases never moves the ones already there.
- `Baseline` accepts a `SealedEvals` — a distinct Go type. A stage that could read the holdout **does not compile**.
- A Case with no assigned split is filtered out too, because treating "unknown" as "dev" is how a holdout leaks one case at a time.

## Injection modes: what a number is a claim about

There are two honest ways to measure an Asset, and they are not interchangeable.

**`context_injection`** puts the Asset directly in the prompt, bypassing retrieval. This is an **upper bound**: what the Asset could contribute if retrieval were perfect. It is cheap and it is not a deployment prediction.

**`knowledge_add`** writes the Asset into the agent's own index and reaches it through the real retriever. **Deployment-faithful**, and only possible with an agent whose index Kno can write.

Every Valuation is labeled with the mode used, and context-mode results are reported as bounds. The distinction also preserves something context injection alone destroys: whether a failure was *missing data* or a *retrieval miss*. Those need completely different fixes.

## Knowledge vs behavior, and why routing comes first

Fine-tuning is a poor vehicle for knowledge — retention is unreliable and you can't patch a stale fact. It is the right vehicle for behavior: format, tone, tool-use patterns, reasoning demonstrations. In-context learning is the reverse.

So Kno classifies each Asset before measuring it. Knowledge assets are valued by context or knowledge injection, where in-context measurement is faithful, and recommended for RAG or context — never the tuning set. Only behavior assets face the fine-tuning bridge at all.

This is why the tool can tell you **where** an asset belongs, not just whether it's good.

## Cost is part of the measurement

Every Asset carries a cost vector: context tokens (recurring, per call), fine-tuning tokens (one-time), acquisition cost, and a staleness flag. The ranking metric is improvement per unit cost.

Money is tracked in integer micro-dollars end to end. Floating-point dollars accumulate error across thousands of calls, and Kno decides whether to spend on these numbers.

## Errors are not scores

A Case where the agent returned a 500 is counted separately and excluded from the score. The agent didn't answer *badly* — it didn't answer. Scoring infrastructure failure as task failure biases the baseline downward, which makes every asset you later measure look better than it is.

Every run reports three counts — attempted, scored, errored — so the exclusion is visible rather than implied. A run whose error rate is too high is marked unusable as a reference rather than silently treated as clean.

## Interruption is boring

Work is checkpointed as each Case completes, in one transaction with its result — there is no separate "done" marker that a crash could leave disagreeing with the data. Resume skips what's finished and reconstructs prior spend from disk, so an interrupted run cannot spend its budget twice.

## Where to go next

- **[What the numbers mean](what-the-numbers-mean.md)** — confidence intervals, the winner's curse in detail, and what a delta does not tell you.
- **[Cookbook](cookbook/)** — task-shaped recipes.
- **[DESIGN.md](../DESIGN.md)** — the architecture, and what is deliberately out of scope.

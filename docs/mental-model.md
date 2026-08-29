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

**Select** — build the Portfolio under budget: greedy on Δ-per-cost, honestly labeled — feasible, deterministic, reproducible, and no approximation guarantee. Every keep/reject decision runs at a Bonferroni-corrected interval, in precedence order (regression, no effect, redundant, cost-dominated, wrong mechanism), and the output is the selection *and* the rejection log. "Include nothing new" is a legal, first-class answer. The portfolio-level gain is a single corrected claim, winner's-curse inflation included — it is a selection-time estimate, not a result.

**Validate** — the Portfolio ships as a set, so it is measured as a set, against the holdout. Two individually-helpful documents can be jointly contradictory. This produces the honest number.

**Export** — write the selected assets into the destination grammar: `context` (a context-pack manifest plus the rendered pack), `knowledge_base` (a manifest plus an instruction list; writable knowledge-base adapters arrive with v0.2), or `tuning_set` (OpenAI chat format JSONL, the shape the Tuner adapters parse). Re-exporting the same Portfolio is byte-identical, and export never mutates a destination.

Today Baseline, Value, Select, and Export are implemented; Validate is next. The
[README Status table](../README.md#status) is the canonical state.

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

## Routing: which Cases measure which Asset

Measuring every Asset against every Case is the obvious design and nobody can afford it. 200 Assets over a 500-Case dev split is 100,000 measurements before you have learned anything.

So Kno **routes**. It clusters the dev Cases your baseline failed by tag, and measures an Asset against the clusters its own tags overlap. An Asset that matches nothing routes to nothing, costs nothing, and is reported as irrelevant — the cheapest valuable answer the stage gives you.

Three fallbacks, because the common case is that nobody has tagged anything:

- **An Asset with no tags** is measured against a sample of every failed Case. Unlabelled is not irrelevant.
- **No Case carries a tag** — the default state of a real eval file — and there are no clusters to overlap. Every Asset is measured against a sample of the failed Cases. This is decided before you are asked to approve the cost, so the number you approve is the number this path costs.
- **Nothing failed.** There is no failure signal to route on, so every Asset is measured against a sample of everything.

Routing decides the candidates. `--sample-rate` bounds the draw from them. Both are reported.

### Why a slice of the Cases is reserved before any of this

Routing to the Cases your baseline failed is what makes the stage affordable. It is also a trap, and it is worth understanding because the trap looks like a free win.

If Kno picks the Cases your baseline failed, and then compares the Asset against *those recorded failures*, it is reusing the same draw twice: once to choose the Case, once as the thing to beat. Every recorded score on that slice is zero by construction, so an Asset that does **nothing at all** measures a large positive gain. At a 70%-pass agent it measures **+0.70**, with a tight interval and a confident-looking report.

Kno avoids this two ways. On the routed Cases it measures a **fresh** control — the Asset's absence, measured now, not read from the baseline file. That doubles the cost of the routed arm, and it is what the delta being real is bought with.

And **before routing runs at all**, Kno sets aside a random slice of your dev Cases that routing never touches. Because that slice was chosen without looking at any outcome, your recorded baseline *is* a valid control there, and it costs one measurement per Case instead of two. That reserved slice is where the regression check lives.

### The regression check, and when it is not one

The reserved slice answers a different question from the routed one: not "did this help", but **"did this break something else"**. An Asset that fixes its own slice and quietly damages another is the failure mode worth paying to catch.

That question is one-sided — you care about harm, not about symmetric uncertainty — and it needs enough Cases to answer. Small samples answer it badly, and badly in the direction that reads as good news: the interval crosses zero, and "no regression found" is what a wide interval looks like.

So Kno reports **the smallest regression the run could actually have seen**, rather than only a pass/fail badge. Roughly:

| Control Cases | Smallest detectable regression |
|---|---|
| 10 | 0.40 |
| 20 | 0.27 |
| 60 | 0.15 |
| 100 | 0.12 |
| 300 | 0.07 |

Read that as a limit, not a score. A run with 30 control Cases that reports no regression has **not** cleared an Asset that costs you 10 points — it could not have seen one. The flag fires below 20 Cases or whenever the honest bound sits above the harm margin, so on most real runs it is on — that is deliberate: the number travels first and the flag travels beside it. Treat the flag as a floor rather than a certificate: above it a run is not "powered", it is merely not absurd, and it is the bound — not the flag — that says what was actually detectable.

This is why the reserved slice is a third of your dev split rather than a token sample, and why a bigger eval set buys you a sharper answer to "did this break something" even though it does not change what routing costs.

**`--route none`** switches routing off: every Asset is measured against a sample of everything. It does not remove the regression check — the reserved slice is drawn before routing and does not depend on it — and it drops the fresh control arm, because a random sample was never conditioned on your baseline's outcomes in the first place.

### What Kno cannot see

All of the above protects you from *Kno's* conditioning. It cannot protect you from your own.

If you read your baseline's failure report and then tag those Cases, or write Assets aimed at them, or run `kno value` twice and adjust in between, then the Cases being measured were chosen using the baseline's outcomes — through a channel that looks identical to honest labelling. Kno sees a tag string and an Asset file. There is no signal in either that separates "these Cases share a topic" from "these Cases are the ones that failed."

The instrument for that is the holdout, which is why `validate` is a separate stage. See [what the numbers mean](what-the-numbers-mean.md).

## Cost is part of the measurement

Every Asset carries a cost vector: context tokens (recurring, per call), fine-tuning tokens (one-time), acquisition cost, and a staleness flag. The ranking metric is improvement per unit cost.

Money is tracked in integer micro-dollars end to end. Floating-point dollars accumulate error across thousands of calls, and Kno decides whether to spend on these numbers.

### Where a cost figure comes from

A reported cost is **reported usage priced against a dated table** — the token counts the provider returned, multiplied by the rates in Kno's price table on the date it was built. It is not an invoice, and the two can differ: discounts, committed-use pricing, and cached-input rates are all things your provider knows and this table does not. `kno doctor` prints the table's date.

Where the provider reports no usage, Kno records its own estimate and says so, rather than presenting a guess as a measurement.

### What a cost cap actually bounds

`--max-cost-usd` bounds **what Kno authorizes before each call**, using a per-Case estimate. It is honest in one direction: the estimate is an upper bound — prompt tokens plus the full output ceiling — so a run stops at or before the cap, never after. The cost of that honesty is that a run with a generous `--max-output-tokens` reserves against output it will probably not generate, so it may stop earlier than the money spent suggests.

A cap Kno cannot compute an estimate for is refused rather than accepted and checked later. A cap discovered at settlement is a cap discovered after the money is gone, which is not a cap.

## Errors are not scores

A Case where the agent returned a 500 is counted separately and excluded from the score. The agent didn't answer *badly* — it didn't answer. Scoring infrastructure failure as task failure biases the baseline downward, which makes every asset you later measure look better than it is.

Every run reports three counts — attempted, scored, errored — so the exclusion is visible rather than implied. A run whose error rate is too high is marked unusable as a reference rather than silently treated as clean.

## Interruption is boring

Work is checkpointed as each Case completes, in one transaction with its result — there is no separate "done" marker that a crash could leave disagreeing with the data. Resume skips what's finished and reconstructs prior spend from disk, so an interrupted run cannot spend its budget twice.

An interrupted run exits `4`, not `1`: it is resumable, not broken, and a CI gate should tell those apart. A second Ctrl-C during the shutdown drain kills the process the ordinary way.

The same design decides how deletion works. `kno purge` removes the agent's output and the judge's rationale — the parts that can be conversation content — and keeps the score, the cost, and the completion record. It never deletes a row, because deleting the row deletes the done-marker, and a purged run would then pay for every Case a second time. A privacy feature that costs you money is not one.

Resuming is refused if the evals, the goal, or the agent changed — averaging Cases measured under two configurations into one number is the same corruption the holdout rule exists to prevent, arriving through a different door.

## Where to go next

- **[What the numbers mean](what-the-numbers-mean.md)** — confidence intervals, the winner's curse in detail, and what a delta does not tell you.
- **[Cookbook](cookbook/)** — task-shaped recipes.
- **[DESIGN.md](../DESIGN.md)** — the architecture, and what is deliberately out of scope.

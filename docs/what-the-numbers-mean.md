# What the numbers mean

Kno reports numbers about your data. This page says exactly what each one claims, and — more usefully — what it does not.

Publishing this is deliberate. A measurement tool that hides its epistemics is asking you to trust it, and you shouldn't.

## The short version

| Number | What it claims | What it does not claim |
|---|---|---|
| Baseline score | The agent's mean score over the Cases it successfully answered, across the whole run including any resumed part | Anything about Cases it failed to answer, or about the holdout |
| Δgoal on an Asset | The change on the slices that Asset was routed to, in the mode stated | That the same change happens in deployment, unless the mode is `knowledge_add` |
| Δ on controls | Whether the Asset hurt slices it wasn't meant to help | That no regression exists outside the measured controls |
| Portfolio dev estimate | The selected set's gain **on the slice it was selected against** | That you will see this gain. It is inflated. See below |
| Holdout gain | The set's gain on Cases nothing had read before | Anything about Cases outside your eval set |

## Why the dev estimate is inflated

Screen 500 assets against 400 cases and keep the 30 that scored highest.

Some of those helped genuinely. Others helped because 400 cases is a finite sample, noise lands somewhere, and you selected on exactly the slice where it landed. You cannot separate the two using the data you selected on — the selection consumed that information.

This is the **winner's curse**, and it scales with how many assets you screen. Screening more candidates makes the top of your ranking *more* inflated, not less. It is why a tool that reports only the selection-time number is flattering you.

Kno names this field `dev_estimated_gain` rather than `expected_gain` so that an SDK user reaching for it by autocomplete is warned by the name and not only by the docs.

**The holdout gain is the number that isn't inflated**, because nothing optimized against it.

## What a confidence interval here is, and isn't

Every reported delta carries an interval, or the delta is not reported at all. A `Valuation` with a delta and no interval is a bug, not a shortcut.

The interval covers **sampling variation across your eval set and across repeated trials**. It does not cover:

- **Whether your eval set represents production.** If your Cases are unrepresentative, every number here is precise and wrong. Kno cannot detect this, and no confidence interval will warn you.
- **Judge error.** When a Goal uses an LLM judge, judge disagreement is a source of error the interval does not include. Calibrate against a human-labeled set (`kno judge calibrate`) before trusting judged numbers.
- **Provider drift.** A model updated between your baseline and your validation makes the comparison invalid in a way no interval expresses.

**An interval crossing zero means the effect is indistinguishable from nothing at your sample size.** It does not mean the asset is useless — it means you don't have enough evidence. More cases or more trials would narrow it.

Kno colors deltas by whether the interval crosses zero, not by sign. A positive delta whose interval spans zero is not a positive result.

## When the holdout is too small

Below 20 Cases, Kno marks the holdout **underpowered** and says so in the output.

A holdout of six can only produce a very wide interval — wide enough that most real effects are indistinguishable from zero. The run still executes, because refusing would make the tool unusable while you're experimenting. But the caveat travels attached to the number rather than living in a doc you might not read.

If your holdout is underpowered, the honest reading of a holdout gain is "consistent with anything in this wide range," not the point estimate.

## Errors are excluded, and why that's the lesser evil

Cases where the agent failed to answer are counted separately and left out of the score.

The alternative — scoring an error as zero — treats infrastructure failure as task failure. That biases the baseline downward, and since every later delta is measured against the baseline, it makes every asset look better than it is.

Exclusion has its own hazard: if the errors aren't random — if hard Cases are the ones timing out — then the surviving sample is easier than the real one and the baseline is biased *up*. Kno cannot detect that either. What it does instead:

- Reports all three counts, so exclusion is visible.
- Marks a run whose error rate exceeds a threshold (5% by default) as **not a usable baseline**, so later stages refuse to treat it as a clean reference.
- Requires that any delta be computed over the intersection of Cases scored in both runs, so a provider outage in one run can't silently change the population being compared.

## A purged run has no baseline score

`kno purge` erases stored conversation content. It preserves the numbers — the score of
each Case survives the blob it arrived in — so a purged run still reports its baseline.

Runs purged by a build older than the score column are the exception. Those Cases are
complete and their scores are gone, and there is no way to recompute them without
re-running the agent. Kno reports **no baseline score at all** for such a run, and says
why, rather than reporting the mean over the Cases that happen to still have numbers.

That mean would be a real number describing a population nobody chose: whichever Cases
escaped a purge. Its counts would span the whole run and its value would span a subset,
which is the same defect as the resumed-run bug this replaced — a number and its
denominator describing different things.

## Context injection is an upper bound

A Δgoal measured with `context_injection` says: *if this Asset were reliably in the prompt, the score would move this much.*

In deployment it usually isn't reliably in the prompt — it's behind a retriever that may or may not surface it. So the context-mode number is a ceiling, and Kno reports it as a bound rather than a prediction.

`knowledge_add` mode writes into the agent's real index and measures through the real retriever. That number *is* a deployment prediction, and it requires an agent whose index Kno can write.

## The fine-tuning bridge is a selection signal, not a guarantee

In-context gains don't reliably predict fine-tuning gains. They're different mechanisms: ICL favors knowledge injection, fine-tuning favors behavior and format, and they diverge precisely where a naive tool would mislead you.

Kno's response, in order of increasing faithfulness and cost:

1. **Route by mechanism.** Knowledge assets never go to the tuning set. This dissolves most of the gap rather than measuring across it.
2. **Screen with ICL.** High recall, low precision — kills the no-effect and regressive assets cheaply.
3. **Proxy fine-tune.** LoRA on a small open model, group ablations. Proxy-to-target transfer is imperfect and well-validated *as a selection signal*. It also gives an interference read — whether tuning on a group regresses your controls — which in-context measurement categorically cannot.
4. **Post-tune validate.** Score your actually-tuned model against the same untouched holdout. This is the honest before/after.

A number from step 3 is a *ranking* signal. Only step 4 is a result.

## What Kno cannot tell you

Stated plainly, because a tool that lists its limits is easier to trust than one that doesn't:

- Whether your eval set reflects production.
- Whether your Goal measures what you actually care about.
- Whether an asset that helps today still helps after a model update.
- Whether an asset is *correct* — only whether it moves your Goal. A confidently wrong document that happens to match your rubric will score well.
- Anything about data you didn't put in the pool. The gaps report tells you where your pool was insufficient; it can't tell you what would have worked.

## If you only remember one thing

**The holdout number is the one you may put in a slide.** Everything else is a measurement in service of producing it.

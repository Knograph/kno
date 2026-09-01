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
| Holdout gain | The set's gain on Cases nothing had read before, reported only beside its interval | Anything about Cases outside your eval set. It is measured by injecting the set into the prompt, so it is still an upper bound |

## Why the dev estimate is inflated

Screen 500 assets against 400 cases and keep the 30 that scored highest.

Some of those helped genuinely. Others helped because 400 cases is a finite sample, noise lands somewhere, and you selected on exactly the slice where it landed. You cannot separate the two using the data you selected on — the selection consumed that information.

This is the **winner's curse**, and it scales with how many assets you screen. Screening more candidates makes the top of your ranking *more* inflated, not less. It is why a tool that reports only the selection-time number is flattering you.

Kno names this field `dev_estimated_gain` rather than `expected_gain` so that an SDK user reaching for it by autocomplete is warned by the name and not only by the docs.

**The holdout gain is the number that isn't inflated**, because nothing optimized against it.

## What a holdout gain claims

`kno validate` runs the holdout Cases twice inside one run: a **control arm** with nothing injected, and a **treatment arm** carrying the whole context-destination Portfolio, injected as a single ordered payload in the system position. The reported number is the mean paired difference between the two arms, sign-corrected by the Goal's direction, over the Cases that scored in both:

```
holdout_gain = mean over holdout Cases of ( score(treatment, c) - score(control, c) )
```

**Why the control arm is measured rather than read off the baseline.** It cannot be read off the baseline: `core.Baseline` is handed a sealed, dev-only view of your Evals, so no baseline run has ever scored a holdout Case, and there is no recorded holdout reference in the database to subtract. Subtracting the *dev* baseline mean instead would produce one number carrying three terms — the Portfolio's effect, plus a dev-versus-holdout population difference, plus provider drift between two runs on different days. The population term is not zero and not small: the split is a hash of the Case ID, so which Cases land where is random, and at a 20-Case holdout that randomness is easily larger than any Portfolio effect. Two arms cost twice the agent calls, and the consent quote shows the doubling (`n_holdout x 2 arms x trials`) rather than burying it. That is the price of the number meaning one thing instead of three.

**What it is unbiased for:** the effect of *this* Portfolio on the holdout population. Nothing optimized against these Cases, so the selection effect that inflates `dev_estimated_gain` is absent.

**What it is not unbiased for:** the effect of the best achievable portfolio. You selected one set out of your Pool, and the holdout says how that one did — not how close it came to the best one available. And it is **not a bias-corrected `dev_estimated_gain`**. The two are different measurements of different slices, not one quantity measured twice; the smaller number is not the "real version" of the bigger one.

**Why the holdout number usually comes in under the dev estimate.** Three contributors, and Kno claims nothing about their relative sizes, because it cannot separate them:

- **Winner's-curse shrinkage.** The dev estimate was computed on the slice the selection ran against, so it carries the inflation described above. Some of the gap is that inflation going away.
- **Sampling noise on both slices.** Two finite samples, two intervals. The gap between two point estimates includes the noise in each of them, and at holdout sizes near `MinHoldout` that noise is the wider term.
- **Estimand mismatch.** `dev_estimated_gain` is a dev-population figure built from per-entry deltas, each scaled by `PortfolioEntry.n_routed_scale` under the uniform-effect model described in [What a scaled delta claims](#what-a-scaled-delta-claims). `holdout_gain` is a direct, unscaled measurement of the whole set over every holdout Case. They are not the same quantity, so their difference is not a bias estimate.

That third one is the single most likely misreading of the stage, which is why the report prints it in line beside the two numbers rather than leaving it on this page. The labelled difference between them is `shrinkage`, and shrinkage is the expected outcome of a correct pipeline — it is not evidence that your Assets interfere with each other.

**Bonferroni is deliberately absent here.** `select` corrects every keep/reject interval for the family of decisions it made, because it screened `n_screened` Assets and kept the winners. Validate screens nothing: it makes one comparison, fixed before the holdout was read — this Portfolio, this holdout. A correction factor would widen the one interval in the product that has actually earned its nominal 95% coverage. If you go looking for `stats/portfolio.Correct` in the validate path, its absence is the decision; a test pins it so that a future reader cannot "fix" it.

**The multiplicity that does apply is repeated holdout use, and it is counted rather than corrected.** A holdout is consumed once per Portfolio. The consumption is recorded before the first agent call, not at completion, because a validate run that died halfway has already peeked — recording at the end would be the leak. Re-validating the same Portfolio against the same holdout is refused outright and there is no flag for it; an interrupted run is resumed, which reads only the Cases the first process never reached. Validating a *different* Portfolio against the same holdout is allowed, with `--allow-repeat-holdout`, and every rendering carries the ordinal: `this holdout has now measured N portfolios`. Kno counts that and does not widen the interval for it, and you should read the Nth number with that in mind — validating N portfolios against one holdout re-introduces multiplicity at rate N, which is the thing the holdout existed to remove. The reason it is allowed at all is that refusing would push you to delete the database or re-split with `--split-seed`, turning a counted second peek into an invisible one.

**The verdict keys on the interval, never on the sign** — the same rule as everywhere else on this page. An interval entirely above zero is `confirmed`; an interval crossing zero is `inconclusive`, which is "not enough evidence at this sample size" and does not fail a deploy gate unless you ask for proof with `--require-gain`; an interval entirely below zero is `not_confirmed` and blocks unconditionally. A holdout too small or too ragged to form an interval reports `unmeasured` and no number, never a bare point estimate.

**And it is still an upper bound.** The treatment arm puts the Portfolio in the prompt, so this is a `context_injection` measurement and everything in [Context injection is an upper bound](#context-injection-is-an-upper-bound) applies to it — a ceiling on what a retriever would deliver, not a deployment prediction. A Portfolio whose entries span more than one Destination is refused rather than approximated; `--context-only` validates the context subset and labels the result as a number about a subset, which is a true number about part of what you are shipping.

## What a corrected interval claims

`select` decides under **Bonferroni correction**: every keep/reject interval is computed at level `1 − (1 − 0.95)/n_screened`, so the error budget is the family of decisions, not any one of them. An asset whose corrected interval crosses zero is rejected as `no-effect` — decisively, not advisory. The portfolio-level `dev_estimated_gain` interval is one corrected claim over the whole selection, combined under the shared-baseline covariance (the deltas are positively correlated through the shared draw, so its half-widths combine linearly, never as independent intervals), and its `method` is recorded as `portfolio-greedy-shared`.

Two things follow. First, the reported 95% intervals on individual assets are *wider than they look*: each one was checked against the whole screened family before it was reported. Second, the portfolio interval is still winner's-curse inflated — correction removes the multiplicity illusion, not the selection effect — so the discipline here reduces the flattery, it does not remove it.

## What a scaled delta claims

A knowledge asset measured with `n_routed` cases of `n_dev` has its delta scaled by `n_routed / n_dev` before it is ranked and reported. That scaling is sound under exactly one model: **the effect is uniform on the routed slice and zero elsewhere.** Under a model where the effect varies by slice — the asset genuinely helps some cases and hurts others — the scaled number overstates the routed-slice effect. Kno records the factor per entry (`n_routed_scale`), and an asset whose `n_routed` is absent is never silently divided: it is flagged as unscaled. Read a scaled delta as "what the routed slice alone would contribute if the effect were uniform," and check the recorded factor before comparing two assets with different routing.

## What a confidence interval here is, and isn't

Every reported delta carries an interval, or the delta is not reported at all. A `Valuation` with a delta and no interval is a bug, not a shortcut.

The interval covers **sampling variation across your eval set and across repeated trials**. It does not cover:

- **Whether your eval set represents production.** If your Cases are unrepresentative, every number here is precise and wrong. Kno cannot detect this, and no confidence interval will warn you.
- **Judge error.** When a Goal uses an LLM judge, judge disagreement is a source of error the interval does not include — and it is not noise that averages out, it is a systematic *shrinkage* of every delta toward zero. `kno judge calibrate` measures how big it is; [What a judge's kappa claims](#what-a-judges-kappa-claims) says what the number means.
- **Provider drift.** A model updated between your baseline and your validation makes the comparison invalid in a way no interval expresses.

**An interval crossing zero means the effect is indistinguishable from nothing at your sample size.** It does not mean the asset is useless — it means you don't have enough evidence. More cases or more trials would narrow it.

Kno colors deltas by whether the interval crosses zero, not by sign. A positive delta whose interval spans zero is not a positive result.

## What a judge's kappa claims

`kno judge calibrate` reports **Cohen's kappa** between a Goal's verdicts and a human-labeled
calibration set, with a percentile-bootstrap confidence interval, and gates on it.

**Not accuracy, and the reason is the failure it hides.** On a set that is 85% "good", a judge
that answers "good" unconditionally scores **0.85 raw agreement** and is worthless. That same
judge scores **kappa = 0, exactly**. Raw agreement is still printed — it is the number a
non-statistician reads correctly at a glance — and the constant judge's score is printed
beside it, because seeing the two together is the lesson.

### Kappa is the factor your effects are multiplied by

This is what makes kappa the right statistic here rather than a conventional one: it is
denominated in the units of the thing Kno sells.

Let the judge misclassify at rate ε in both directions, independently of which arm a Case is
in. The observed pass rate is then `p* = ε + p(1 − 2ε)`, so for any two arms:

    p*_treat − p*_control = (1 − 2ε) · (p_treat − p_control)

**Every delta measured through this judge is attenuated by exactly (1 − 2ε).** On a balanced
set the judge's own marginals are balanced too, so chance agreement `p_e` is 0.5 and observed
agreement `p_o` is `1 − ε`:

    kappa = (1 − ε − 0.5) / 0.5 = 1 − 2ε

Kappa *is* the attenuation factor. A judge at kappa 0.80 shrinks a true 10-point improvement
into an 8-point measured one.

### Why the floor is 0.60, and what it costs you

**It is not Landis and Koch.** Their "substantial" band starts at 0.61, and the near-coincidence
is named here only so that nobody mistakes it for the argument. A threshold imported from a 1977
paper about medical raters is an invented threshold wearing a citation.

The floor is read off the attenuation instead. Power for a paired comparison scales with the
*square* of the effect size, so attenuating an effect by a factor `a` requires `1/a²` times as
many Cases to reach the same power. Setting the ceiling at **"a judge may cost at most 3× your
eval budget"** gives `a ≥ 1/√3 = 0.577`, rounded up to **0.60**.

So the floor is a published price: **using a judge at kappa 0.60 roughly triples what a
measurement costs you**, relative to a perfect scorer, for the same statistical power. If 3× is
too generous for what you are measuring, raise it — `--min-kappa 0.75` costs about 1.8× — and
you now know what you are buying.

### The assumptions are checked, not assumed

The derivation above needs two things, and the command gates on both separately so a failure
names which one broke.

**Balance.** The identity `kappa = 1 − 2ε` holds exactly only on a perfectly balanced set. The
calibration set's loader enforces a **minority class of at least 40%** and refuses a set that
breaks it, which fixes the well-known kappa paradox at its cause rather than swapping in a
statistic (Gwet's AC1, prevalence-adjusted kappa) that reports a flattering number on a lopsided
set. Here is what balance actually costs, computed rather than asserted:

| minority class p | p_e at ε = 0.20 | kappa | shortfall vs 1 − 2ε = 0.60 | kappa at ε = 0.10 | shortfall vs 0.80 |
|---|---|---|---|---|---|
| 0.50 | 0.500 | 0.600 | 0.0% | 0.800 | 0.0% |
| 0.45 | 0.503 | 0.598 | 0.4% | 0.798 | 0.2% |
| **0.40 — the invariant** | 0.512 | 0.590 | **1.6%** | 0.793 | 0.8% |
| 0.30 | 0.548 | 0.558 | 7.1% | 0.771 | 3.7% |
| 0.20 | 0.608 | 0.490 | 18.4% | 0.719 | 10.1% |

At the invariant's boundary the identity holds to **1.6% near the floor**, which is where the
decision is made; across the whole kappa range the worst case is 3.9%. Below the invariant it
degrades fast — at a 20% minority class the identity is wrong by 18% — which is why balance is
enforced when the set is written instead of corrected for afterwards. Every row of that table is
a test (`TestPrevalenceSensitivityTable`), so this page and the arithmetic cannot drift apart.

**Symmetry.** Non-differential error is *not* enforceable, so it is measured: the command fails
when `|sensitivity − specificity| > 0.20`, **even when kappa is comfortably above the floor**.
A judge biased in one direction is worse than a symmetric one with the same kappa, because it
moves every measured delta the same way and the headline number does not show it.

### Three verdicts, and the third one fails

| Bootstrap CI on kappa vs the floor | Verdict | Exit |
|---|---|---|
| entirely at or above 0.60 | **PASS** | 0 |
| entirely below 0.60 | **FAIL — below the floor** | 1 |
| straddles 0.60 | **INDETERMINATE — the set is too small to decide** | 1 |

INDETERMINATE fails, and the fix printed with it is *add records*. "We cannot tell" is not "it
is fine" — the same discipline the gaps verdict uses for UNKNOWN. Gating on the point estimate
alone would let a 40-record set flip a gate on noise; gating on the lower bound alone would fail
every small set for being small, which is true and useless as a signal about the judge.

Two more verdicts exist and neither is about the judge:

- **The labels do not agree with each other.** Inter-human kappa is the *ceiling* — a judge
  cannot be held to an agreement its own labelers do not reach. When the ceiling is below the
  floor the run is INDETERMINATE and says the cause is the labels, so nobody goes and edits a
  prompt.
- **Not a usable calibration.** Above a 5% judge error rate the surviving records are a subset
  chosen by which calls happened to work. Same threshold and the same words as the baseline
  gate.

A **graded** Goal (`SCORE_DOMAIN_UNIT_INTERVAL`) reports weighted kappa, Spearman's rho and mean
absolute error, and prints `GATE: not applicable`. Kappa is undefined on continuous scores, and
gating one needs an anchored scale the calibration format does not yet carry; inventing one here
would be the invented threshold this command exists to avoid.

### What the calibration set is, and what it is not

The set that ships in the binary is **synthetic**: question-and-answer records over a fictional
API, written from a human-authored template, each carrying two independent labels and an
adjudicated reference verdict. It contains **no customer content, no trace content, and no PII**,
and there is no spelling in the format for a record harvested from a real deployment — the
loader refuses any provenance that is not `authored` or `synthetic`.

Synthetic still calibrates something real, and it is worth being precise about what. The
statistic measures whether a judge reproduces *stated human judgements about stated rubrics*. A
synthetic record is a real instance of that as long as the rubric and the disagreement are real,
and the near-miss records — answers that are correct but not byte-identical — are exactly the
cases a judge exists to handle and a string comparison does not. What synthetic records cannot
tell you is whether your *production* distribution is like this one. That is why the set is a
regression instrument and an on-ramp, not a certificate: **add records from your own domain**,
under the same two-labeler and balance rules, and the number starts describing your judge on
your data.

Two limits that do not go away:

- **A public calibration set is contaminated for training purposes the day it is published.**
  Any model released afterwards may have seen it. It detects a prompt change making things
  worse; it is not evidence that a judge generalizes.
- **Non-differential error is an assumption the symmetry gate only partly protects.** A judge
  whose errors correlate with the *content* of an arm — it dislikes long outputs, and the
  treatment arm makes outputs longer — violates it in a way no per-class recall reveals.


## When the holdout is too small

Below 20 Cases, Kno marks the holdout **underpowered** and says so in the output.

A holdout of six can only produce a very wide interval — wide enough that most real effects are indistinguishable from zero. The run still executes, because refusing would make the tool unusable while you're experimenting. But the caveat travels attached to the number rather than living in a doc you might not read.

If your holdout is underpowered, the honest reading of a holdout gain is "consistent with anything in this wide range," not the point estimate.

## Separable effect, and minimum detectable harm

Two numbers in Kno's output answer near-identical-sounding questions with
different values, and both carry their sidedness wherever they appear.

**`separable_effect`** (`kno eval inspect`) is the smallest effect a
behavior's dev Cases could separate from zero — **two-sided at 95%**. It is
computed from the sample size and the worst-case paired-binary standard
deviation (sqrt(0.5)) alone, so it is a **bound rather than an estimate from
your data**, and that is precisely what makes it printable *before* any
measurement exists. It is the arithmetic behind
[evaluation-design.md](evaluation-design.md#separable-effect-the-arithmetic-behind-10-cases)'s
"~10+ Cases per behavior" heuristic: ten Cases separates 0.51 and nothing
smaller.

**`min_detectable_harm`** (`Plan.MinDetectableHarm`, reported by `kno value`
and in `inspect`'s observed section) is the smallest *regression* a run's
control arm could distinguish from zero — **one-sided at 95%**.

They differ because the questions differ. Harm detection is directional: you
are asking "did this get worse", so the whole error budget goes in one tail.
"Is this behavior distinguishable from noise" is symmetric, so the budget
splits, and the bound is wider. At 20 Cases the one-sided figure is 0.27 and
the two-sided is 0.33 — reusing the one-sided number for the symmetric
question would report an eval set as more powerful than it is, which is the
one direction these numbers must never err in.

Two consequences worth stating plainly:

- **Both over-warn on a continuous Goal.** sqrt(0.5) is the paired-*binary*
  maximum. A Goal with lower variance can separate smaller effects than these
  numbers claim. Conservative in the recoverable direction — neither number
  will ever tell you your eval set is more powerful than it is.
- **`separable_effect` is dev-only.** It counts the Cases the power
  arithmetic actually uses. A behavior's true Case count is roughly the
  holdout fraction higher, and the column header says `dev Cases` for that
  reason.

## Errors are excluded, and why that's the lesser evil

Cases where the agent failed to answer are counted separately and left out of the score.

The alternative — scoring an error as zero — treats infrastructure failure as task failure. That biases the baseline downward, and since every later delta is measured against the baseline, it makes every asset look better than it is.

Exclusion has its own hazard: if the errors aren't random — if hard Cases are the ones timing out — then the surviving sample is easier than the real one and the baseline is biased *up*. Kno cannot detect that either. What it does instead:

- Reports all three counts, so exclusion is visible.
- Marks a run whose error rate exceeds a threshold (5% by default) as **not a usable baseline**, so later stages refuse to treat it as a clean reference.
- Requires that any delta be computed over the intersection of Cases scored in both runs, so a provider outage in one run can't silently change the population being compared.

## Weak labels are expectations nobody wrote

A run over a mined or harvested eval set reports how many of its Cases carry **derived provenance** — expectations that come from a transcript or a trace rather than a human author. The number is printed with the run and recorded on it, so a weak-label eval set cannot pass for a hand-authored one.

The mark means different things per source, and the number says which:

- The `mine` command (and a jsonl file of its output) marks **every** Case derived: the whole set came from transcripts, wholesale.
- The LangSmith adapter does the same — a LangSmith dataset has no per-row signal, so the adapter cannot distinguish harvested rows, and marks the set wholesale.
- The Langfuse adapter marks **per item**: an item carrying `sourceObservationId` or `sourceTraceId` (harvested from a trace) is derived; an item with neither is hand-authored and stays unmarked.
- The Braintrust adapter marks **per event**: an event carrying an `origin` object (Braintrust's record of "copied from another object") is derived; an event with none is hand-authored and stays unmarked. The origin's `object_type` — an experiment, a span, or an eval result — names the derivation note; it is the *presence* of the copy, not its kind, that marks the label weak.

So the same `weak-label N` line means "the whole set is derived" for a mined or LangSmith run, and "exactly N of these Cases are trace-harvested or copied" for a Langfuse or Braintrust run. The expectation of an exact-match goal is an exact string a human wrote, checked against the agent's answer; a derived expectation is weaker evidence — it is what the trace *recorded* or another object *copied*, not what someone *judged* — and the weak-label count is how the run tells you how much of its denominator rests on that weaker ground.

## A purged run has no baseline score

`kno purge` erases stored conversation content. It preserves the numbers — the score of
each Case survives the blob it arrived in — so a purged run still reports its baseline.

Two cases are the exception, and they produce the same state. A run purged by a build
older than the score column lost its numbers with the blob they lived in. A Score that
cannot be read back — a corrupt row, or one written by a build this one does not
understand — is equally gone. Either way the Cases are complete and there is no way to
recompute the scores without re-running the agent.

Kno reports **no baseline score at all** for such a run, says why, and says how many
Cases are affected — one lost Case in 10,000 and 10,000 lost out of 10,000 are the same
sentence otherwise, and only one of them is worth paying to re-run.

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

## What a confidence interval on a delta claims

Every delta Kno reports comes with an interval, and the interval is the product — a Δ of +0.04 with a range of [−0.11, +0.19] is not a finding, and shipping it as one is what the whole discipline exists to prevent.

Three things about how they are computed, because they change how you read them:

**They are paired.** Each Case is compared against itself rather than against the average of other Cases, which removes the difference between an easy Case and a hard one from the uncertainty. For the same money the interval is much tighter than an unpaired comparison would give.

**They are wide when the sample is small, and they say so.** Each interval records how many pairs it came from. An interval of ±0.30 from twenty Cases is not a measurement failure; it is twenty Cases honestly reported. Buy more Cases and it narrows.

**Every one is a PER-COMPARISON claim.** A 95% interval is right 95% of the time *about that one asset*. Value a pool of 200 and roughly ten assets that do nothing will have intervals excluding zero purely by chance — and they cluster among the small, cheap assets, which is exactly where the ranking puts things at the top. The run records how many comparisons it made so a reader can weigh that. This is one of the reasons `validate` exists: it measures the assets you selected against Cases nothing in the loop has touched.

## What Kno cannot tell you

Stated plainly, because a tool that lists its limits is easier to trust than one that doesn't:

- Whether your eval set reflects production.
- Whether your Goal measures what you actually care about.
- Whether an asset that helps today still helps after a model update.
- Whether an asset is *correct* — only whether it moves your Goal. A confidently wrong document that happens to match your rubric will score well.
- Anything about data you didn't put in the pool. The gaps report tells you where your pool was insufficient; it can't tell you what would have worked.
- **Whether you chose the Cases after looking at the answers.** If you read your baseline's failures and then tagged those Cases, or wrote assets aimed at them, the Cases Kno measures were picked using information from the baseline — and a Δ measured that way is biased **upward by roughly how often your agent gets those Cases right when you re-run them** — not by its score on the recorded baseline, which on the Cases you picked is zero by construction. If your agent answers a Case correctly 70% of the time, an asset that does nothing can measure **+0.70** on the subset where it happened to fail. That figure is a ceiling: it assumes you picked exactly the failures, and real tagging is softer.

  Kno guards its own version of this: when it routes to the Cases a baseline failed, it measures a fresh control arm rather than reusing the draw that did the selecting, and it reserves the control Cases at random before routing runs. It cannot guard yours, because a tag that means "billing" and a tag that means "the ones that failed" look identical from here.

  This is not a reason to avoid reading your failures — that loop is the point of the tool. It is the reason `validate` exists: the holdout is untouched by any of it, so a gain that came from selection shows up there as a gain that doesn't replicate. [ADR-0005](adr/0005-value-cannot-see-user-side-conditioning.md).

- **The harm bound is a limit, not a score.** The run's Plan — recorded on the Run at close — reports `min_detectable_harm`: the smallest regression its control sample could have separated from zero, at the shipped confidence level, computed from the worst-case paired variance — it does not shrink because your observed variance happened to be small, and it uses the t distribution at small samples. Against a harm margin of 0.10 the honest threshold sits near 135 control Cases, so most real runs report a bound larger than the margin and carry the underpowered flag. That is information, not noise: a run reporting no regression while able to see only ±0.27 has not cleared an asset that costs 0.10, and the number says so. **A delta is reported only beside its interval** — if no interval could be formed (too few pairs, or ragged attrition), the Valuation reports `UNDERPOWERED` and no delta, never a bare number.

## What a gaps verdict claims

The gaps statistic is Export's per-cluster answer to "is anything we routed
here actually improving these failures?" A failure cluster — the dev Cases
that shared a tag and failed the baseline — is reported one of three ways,
and each is a different claim:

- **IMPROVED**: an Asset routed to at least 5 of the cluster's Cases has a
  delta whose 95% CI excludes zero. The claim is about that Asset's
  measurement, not about the cluster being fixed.
- **GAP**: the cluster was well-covered and no covering measurement was
  significant. This is a "we looked and found nothing" — it costs a cluster
  its slot only when the look was real.
- **UNKNOWN**: nothing routed to enough of the cluster's Cases, or the
  covering measurement was underpowered. Non-significance is not absence:
  a verdict you cannot distinguish from "we did not look" is labeled that
  way, because the output is a spend recommendation and an UNKNOWN is a
  recommendation to spend to find out.

The reported number per cluster is the best covering Asset's delta and
interval — never a cluster-level threshold, and multiple-testing is labeled
when more than one cluster was evaluated. A run with no cluster data (it
predates the snapshot, or nothing failed, or routing was off) reports "no
cluster data for this run" rather than a guessed verdict.

## What a redundancy claim claims

`REJECTION_REASON_REDUNDANT` is Select's strongest exclusion after
REGRESSION and NO_EFFECT, and it is worth being precise about what it does
and does not assert.

**It is an inference from two independent measurements, not a measurement of
marginal contribution.** Nothing in the pipeline ever measures Asset A and
Asset B *together*. The Value stage measures each Asset against the baseline
independently, so the redundancy rule infers substitutability rather than
observing it: two Assets that each fix the same Cases alone might, together,
fix no more than either alone (redundant — the inference is right) or might
interact (they are not, and the inference would be wrong). The claim behind
a REDUNDANT verdict is **"their measured effects are equivalent and
co-located"**, never **"adding the second buys nothing"** — the tool does not
have evidence for the second, stronger sentence, and does not print it. See
[debt.md#155](debt.md).

**Three verdicts, and which way the tool errs.** Two already-selected-scope
Assets are compared over the Cases they were BOTH routed to and measured on
(`C`). When `|C|` is at least 5 Cases (`core.MinClusterCases`, the same
"did we look?" floor the gaps verdict uses — [debt.md#160](debt.md)), the
comparison decides REDUNDANT or DISTINCT; below that floor, or when a delta
vector cannot be reconstructed at all (a purged or never-measured Asset), the
verdict is UNKNOWN. **UNKNOWN selects both Assets.** The costs of the two
possible mistakes are not symmetric: a missed redundancy costs budget — a
capped, visible, recoverable resource — while a false redundancy silently
deletes a real gain from the portfolio with no signal that it happened. The
detector's posture is therefore *select unless proven redundant*.

**Two conditions, both required, and why one alone is dangerous.**
REDUNDANT requires BOTH: **Condition 1**, an equivalence test (not a
difference test) showing the paired per-Case difference between the two
Assets' deltas lies entirely inside a margin `delta`; and **Condition 2**, a
chance-corrected co-improvement Jaccard showing the two Assets improved the
*same* Cases, not merely Cases at the same rate. Condition 1 alone is
actively dangerous: two Assets with identical mean deltas that fix
*disjoint* Cases are perfect complements, and dropping either loses half the
gain. It takes two conditions to make the claim and one to refuse it.

- `delta` is **derived, never invented**: `max(--redundancy-margin,
  MinDetectableEffect(|C|, level))`. The default is 0 — the sample's own
  resolution decides, and a user may only raise the bar, never buy a finer
  claim than the data can support. When the required `delta` would exceed
  `--redundancy-max-margin` (default 0.10 — a tenth of the Goal's range is
  not "the same effect" for any Goal this tool has shipped), the verdict is
  UNKNOWN rather than resting on a margin wide enough to swallow the effect.
- The co-improvement floor is **derived, never invented**: `max(
  --redundancy-min-coimprovement, J_chance)`, where `J_chance` is the
  co-improvement two *unrelated* Assets with the same observed improvement
  rates would produce by coincidence. A raw agreement number is
  uninterpretable until chance agreement is subtracted — the same correction
  `judge calibrate`'s kappa makes, for the same reason. Two Assets that each
  improve 80% of a slice overlap on ~64% of it while sharing nothing but
  prevalence; a fixed constant would call that co-located. When both Assets
  improve (nearly) everything in `C`, `J_chance` approaches 1 and no interval
  can clear it — co-location carries no information when everything is
  co-located — so the verdict is UNKNOWN, correctly.

**Read this before relying on a default run to catch duplicates.** At the
routed-slice sizes a default run typically produces (`MinClusterCases` and
`value.DefaultMinSample` are both 5), a typical shared slice sits at or just
above the equivalence-test floor, where the test has almost no power — at 20
shared Cases, `MinDetectableEffect` is already ~0.33, above the *default*
`--redundancy-max-margin` ceiling of 0.10. That pushes most default-sized
comparisons to UNKNOWN rather than a verdict either way. That is the correct
and safe outcome, but it means the tuning-set duplication cost this feature
exists to catch will often go undetected on a default run unless
`--redundancy-max-margin` is widened deliberately. See
[debt.md#162](debt.md).

**The multiplicity correction is data-dependent.** `Portfolio.n_redundancy_tests`
counts the pairwise redundancy tests Select's greedy decision actually
performed — not the worst-case `k(k-1)/2`, which would correct every
interval to a level so extreme that no pair is ever equivalent. Every
redundancy interval is Bonferroni-corrected against this count. Because the
count is itself a function of which pairs were found redundant — which is a
function of the corrected interval — the guarantee is slightly weaker than a
pre-registered correction. It is conservative in the direction that matters:
it admits FEWER redundancy claims, never more. See [debt.md#157](debt.md).

**A measurement-equivalent pair is resolved by carrying cost, not by which
one a noisy ranking saw first — inside a bias band.** Once Condition 1 has
passed, the two Assets' measured deltas cannot, by construction, distinguish
them — so the old first-seen-wins tie-break was breaking the tie on noise.
The cheaper Asset now survives, **unless** the two carrying costs differ by
less than 2.4x, the content-type spread [debt.md#68](debt.md) already names
in `context_tokens`: inside that band the estimate cannot support a cost
claim, and the tie falls to Asset ID instead — deterministic and unbiased,
rather than a guess dressed as a measurement. `RedundancyEvidence.decided_by`
and `.cost_ratio` make this auditable per rejection. See
[debt.md#161](debt.md).

**Content similarity decides only when measurement evidence does not
exist.** The 3-gram shingle rule this tool shipped in v0.1 (a 0.6 Jaccard
threshold, unargued and unchanged — [debt.md#159](debt.md)) still runs, but
only for two `KIND_KNOWLEDGE` Assets, destination-blind, exactly as before —
byte-compatible with the Portfolio a poolless-content run produced
previously. When a measured comparison IS available, it decides, and the
shingle number is recorded only as corroborating evidence.

**Nothing here measures A and B together, and redundancy needs measurement
overlap that routing does not aim for today.** Two genuinely duplicate
Assets routed to disjoint Case clusters are UNKNOWN forever, no matter how
much is spent measuring them separately. See [debt.md#155](debt.md) and
[debt.md#156](debt.md).

**Check a claim you disagree with.** `kno select --explain <asset-id>` is
free, read-only, and makes no provider call: it prints the per-Case table
behind every REDUNDANT verdict the Asset carries — the baseline score, each
Asset's delta, and whether each counted as an improvement — so a user who
believes two Assets are complements can see the disjoint columns for
themselves.

## What a cost figure claims

Every cost figure Kno reports claims exactly this: **reported usage at rates as published on `<date>`** — the token counts the provider reported, multiplied by the rates in the price table, where `<date>` is the day those rates were read, carried on the table as `pricing.Version`. The figure is an estimate, not an invoice: settlement reconciles against the provider's own reported usage, and the two can differ — discounts and committed-use pricing are things your provider knows and this table does not. If a price is wrong or missing for your model, you do not have to wait for a release: `--price-input-per-mtok` and `--price-output-per-mtok` state the rates for a run.

The Bedrock and Vertex schemes add a **regional multiplier**: 1.10x in Europe, 1.00x in the US, applied to the table rate before the budget guard settles — so the consent figure and the cap both use the regional number. The 1.10x constant is confirmed on every pricingcheck run against AWS's machine-readable Bedrock price list; Google publishes no machine-readable list, so Vertex's multiplier is the same committed constant and the check reports that obligation every run.

The table is hand-entered and dated, so it can go stale — and a stale table is refused loudly, not silently wrong. When the table is older than 90 days the pricing check fails and files a `pricing-drift` issue, because a cost cap's ceiling is only as good as the rates it was computed from. The thing watching the date is the pricing drift detector: a weekly scheduled job, and `make pricing-check` locally, that fetches the providers' published rates and compares them against the table. Each open issue closes itself with a verification comment on the first run whose report
no longer carries its finding; findings still present keep their issue open and updated.

### What a *reported* spend figure covers

`kno baseline` and `kno value` each report what they spent, as one line in the human output and as a `spend` block in `--json`; `kno report` sums the runs it names into the pipeline total. Two things that figure is, and is not.

**It is the run's lifetime spend, not this process's.** A run killed and resumed reports what the whole run has settled, prior sessions included — the budget guard is restored from the durable record before a resumed run authorizes anything, and the run, not the session, is the unit you authorized: the consent quote, `--max-cost-usd`, and the resume dialog all bound the run. A resumed run says so, in both renderings. Reporting per-session would mean a run interrupted four times prints four small numbers, each of them true and each of them an understatement, with the smallest one in your CI log.

**It is what the guard settled, which is neither an invoice nor a bound on what was billed.** It carries every caveat above — reported usage at published rates, no discounts, no committed-use pricing — plus one more: a run killed after a provider charge but before the charge was persisted under-counts by up to `--concurrency` calls' worth ([debt.md#20](debt.md)). And a stage that reports **no** spend figure at all is not reporting zero: `select`, `export` and `report` run no budget guard and emit no spend keys, saying so positively with `"guarded": false` rather than leaving you to infer it from a missing key. The rules are [ADR-0006](adr/0006-the-json-contract.md); the short version is that `.spent_usd // 0` is the wrong repair, because it turns "this stage had no meter" into "this stage was free".

## `delta_per_cost` carries the tokenizer's bias

The ranking metric divides Δgoal by the Asset's `context_tokens` — the carrying cost pool adapters estimate from bytes. The markdown, CSV, and Hugging Face pools all use the same estimate, bytes over the fixed 3.6 divisor, so the bias is uniform across content-type pools rather than a difference between them. That estimate is deliberately pessimistic: it reserves roughly 3x what English prose actually uses, which is the right direction for reserving money and the wrong one for ranking. Two Assets of equal real token cost can differ **~2.4x in `delta_per_cost` by content type alone**, and `select` ranks on this number, so the bias travels into the portfolio. It is acknowledged here and on the field itself rather than argued away; the fix (a real tokenizer) is [ledgered](debt.md#68) as debt rather than shipped silently.

## If you only remember one thing

**The holdout number is the one you may put in a slide.** Everything else is a measurement in service of producing it.

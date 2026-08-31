# `kno eval inspect`: is this eval set decomposed enough to attribute anything?

`docs/evaluation-design.md` closes with a section titled **"The inspect idea"** naming this
command by name — "flagging underpowered behaviors, multi-behavior Cases, and coarse Goals
from the data a run already records" — and saying the page exists so that command's output
has a vocabulary to point at. This plan turns that paragraph into a command, and is written
against the actual `Case` message, the actual `Goal` implementations, and the actual
statistics in `stats/interval` and `core/value`.

**One headline finding up front, because it changes the product spec.** The target output the
product owner supplied contains the line `"overall_quality" accounts for 62% of total score`.
**That number is not computable in this build, and this plan does not ship it.** `Score` has a
`components map<string, double>` field (`proto/kno/v1/case.proto`, field 4), the full `Score`
proto is persisted as `score_proto` (`store/sqlite.go`, `RecordOutcome`) — and **nothing in
the tree ever writes a component**. `grep -rn "Components"` outside `gen/` returns nothing.
The only `Goal` that ships is `goal/exactmatch`, which sets `Value`, `Passed` and `Rationale`
and no components (`goal/exactmatch/exactmatch.go`), `resolveGoal` accepts exactly one name
(`cli/render.go:29`), and `judge/` contains one file: `doc.go`. A run has one Goal and that
Goal emits one number. There is no score to take a percentage of. Section
"The line that isn't computable" below says what replaces it and what would have to land
before the original line could be honest.

**Phase-1 re-reviewed 2026-08-30 — verdict: amend; amendments applied.** Four findings, all
folded in and tagged. The command now states **once, prominently in the output**, that every
per-tag number and suggestion is conditional on the user's tags naming behaviors — because a
taxonomy of `p0` and `regression-2024` is indistinguishable from a real behavior taxonomy to
this tool, and the first draft worried only about under-crediting good taxonomies, never about
confidently advising on tags that are not behaviors at all *(F1)*. `separable_effect` is
reported **two-sided**, because "is this behavior distinguishable from noise" is a symmetric
question and a one-sided bound at the same level is tighter — reusing it would make eval sets
look more powerful than they are, the opposite of the conservative discipline this plan applies
elsewhere; `Plan.MinDetectableHarm` stays one-sided where it belongs *(F2)*. The invented 25%
multi-behavior threshold is deleted and `behavior_separation` becomes reported-never-flagged,
leaving **five flaggable checks, not six** *(F3)*. The concentration semantic — whose formal
definition and example wording contradicted each other — is pinned before any golden is
written *(F4)*. Approved as designed and not reworked: the
`stats/interval.MinDetectableEffect` move and its characterization test, dropping the
uncomputable "62% of total score" line, the Case-share concentration/untagged replacement,
the `core.Seal` holdout handling, and exit-0-always.

## Problem

Kno's central promise is attribution: *this Asset moved this outcome by this much*. Every
mechanism that delivers it is bounded by the granularity of the eval set, and the tool
currently discovers that bound the expensive way — after the user has paid for a baseline and
a value run.

Concretely, from the code:

- **Routing degrades silently to "measure everything against everything".** `cluster()`
  (`core/value/route.go:611`) returns `ModeAllFailed` when no dev Case carries a tag, and
  `route.go:63` says so in as many words: *"This is the DEFAULT STATE OF A REAL EVAL FILE —
  `Case.tags` is optional and nothing populates it."* A user whose Cases are untagged gets
  a more expensive run with no per-behavior attribution, and finds out in the run's mode
  line, mid-spend.
- **Per-behavior power is invisible until a Valuation comes back `UNDERPOWERED`.**
  `core.MinClusterCases` is 5 (`core/gaps.go:15`) — the exported line between "we looked and
  found nothing" (GAP) and "we did not really look" (UNKNOWN). Nothing tells the user, before
  a run, which of their behaviors sits below it.
- **The advice exists and nothing operationalises it.** `docs/evaluation-design.md` §2 offers
  "~10+ Cases per behavior" as an explicitly-labeled heuristic and §8 names "one giant score"
  and "few Cases, many assets" as anti-patterns. It is prose. The user has to audit their own
  JSONL by hand against it.

`kno eval inspect` reads an Evals source, reports what the routing and power machinery will
actually see, and does it before anything is spent.

## Design

### Surface

```
kno eval inspect --evals <source> [--value-run-id <id>] [--db <path>] [--json]
```

`eval` is a parent command with one child. **This is the first two-level command in the
tree** — `NewRootCmd` (`cli/root.go:126-135`) registers nine flat commands today. Chosen
anyway, for one reason: `docs/evaluation-design.md` already published the name
`kno eval inspect`, and a shipped doc naming a command is a promise in the same way
`ErrCapabilityUnsupported`'s fix line was a promise that `kno doctor` had to keep. The
namespace also has obvious future members (`kno eval split`, `kno eval diff`), so `kno
inspect` at the root would be the shape that needs renaming later.

**Read-only and free**, the `kno doctor` posture (`cli/doctor.go:23`): it constructs no
`Agent`, resolves no model credential, makes no LLM call, creates no Run, and writes nothing.
No budget guard, no consent dialog — there is no spend path to gate. One caveat stated in the
help text: a remote Evals source (`langsmith:`, `langfuse:`, `braintrust:`, `hf:`) does make
vendor API calls with the vendor's credentials, because reading the dataset is the job.
"Costs nothing" is a claim about LLM spend, and the help text says exactly that.

### What data this actually has

Verified field by field against `proto/kno/v1/case.proto` and the adapters. **Without a run**,
from `core.Evals`:

| Available | Where |
|---|---|
| `Case.id` | `case.proto` field 1 |
| `Case.input`, `Case.expected`, `Case.rubric` | fields 2, 3, 4 |
| `Case.split` (DEV / HOLDOUT / UNSPECIFIED) | field 5 |
| `Case.tags []string` | field 6 |
| `Case.provenance` (`Derived`, note, source ref) | field 7 |
| `Case.history []Turn` | field 8 |
| dev / holdout / weak-label counts | `evalSource.CountSplits` → `split.Counts{Dev, Holdout, WeakLabelCases, HoldoutFrac}` |
| content fingerprint | `evalSource.ContentHash` |

**Not available, and not invented:** there is no per-Case Goal, no per-Case behavior field,
no declared behavior list anywhere in the schema, and no `Goal` registry beyond
`exact-match`. `Goal` is an interface with `Score`/`Domain`/`Direction` (`core/ring0.go`);
nothing enumerates Goals and nothing associates a Case with one.

**With `--value-run-id`**, additionally, all through existing readers:

| Available | Where |
|---|---|
| the routing plan (`Mode`, `Clusters[]{Tag, CaseIDs, NDropped}`, `Routed[]{AssetID, CaseIDs, FreshControlArm, NotMeasuredReason}`, `EligibleCases`, `ControlCaseIDs`, `MinDetectableHarm`, `ControlUnderpowered`) | `Run.value_plan` (field 29), gob-decoded into `core/value.Plan` — the same decode `core/export.go:243-248` already performs |
| per-Asset Valuations with intervals | `store.Valuations(ctx, runID)` |
| per-cluster improved / gap / unknown verdicts | `core.ComputeGaps(plan, valuations)` |
| the baseline this run paired against | `Run.baseline_run_id` (field 30); `cli/report.go:239` already chains this way |
| per-Case pass/fail from that baseline | `store.CaseScores(ctx, baselineRunID)` → `map[string]CaseScore{Value, Passed, Unrecoverable}` |
| whether the Evals source still matches the run | `Run.eval_content_hash` (field 21) vs. `evalSource.ContentHash` |

No new store method, no new proto field, no new event.

### "Distinct behaviors" — the definition, and why it is tags

**A behavior is a normalized tag.** Not a Goal, not both.

Goals are out because there is exactly one per run and one implementation in the build; a
"behaviors from Goals" definition would report `1` for every eval set in existence and would
be dishonest the moment a second Goal shipped without an association from Case to Goal
(which the schema does not have).

**And the tool cannot tell whether your tags are behaviors at all** *(F1)* — the finding that
matters most about this definition, and one the first draft only half-confronted. It worried
about *under*-crediting: a user with a disciplined taxonomy in a field the schema does not
have gets no credit for it. The worse and likelier failure is the inverse. Tags routinely
encode priority, provenance or date — `p0`, `flaky`, `regression-2024`, `imported-from-zendesk`
— and `inspect` will report those as N "distinct behaviors", attach a specific separable-effect
number to each, and then advise the user to *"split `p0` into the behaviors you would act on
separately"* or *"add Cases to `regression-2024`, or merge it into a behavior you would fix
together"*. That is confident, domain-specific, directive advice about a tagging convention
the tool cannot distinguish from a real behavior taxonomy, emitted by a command whose entire
premise is refusing to say more than the data supports.

The fix is not to guess (there is nothing to guess from) and not to bury the caveat in
Accepted Risks. It is to make **every** per-tag number and suggestion explicitly conditional,
stated once and prominently in the output rather than repeated into invisibility — see the
standing conditional in "Output" below, which is pinned by a golden in both renderings and is
the first thing printed above the behavior table.

Tags are in because tags are what the machinery *already* means by a behavior.
`cluster()` groups failed dev Cases by tag; `candidatesFor` routes an Asset to the clusters
its own tags overlap; `snapshotClusters` freezes them; `core.ComputeGaps` reports one verdict
per tag. `Case.tags`' own godoc reads: *"Labels for slicing results: 'billing', 'refund',
'tool-use'. Failure clusters are reported in these terms."* Defining a behavior as anything
else would make `inspect` report on a structure the engine does not use.

**Normalization is routing's, exactly.** `core/value.normalizeTag` is
`strings.ToLower(strings.TrimSpace(t))` (`route.go:741`) and empty keys are skipped
(`route.go:635`). `inspect` must use the *same function*, not a copy — a second normalizer
that drifts would report a behavior count the engine disagrees with, which is worse than no
report. `normalizeTag` is unexported; see "Code the plan needs and does not have" below.

`inspect` additionally **reports the collapse**: if `Refunds`, `refunds` and ` refunds `
appear, it says `3 spellings collapsed into "refunds"`. A user who believes they have eight
behaviors while routing sees six needs that line more than any other in the output.

### The multi-behavior heuristic, labeled as one

**Rule: a Case carrying two or more distinct normalized tags is counted as multi-behavior.**

That is a heuristic and the output says so in the same sentence as the number. It is a
heuristic because a tag is a label, not a claim about what a Case exercises: `["billing",
"tool-use"]` might mean "this Case tests two behaviors and a failure cannot be attributed to
either" or it might mean "this is a billing Case, and it happens to involve a tool" — and
nothing in the schema distinguishes those. The tool cannot know; it can only count.

What makes the count worth printing anyway is that the count is *exactly what routing does*.
`cluster()` (`route.go:631-641`) appends a multi-tagged failed Case to **every** one of its
tag's clusters. A Case with three tags is a member of three clusters and testifies about all
three. So the number is not a guess about the user's intent — it is a measurement of how much
of the cluster structure is shared, which is precisely the thing that makes per-behavior
attribution ambiguous.

Wording, pinned in the golden: `31% of dev Cases carry more than one behavior tag — a failure
in those Cases testifies about every tag it carries, so per-behavior attribution is shared.
(Heuristic: a tag is a label, not a claim about what a Case exercises.)` — `dev Cases`, over
the denominator pinned in the concentration section *(F4)*.

**This share is reported and never flagged** *(F3)* — see "The five checks" below.

### The underpowered threshold, and where the math comes from

The number reported per behavior is **the smallest effect a Case set that size can separate
from zero**, not a bare "too few".

The math exists: `core/value.minDetectableHarm(m)` (`route.go:98`) is
`t95OneSided[m-2] * sqrt(0.5) / sqrt(m)` — a one-sided 95% bound using the worst-case
paired-binary standard deviation `sqrt(0.5)`, with a small-`m` Student-t table
(`route.go:121`) rather than `z`. It is a bound, not an estimate from the data, which is what
makes it printable before a run exists. `Plan.MinDetectableHarm` already carries it for the
control arm, `MinControlSample` is 20 and `HarmMargin` is 0.10 (`route.go:243-263`).

**Sidedness: `inspect` reports the two-sided figure** *(F2)*. This is the amended decision and
it changes every number below. `minDetectableHarm` is one-sided because harm detection is
directional — you are asking "did this get worse", not "did this move". `inspect`'s stated
job is the symmetric question: *is this behavior distinguishable from noise*, which is the
same symmetric language `docs/evaluation-design.md` §2 uses. A one-sided bound at a given
level is **tighter** than the two-sided bound at that level, so reusing `MinDetectableHarm`'s
figure would report eval sets as more powerful than they are for the question `inspect`
actually answers — the exact opposite of the erring-conservative discipline this plan applies
when it chooses the worst-case `sqrt(0.5)` standard deviation. The first draft chose the
one-sided figure to keep one name meaning one number; the amendment keeps that property the
right way, by **labeling both**: `inspect` prints `two-sided 95%` in the column header, and
`Plan.MinDetectableHarm` — which appears only in the `observed` section, only about the
control arm — is printed as `one-sided 95%` beside it. Two figures with two labels is
vocabulary; two figures with one label was the drift to avoid.

Reproducing the formula over the shipped table (`df = m − 1`, which is what `t95OneSided[m-2]`
indexes) gives, in both sidednesses:

| dev Cases in the behavior | separable effect (two-sided 95%) — what `inspect` prints | one-sided 95% — what `Plan.MinDetectableHarm` reports |
|---|---|---|
| 3 | 1.76 (i.e. nothing) | 1.19 |
| 5 (`core.MinClusterCases`) | 0.88 | 0.67 |
| 10 (the evaluation-design "~10+" heuristic) | 0.51 | 0.41 |
| 20 (`value.MinControlSample`) | 0.33 | 0.27 |
| 44 | 0.21 | 0.18 |
| ~135 | 0.12 | 0.10 (`value.HarmMargin`) |
| ~195 | 0.10 | 0.08 |

That table is the honest version of §2's "~10+ Cases per behavior": ten Cases buys you the
ability to detect a **51-percentage-point** swing on a binary Goal. It also prices the
sidedness choice — reaching `HarmMargin`'s 0.10 two-sided needs roughly 195 Cases where the
one-sided figure gets there at 135 — which is a fact about the question being asked, not a
tax this plan is choosing to levy. `inspect` prints the number per behavior rather than the
adjective, so the user is arguing with arithmetic instead of with an opinion.

**Thresholds**, each anchored to an existing exported constant rather than to a number this
plan invents:

- Below `core.MinClusterCases` (5) dev Cases → `UNDERPOWERED`. This is the constant
  `core.ComputeGaps` already uses to decide whether a measurement may testify about a
  cluster at all, so a behavior below it cannot produce a cluster verdict by construction.
- At or above 5 → `OK`, **with its detectable effect always printed**. There is no second
  adjectival tier; the number is the tier. A behavior with 12 Cases is not "adequate", it is
  "can separate 0.45", and only the user knows whether 0.45 is the effect they would act on
  *(F2: two-sided; the one-sided figure would have read 0.37)*.

Sidedness is settled above and needs no reviewer call: `inspect` reports two-sided, the
`observed` section reports `Plan.MinDetectableHarm` one-sided, both are labeled at every
appearance in both renderings, and `core/value` is **not** changed to report both — the
control arm's question stays one-sided *(F2)*.

### Code the plan needs and does not have — scoped, not assumed

Two functions this command needs are unexported. Both are named here rather than discovered
by the implementer.

1. **`core/value.minDetectableHarm`** is unexported and lives in a package whose whole point
   is that it never sees a score. **Proposal: move the arithmetic to
   `stats/interval.MinDetectableEffect(n int, side knov1.Sidedness, level float64) float64`
   and have `core/value.minDetectableHarm` delegate.** `stats/interval` is where this math
   belongs — it already exports `Quantile(level, side, df)` for precisely this
   cross-package need (`interval.go:345`, exported "for the multiplicity correction in
   `stats/portfolio`") and it already owns the `studentTQuantile` implementation. `cli/`
   may import `stats/interval` (it already imports `stats/budget` in `cli/render.go`), and
   `core/value` importing `stats/interval` respects the layering.

   **The extraction must serve both sidednesses, which F2 makes load-bearing rather than
   incidental** *(F2)*: `MinDetectableEffect` delegates to the existing
   `Quantile(level, side, df)` with `df = n − 1` and multiplies by `sqrt(0.5)/sqrt(n)`, so
   `SIDEDNESS_UPPER` reproduces `core/value`'s numbers exactly while `SIDEDNESS_TWO_SIDED`
   produces the figure `inspect` prints — one implementation, two call sites, no second
   table, and no way for the two commands' arithmetic to drift apart. The `t95OneSided` table
   in `route.go` becomes redundant and is deleted in the same PR, with a **characterization
   test** asserting the new function *under `SIDEDNESS_UPPER`* reproduces the old table's
   values to the last digit at every `m` from 1 to 40 — a refactor that changes a reported
   bound is a P0, and the test is what makes that impossible.
2. **`core/value.normalizeTag`** is unexported. **Proposal: export as
   `value.NormalizeTag(string) string`**, one line, with godoc saying it is the routing
   normalizer and that any consumer reporting tag counts must use it. The alternative —
   `inspect` reimplementing `ToLower(TrimSpace(...))` — is two lines of duplication that
   silently diverge the day routing's rule changes, and the divergence would be invisible.

Both are additive to a pre-1.0 Go surface, both are covered by existing package tests plus
the characterization test above.

### The line that isn't computable, and what replaces it

`"overall_quality" accounts for 62% of total score` is dropped. The evidence is in the
preamble: no `Goal` in the build emits `Score.components`, there is one Goal per run, and no
store reader surfaces components even though `score_proto` would carry them. Printing a
score decomposition derived from nothing would be the exact failure this repo has been burned
by twice, in the output of the command whose entire purpose is to criticize numbers that
aren't earned.

**Replacement, computable and honest:** two lines about *Case* concentration, never about
score.

**The semantic, pinned unambiguously before any golden is written** *(F4)*. The first draft's
formal definition ("share of Cases carrying the single most common tag") and its example
wording ("is the **only** behavior on 62% of Cases", which implies exclusivity) described two
different quantities, and a byte-identical golden cannot be written against an ambiguous
spec. The definition wins and the wording is corrected to match it:

> **concentration = (dev Cases carrying the most common normalized tag) ÷ (all dev Cases).**
> Membership is **non-exclusive**: a Case tagged `["overall_quality", "billing"]` counts
> toward `overall_quality`'s numerator. The denominator is **all dev Cases, untagged
> included** — the same `dev Cases` total printed above the behavior table.

Non-exclusive is the right choice because it is how every other count in this command works
and how routing itself works: `cluster()` puts a multi-tagged failed Case into *every* one of
its clusters, so an exclusivity-based concentration would describe a structure the engine does
not use. Including untagged Cases in the denominator is the right choice because it makes
`concentration` and `untagged` shares of the same population, so the two lines can be read
against each other.

- `concentration`: `"overall_quality" is carried by 62% of dev Cases` — the corrected
  wording; the word "only" is gone. This is the "one giant score" anti-pattern
  (`evaluation-design.md` §8) as it actually manifests in an eval file: a catch-all tag under
  which nothing can be attributed.
- `untagged`: the share of dev Cases carrying **no** tag, over that same denominator. This is
  the more consequential of the two and has no analogue in the product owner's draft: an
  untagged Case cannot join any cluster, and if *no* dev Case is tagged, `cluster()` returns
  `ModeAllFailed` and per-behavior attribution does not happen at all for the entire run.

The multi-behavior share uses the same denominator for the same reason (`21 / 67` in the
example below), and every one of the three lines says `dev Cases` rather than `Cases`.

**What would have to land for the original line to become honest**, recorded so the deferral
is a deferral and not an omission: a composite `Goal` that populates `Score.components`, plus
a store reader that surfaces them per run. Both belong to the judge milestone
(`judge calibrate` is v0.2 in DESIGN.md:398). The trigger is written into the ledger entry
this plan adds: *when the first Goal that populates `Score.components` merges*.

### The grade: replaced, and why

The product owner's draft ends `Attribution quality: MODERATE`. **This plan does not ship an
adjectival grade**, and the reason is that the command would be self-refuting: the finding
directly above it is "`overall_quality` accounts for most of your signal, which is
unactionable", and `MODERATE` is a single coarse score blending five different questions with
five different fixes into one word. `evaluation-design.md` §8 lists that exact shape as
anti-pattern number one. A tool cannot credibly criticize a number it is simultaneously
emitting.

**What ships instead** — three-state per-check statuses, which is the discipline the repo
already uses. `knov1.GapStatus` is `IMPROVED / GAP / UNKNOWN` precisely because
"non-significance is not absence" (`core/gaps.go:22`); `store.CaseScore` carries
`Unrecoverable` because "has no score" and "scored and the number is gone" are different
states. Same rule here:

- Each check reports `OK`, `FLAGGED`, or `UNKNOWN`. `UNKNOWN` is a real answer — a check that
  needs a run reports `UNKNOWN` without `--value-run-id` rather than passing by default.
- The headline is a **count, not a word**: `2 of 5 checks flagged`. A count is ordinal,
  reproducible, and cannot be mistaken for a measurement of anything.
- Exit code is **0 regardless of findings**. `inspect` is a diagnostic, not a gate; a CI
  consumer that wants a gate reads `checks_flagged` from `--json`. Making findings non-zero
  would make `kno eval inspect` unrunnable in the pre-commit position where it is most
  useful, and would collide with `ExitError`'s meaning ("something is broken"). A future
  `--strict` flag that opts into a non-zero exit is a possible addition and not a blocker for
  this plan; it is named here so a later PR proposing one is extending a decision rather than
  reversing one.

### The five checks *(F3)*

Each names one observable, one threshold, and one constant it is anchored to. **There are
five, not six**: `behavior_separation` was the sixth, and it is no longer a check.

| Check | Question | Threshold / anchor | Needs a run? |
|---|---|---|---|
| `behaviors_declared` | Do any dev Cases carry tags at all? | zero tagged dev Cases → FLAGGED, because `cluster()` returns `ModeAllFailed` and there is no per-behavior attribution | no |
| `behaviors_powered` | Per behavior: enough dev Cases to separate an effect? | `core.MinClusterCases` (5); detectable effect always printed | no |
| `behavior_concentration` | Share of dev Cases under the single most common tag; share untagged | reported; FLAGGED when one tag covers >50% of dev Cases, or when untagged dev Cases exceed 50% of dev Cases — denominator pinned in the concentration section *(F4)* | no |
| `holdout_powered` | Is the holdout large enough for `validate`? | `split.MinHoldout` (20), via `Counts.Underpowered()` — the same check `cli/render.go:188` prints *after* a baseline, surfaced *before* | no |
| `attribution_observed` | What did routing actually do, and which behaviors got a verdict? | `Plan.Mode`, `core.ComputeGaps` verdicts, `Plan.ControlUnderpowered`, `Plan.MinDetectableHarm` | **yes** — `UNKNOWN` without `--value-run-id` |

**`behavior_separation` is deleted as a check and survives as a reported number** *(F3)*. The
first draft flagged it above 25% and admitted, in a `(verify)` tag and again in Accepted
Risks, that 25% was anchored to nothing in the tree. A tool whose thesis is *"do not invent
thresholds"* cannot flag on an invented one; that is self-contradiction, not a documented
heuristic, and the plan's own offered fallback is the right answer. The multi-behavior share
is still computed, still printed as a finding line, and still in `--json` as
`multi_behavior_share` — it simply never produces a status. It is **not** given a fourth
status to preserve its place in the array: three states are the discipline borrowed from
`GapStatus`, and inventing a fourth to rescue a deleted threshold would cost more than the
check was worth. Findings-with-status and reported-numbers are rendered with different markers
(`!` / `✓` for checks, `·` for reported-never-flagged) and the legend says so.

### Does it need a run? No. Does it use one? Yes.

**Evals alone is the primary mode** and must be, because the command's job is to be run
*before* spending. Four of the five checks need only the eval source.

`--value-run-id` is **optional** and adds the fifth check plus per-behavior observed failure
rates (via `Run.baseline_run_id` → `store.CaseScores`, giving `refunds: 4 of 6 dev Cases
failed at baseline` — the thing that tells a user *which* behavior is worth buying data for).
It never changes the other four checks' verdicts: a static property of the eval file must not
depend on whether a run happens to exist.

`--db` defaults to `kno.db` like every other command and is only read when `--value-run-id`
is given.

**Fingerprint check:** when `--value-run-id` is given, `inspect` compares
`evalSource.ContentHash(ctx)` against `Run.eval_content_hash` (field 21) and, on a mismatch,
reports the observed section as `UNKNOWN` with "the eval source has changed since this run"
rather than joining a current tag structure to a stale plan. Never silently.

### Holdout

**Per-Case analysis reads through `core.Seal`.** Tag counting, multi-behavior counting and
concentration are computed over `SPLIT_DEV` Cases only, obtained via `core.Seal(src).Cases()`
(`core/seal.go:38`) — the type that makes forgetting a compile error.

Totals come from `CountSplits`, which iterates every Case (holdout included) but yields
nothing and retains nothing but counters. That distinction is the existing precedent:
`CountSplits` already runs at ingestion before any spend, and counting is not reading.

The reported numbers are therefore **dev-side**, and the output labels them that way —
which is also the statistically correct choice, since routing, clustering and every
`MinClusterCases` decision operate on the dev split. Holdout appears exactly once, as a total,
in the `holdout_powered` check.

Consequence, stated: a behavior's true Case count is roughly 25% higher than the dev count at
the default holdout fraction. `inspect` reports the dev count because that is the number the
power arithmetic uses, and the column header says `dev Cases`.

### Output

```
kno eval inspect --evals cases.jsonl

Evals  cases.jsonl
  84 Cases — 67 dev, 17 held back
  6 distinct behaviors (tags), 3 spellings collapsed into "refunds"

  Everything below reads your tags as behaviors, because that is what routing does. If these
  tags name something else — priority, source, a date — the per-tag numbers and suggestions
  below do not apply to them. Kno cannot tell the difference.

BEHAVIOR         DEV CASES  SEPARABLE EFFECT (two-sided 95%)  STATUS
billing                 22                             0.31   ok
refunds                 18                             0.35   ok
shipping                12                             0.45   ok
account                  9                             0.54   ok
tool_use                 3                             1.76   underpowered
refund_policy            3                             1.76   underpowered

  · 31% of dev Cases carry more than one behavior tag — a failure in those Cases testifies
    about every tag it carries, so per-behavior attribution is shared.
    (Heuristic: a tag is a label, not a claim about what a Case exercises. Reported, not
    flagged: there is no principled threshold for this.)
  ! "overall_quality" is carried by 62% of dev Cases — a catch-all tag under which nothing
    can be attributed. If it is a behavior you would fix in one place, that is fine; if it
    is several, split it.
  ! 2 behaviors have fewer than 5 dev Cases, the minimum a measurement needs before it may
    testify about a behavior at all (core.MinClusterCases).
  ✓ the holdout has 17 Cases (20 is the minimum for a meaningful interval at validate)

2 of 5 checks flagged.  If these tags are behaviors you would fix separately:
  - split "overall_quality" into the behaviors you would act on separately
  - add Cases to tool_use and refund_policy, or merge them into a behavior you would fix
    together
  - re-run with --value-run-id <id> to see which behaviors a run actually attributed

What each number claims: docs/what-the-numbers-mean.md   Designing evals:
docs/evaluation-design.md
```

Three things in that block are amendments, not cosmetics. The paragraph above the table is the
**standing conditional** *(F1)*: stated once, before any number, in the place a reader cannot
skip, rather than repeated per line until it reads as boilerplate — and the suggestions block's
header carries the same conditional in four words, because that is where the directive advice
actually lands. The column header reads `two-sided 95%` *(F2)*. The multi-behavior line is
marked `·` — reported, never flagged — and the headline counts five checks *(F3)*; the
concentration line's denominator and its non-exclusive reading are the ones pinned in the
concentration section, and the word "only" is gone *(F4)*.

Deterministic: behaviors sorted by dev Case count descending, tag ascending on ties. Findings
in fixed check order, reported-only lines in fixed position among them. Golden-pinnable end to
end.

### `--json`, per ADR-0001

Hand-written struct in `cli/jsonreport.go` — the one file with the `encoding/json` exemption,
scoped by filename — never protojson over a kno.v1 type, for the reason that file's header
gives: a CLI contract aimed at a jq pipeline must not mirror proto field names or shift when
the schema gains a field.

```json
{
  "evals": "cases.jsonl",
  "cases": { "total": 84, "dev": 67, "holdout": 17, "weak_label": 0 },
  "behaviors": [
    { "tag": "billing", "dev_cases": 22, "separable_effect": 0.3135,
      "sidedness": "two-sided", "level": 0.95, "status": "ok", "spellings": 1 },
    { "tag": "tool_use", "dev_cases": 3, "separable_effect": 1.7566,
      "sidedness": "two-sided", "level": 0.95, "status": "underpowered", "spellings": 1 }
  ],
  "collapsed_spellings": 3,
  "untagged_dev_cases": 0,
  "multi_behavior_dev_cases": 21,
  "multi_behavior_share": 0.3134,
  "dominant_behavior": { "tag": "overall_quality", "dev_cases": 42, "share": 0.6269 },
  "checks": [
    { "name": "behaviors_declared", "status": "ok" },
    { "name": "behaviors_powered", "status": "flagged",
      "detail": "2 behaviors below core.MinClusterCases (5)" },
    { "name": "behavior_concentration", "status": "flagged",
      "detail": "\"overall_quality\" is carried by 62% of dev Cases" },
    { "name": "holdout_powered", "status": "ok" },
    { "name": "attribution_observed", "status": "unknown",
      "detail": "no --value-run-id given" }
  ],
  "checks_flagged": 2,
  "checks_total": 5,
  "suggestions": ["..."],
  "notes": [
    "every per-tag number and suggestion assumes your tags name behaviors you would fix separately; kno cannot distinguish a behavior tag from a priority, source or date tag",
    "separable_effect is a two-sided 95% bound using the worst-case paired-binary standard deviation; it is a bound, not an estimate from your data",
    "multi_behavior_share is reported and never flagged: there is no principled threshold for it"
  ]
}
```

With `--value-run-id`, one additional object:

```json
"observed": {
  "value_run_id": "…", "baseline_run_id": "…", "routing_mode": "tag-overlap",
  "eval_source_matches_run": true,
  "control_cases": 22, "control_underpowered": true,
  "min_detectable_harm": 0.2734, "min_detectable_harm_sidedness": "one-sided",
  "behaviors": [
    { "tag": "refunds", "cluster_cases": 6, "failed_at_baseline": 4,
      "gap_status": "GAP_STATUS_GAP", "best_asset_id": "refund-policy-v3",
      "best_delta": 0.0, "covered_count": 6 }
  ]
}
```

`min_detectable_harm` is `Plan.MinDetectableHarm` verbatim and therefore **one-sided**, while
`behaviors[].separable_effect` is **two-sided**; both carry an explicit sidedness key so a jq
consumer cannot mistake one for the other *(F2)*.

Keys are stable, `omitempty` only where absence is meaningful, floats unrounded. Human and
JSON renderings are pinned to identical content by an equivalence golden — two renderers, one
pinned content, the convention `docs/plans/2026-08-29-report-tui.md` established.

### Streaming and memory

`inspect` consumes `iter.Seq2` end to end and retains **only** per-tag counters, a
tag-spelling set, and four scalars. Memory is O(distinct tags), not O(Cases): a 1M-Case eval
set must not load into RAM, and `iter.Seq` being load-bearing is a CLAUDE.md performance
rule. Case IDs are **not** retained in the Evals-only path. With `--value-run-id`, the plan's
cluster ID lists are retained, bounded by the dev split, which the Value stage already held
in memory when it produced them.

## Acceptance criteria

1. `kno eval inspect --evals <file>` with no run exits 0 and prints the standing conditional
   paragraph, the behavior table, the findings, and the `N of 5 checks flagged` line *(F3)*.
2. Behaviors are normalized with `value.NormalizeTag`, and a file containing `Refunds`,
   `refunds` and ` refunds ` reports **one** behavior with `spellings: 3` and the
   "spellings collapsed" line.
3. Empty-string tags are excluded from the behavior list and do not create a behavior,
   matching `cluster()`'s `key == ""` skip.
4. A Case carrying the same tag twice counts once toward that behavior's dev Case count, and
   the duplicate is reported — matching `snapshotClusters`' `NDropped` accounting.
5. `separable_effect` for a behavior of `m` dev Cases equals
   `interval.MinDetectableEffect(m, SIDEDNESS_TWO_SIDED, 0.95)` and is labeled `two-sided
   95%` in both renderings *(F2)*; separately, a characterization test asserts
   `MinDetectableEffect(m, SIDEDNESS_UPPER, 0.95)` reproduces `core/value`'s pre-refactor
   `minDetectableHarm` for every `m` in 1..40 to full float64 equality. A test also asserts
   the two-sided figure is strictly larger than the one-sided figure at every `m ≥ 2` — the
   property that makes reusing the one-sided number a power overstatement.
6. A behavior with fewer than `core.MinClusterCases` dev Cases reports status
   `underpowered`; one at exactly `core.MinClusterCases` reports `ok`. Both thresholds read
   the exported constant — a test changing the constant changes the verdict.
7. An eval file where **no** dev Case carries a tag reports `behaviors_declared: flagged`
   with detail naming `ModeAllFailed`, and reports zero behaviors rather than erroring.
8. `holdout_powered` is `flagged` for a holdout below `split.MinHoldout` (20) and `ok` at or
   above it, computed from `CountSplits`, agreeing with `Counts.Underpowered()`.
9. Without `--value-run-id`, `attribution_observed` is `unknown` — never `ok` — and
   `checks_total` is **5** *(F3)*.
10. With `--value-run-id`, the `observed` object reports `Plan.Mode`'s string form,
    `Plan.ControlUnderpowered`, `Plan.MinDetectableHarm`, and one entry per plan cluster with
    its `core.ComputeGaps` verdict.
11. With `--value-run-id` whose `Run.eval_content_hash` differs from the source's
    `ContentHash`, `observed` is omitted and `attribution_observed` is `unknown` with detail
    "the eval source has changed since this run". Exit 0.
12. With `--value-run-id` naming a run with an empty or undecodable `Run.value_plan`,
    `attribution_observed` is `unknown` with detail naming the absence. No guess, no panic.
13. `--value-run-id` naming a run that does not exist, or `--db` pointing at an unreadable
    path, is refused with `errs.ErrInvalidInput` (exit 1) and a fix line.
14. No output at any verbosity contains `Case.input`, `Case.expected`, `Case.rubric`, or any
    `Turn.content` — tags, counts and IDs only. A test drives a real inspection over Cases
    whose input is a sentinel string and asserts the sentinel appears nowhere in stdout or in
    `--json`.
15. No holdout Case ID appears in any output; the per-Case analysis is obtained through
    `core.Seal` and a canary test plants a holdout Case with a distinctive tag that must not
    appear in the behavior list.
16. `kno eval inspect` makes no LLM call and constructs no Agent: a test fails on any
    `agentwiring` construction and on any dial for a `jsonl` source.
17. Exit code is 0 whether zero or five checks are flagged; a test asserts an all-flagged
    inspection still exits 0.
18. `--json` emits exactly one document, no prose, and its `checks` array content matches the
    human findings (equivalence golden). Behavior ordering is identical in both.
19. Output is deterministic across runs and across map-iteration order: the same file
    inspected twice is byte-identical.
20. `kno eval --help` lists `inspect`; `kno eval inspect --help` mentions `--evals`,
    `--value-run-id`, "makes no LLM call", and the vendor-API caveat for remote sources.
21. An eval source with zero Cases is refused with `errs.ErrInvalidInput` (exit 1) naming
    `--evals` — not reported as "0 behaviors, 0 checks flagged".
22. A malformed JSONL line surfaces the adapter's fatal error with file and line context and
    exits 1; no partial analysis is printed.
23. **The standing conditional appears exactly once and before any per-tag number** in the
    human rendering, and its sentence is the first element of `--json`'s `notes` array; the
    suggestions block is introduced by the conditional header. A golden pins both, and a test
    asserts no per-tag suggestion is emitted outside that conditional framing *(F1)*.
24. `observed.min_detectable_harm` is `Plan.MinDetectableHarm` unchanged, carries
    `"one-sided"`, and is never compared to or substituted for `separable_effect`, which
    carries `"two-sided"` *(F2)*.
25. `behavior_separation` appears in **no** `checks` array and produces no status in either
    rendering; `multi_behavior_share` is still emitted, and a test asserts that flipping the
    share from 5% to 95% changes `checks_flagged` by zero *(F3)*.
26. Concentration is computed as *(dev Cases carrying the most common normalized tag) ÷ (all
    dev Cases)*, non-exclusively: a fixture in which every Case carries both
    `overall_quality` and a second tag reports concentration 100%, and the rendered line reads
    `carried by`, never `is the only behavior on`. The untagged share and the multi-behavior
    share use the same denominator *(F4)*.

## Alternatives considered

**Define a behavior as a Goal rather than a tag.** Rejected on the facts: `resolveGoal`
accepts one name, `goal/` contains one implementation, `judge/` contains only `doc.go`, and
there is no Case→Goal association anywhere in `case.proto`. Every eval set would report
exactly one behavior. It is also the wrong axis even when judged Goals land: the engine
clusters and attributes by tag, so a Goal-based behavior count would describe a structure the
measurement machinery does not use.

**Require a Value run (make `--value-run-id` mandatory).** Rejected: it inverts the command's
purpose. The value of `inspect` is that it runs *before* the spend it might prevent, and a
version that demands a completed value run can only tell you your eval set was inadequate
after you paid to find out. The observed section is strictly additive.

**Emit the adjectival grade (`Attribution quality: MODERATE`).** Rejected in full above: the
command's own second finding condemns single coarse scores, so emitting one is self-refuting;
and a word blending five checks with five different fixes is exactly the unactionable number
`evaluation-design.md` §8 names as anti-pattern one. Replaced by three-state per-check
statuses (the `GapStatus` discipline) plus an ordinal `N of M` count.

**Report `"overall_quality" accounts for 62% of total score`.** Rejected because it is not
computable: no `Goal` in this build populates `Score.components` and no store reader surfaces
them. Replaced by Case-share concentration, which measures the same anti-pattern from the
data that exists. The trigger for revisiting is recorded in the ledger.

**Make findings non-zero exit codes so CI can gate on them.** Rejected: `ExitError` (1) means
"something is broken" and README's exit-code table is a published contract; overloading it
with "your eval set is coarse" trains people to ignore 1, which the table itself warns
against. `--json`'s `checks_flagged` is the gate for anyone who wants one.

**Put the analysis in `core/` as a reusable stage.** Rejected for now: `inspect` produces no
Run, no Valuation and no persisted artifact, and every input it reads is already exposed by
`core.Evals` and `store.Store`. Putting it in `core/` would add a public surface with a
stability promise for something whose output shape is a CLI report. It moves to `core/` the
day the API needs to serve it — named here so the move is a decision rather than a surprise.

## Affected packages

`cli/` (`cli/eval.go` for the parent, `cli/evalinspect.go` for the child, the `--json`
struct in `cli/jsonreport.go`, one `AddCommand` line in `cli/root.go`, and a refactor of
`resolveEvals` — which today takes `*baselineFlags` (`cli/evals.go:61`) and must be
generalized to take the four fields it actually uses: eval path, holdout fraction, split
seed, and the two endpoint-security opt-outs). `stats/interval` (new exported
`MinDetectableEffect`). `core/value` (`minDetectableHarm` delegates; `t95OneSided` deleted;
`NormalizeTag` exported). `docs/` (`evaluation-design.md`'s closing section rewritten to
present tense, a cookbook entry, the README status table). `docs/debt.md` (one new entry for
the components deferral). `CHANGELOG.md`. **Nothing in** `store/`, `proto/`, `adapters/`,
`bridge/`, `plugin/`, `api/`.

## Proto / schema impact

**None.** Verified against `proto/`:

- No new message. The output is a CLI report, not a wire type.
- No new field. Everything read already exists: `Case.tags` (6), `Case.split` (5),
  `Case.provenance` (7), `Run.value_plan` (29), `Run.baseline_run_id` (30),
  `Run.eval_content_hash` (21), `Valuation.delta_interval`, `Gaps`/`GapCluster`.
- No new `Stage` and no new `Event` oneof member: `inspect` creates no Run and emits no
  events, because it performs no measurement. Adding one would be a new user-visible state
  with nothing to correlate it to.
- No `store.Store` method: `GetRun`, `Valuations` and `CaseScores` all exist.
- No `kno.yaml` key: `inspect` adds no configurable behavior, so `configSpecs` is untouched
  and `configVersion` stays 1.

`buf lint` and `buf breaking` are no-ops for this change. The Go API additions
(`interval.MinDetectableEffect`, `value.NormalizeTag`) are additive and pre-1.0.

## Edge cases

| Case | Behavior |
|---|---|
| Eval source with zero Cases | Refused, `ErrInvalidInput`, exit 1, fix names `--evals` (criterion 21) |
| Eval source where every Case is holdout (`--holdout-frac` near 1) | Dev count 0; behavior table empty; `behaviors_declared` FLAGGED with detail "no dev Cases"; `holdout_powered` still evaluated. Exit 0 |
| No Case carries any tag | Zero behaviors, `behaviors_declared` FLAGGED naming `ModeAllFailed`; the other checks still run |
| Every Case carries exactly one tag, all the same | One behavior; `behavior_concentration` FLAGGED at 100% |
| Every Case carries the dominant tag **plus** a second tag | Concentration is still 100% — membership is non-exclusive, per the pinned definition; the multi-behavior share is also 100% and is reported, not flagged *(F4, F3)* |
| Half the dev Cases are untagged and the rest all carry one tag | Concentration is 50% over *all* dev Cases (not 100% over tagged ones) and does not flag; `untagged` is 50% and does not flag; both denominators are the dev total *(F4)* |
| Tags that are not behaviors (`p0`, `regression-2024`) | Reported as behaviors, because the tool cannot tell — the standing conditional above the table says so, and the suggestions are framed conditionally *(F1)* |
| Tags differing only in case or surrounding space | Collapsed by `value.NormalizeTag`, and the collapse is **reported** with the spelling count |
| Empty-string or whitespace-only tag | Skipped, matching `cluster()`; counted in a `blank_tags` note so it is not silently dropped |
| A Case tagged with the same tag twice | Counted once; the duplicate reported, matching `snapshotClusters`' `NDropped` |
| 1M-Case eval set | Streams; memory O(distinct tags). No Case IDs retained in the Evals-only path |
| A single Case with 500 tags | 500 behaviors, each with 1 dev Case, all `underpowered`. The table is truncated to the top 50 by dev Case count with "…and 450 more (see --json)"; `--json` carries all of them |
| Malformed JSONL line | Adapter's fatal error surfaces with file:line; exit 1; no partial analysis (criterion 22) |
| Duplicate Case IDs | The `jsonl` adapter is already fatal on these; the error surfaces unchanged |
| Case with `input` but no `expected` and no `rubric` | Counted; a note reports how many Cases have neither, since `exact-match` scores those as failures by construction |
| Remote source (`langsmith:` etc.) with bad credentials | The adapter's Actionable refusal, unchanged; the per-source fix lines in `cli/evals.go` are reused |
| Remote source, network unreachable | Adapter error surfaces; exit 1 |
| `--value-run-id` given, `--db` absent/unreadable | Refused, exit 1, fix names `--db` |
| `--value-run-id` names an unknown run | Refused, exit 1; the fix line names where run IDs come from (`kno value` prints them) — `store` has **no** `ListRuns`, so no command is claimed that does not exist |
| `--value-run-id` names a *baseline* run (wrong stage) | Refused, exit 1, naming the run's actual stage |
| `--value-run-id` run has empty `value_plan` (budget-stopped before planning) | `attribution_observed: unknown`, detail names the absence, exit 0 |
| `--value-run-id` run's `value_plan` fails to gob-decode | Same UNKNOWN branch — the treatment `core/export.go:243-248` already applies. Never a panic |
| `--value-run-id` run is `BUDGET_STOPPED` or `INTERRUPTED` | Observed section rendered **with the run status**, like Select's source-run rule; verdicts labeled partial |
| Eval source changed since the run (`eval_content_hash` mismatch) | Observed section omitted, `unknown` with "the eval source has changed since this run" |
| Non-TTY stdin or stdout | No effect: nothing prompts, nothing redraws, no raw mode. Identical output |
| CI | Same. Exit 0 regardless of findings; `checks_flagged` in `--json` is the gate |
| `kno.yaml` present | Read for `--db` and the eval-source flags via the normal `configSpecs` path, like any read-only command. No spend key is consulted, because no spend happens |
| Holdout | Per-Case analysis goes through `core.Seal`; holdout appears only as a count. Canary test (criterion 15) |
| A Case whose `input` contains something secret | Never rendered: criterion 14's sentinel test. `inspect` prints tags, counts and IDs only |
| Interrupted (Ctrl-C) mid-read of a large source | ctx cancellation propagates through the iterator's pre-yield check; exit 4, nothing partial printed |

## Test plan

**Unit (`cli/`, table-driven, `t.Parallel()`, subtests named in vocabulary terms):**

- Behavior extraction: normalization collapse and its report; empty tags; duplicate tags on
  one Case; zero-tag file; single-tag file; 500-tag Case truncation.
- Check verdicts at every threshold boundary: dev Cases at 4 / 5 / 6 against
  `core.MinClusterCases`; holdout at 19 / 20 / 21 against `split.MinHoldout`; concentration at
  49 / 50 / 51%. Each test reads the exported constant, so changing the constant changes the
  test's expectation rather than breaking it.
- `checks_total` is **5** with and without a run; `attribution_observed` never `ok` without
  one; `behavior_separation` appears in no `checks` array and a multi-behavior share swept
  from 0 to 1 never moves `checks_flagged` *(F3)*.
- Concentration semantics *(F4)*: the non-exclusive fixture (every Case carries the dominant
  tag plus a second), the untagged-denominator fixture, and a rendering assertion that the
  word "only" never appears in the concentration line.
- The standing conditional *(F1)*: present exactly once, above the table, in both renderings;
  present as `notes[0]` in `--json`; the suggestions header carries it.

**Statistical (`stats/interval`):**

- **The characterization test** (criterion 5): `MinDetectableEffect(m, SIDEDNESS_UPPER, 0.95)`
  reproduces the deleted `t95OneSided`-based `minDetectableHarm` for `m` in 1..40 to full
  float64 equality. This is the test that makes the refactor safe, and it is the one that
  would fail if someone "simplified" the t table into a z approximation — which would silently
  narrow a reported bound on exactly the small samples where it matters.
- **Sidedness ordering** *(F2)*: `MinDetectableEffect(m, SIDEDNESS_TWO_SIDED, 0.95) >
  MinDetectableEffect(m, SIDEDNESS_UPPER, 0.95)` for every `m ≥ 2`. This is the property that
  makes the one-sided figure an overstatement of power for `inspect`'s question, so it is
  asserted rather than assumed.
- Monotonicity property: the separable effect is strictly decreasing in `m` for `m ≥ 2`, in
  both sidednesses.
- `m < 1` returns the same "nothing is detectable" answer, never a small number that reads as
  a tight bound.

**Golden:**

- Full human output and `--json`, across: a healthy eval set, an untagged one, a
  heavily-multi-tagged one, one with a dominant tag, one with an underpowered holdout, and
  each with and without `--value-run-id`.
- Human/`--json` equivalence golden (the `report` plan's convention).
- Help substring assertions (`cli/cli_test.go`'s existing convention — substrings, not full
  golden files).

**Integration (`cli/`, against a real SQLite store):**

- Drive a real `baseline` + `value` against `fake:` over a tagged fixture, then inspect with
  `--value-run-id`: assert the `observed` section's routing mode, cluster verdicts, and
  per-behavior baseline failure counts match what `core.ComputeGaps` and `store.CaseScores`
  return directly.
- Fingerprint mismatch: edit the eval file after the run; assert the observed section is
  withheld.
- Run with an empty `value_plan`; run in `BUDGET_STOPPED`; run of the wrong stage.

**Security / honesty:**

- **Content sentinel** (criterion 14): every Case's `input`, `expected`, `rubric` and
  `Turn.content` set to a unique sentinel; assert none appears in stdout, stderr, or
  `--json`.
- **Holdout canary** (criterion 15): a holdout Case with a tag that appears nowhere else;
  assert that tag never appears in the behavior list.
- **No-spend**: assert no Agent is constructed and no dial occurs for a `jsonl` source.

**What FAILS if this regresses:** the characterization test catches a changed statistical
bound; the sentinel test catches a debugging line that prints a prompt; the canary catches a
seal being dropped from the read path; the constant-reading threshold tests catch a hardcoded
5 or 20 drifting from `core.MinClusterCases` / `split.MinHoldout`; the equivalence golden
catches a caveat surviving in one renderer and not the other.

## Rollback

Delete `cli/eval.go`, `cli/evalinspect.go`, the `--json` struct, the goldens, and the
`AddCommand` line. `inspect` writes nothing, creates no Run, and persists nothing, so there
is no data to migrate back.

Two changes outlive a naive revert and are reverted explicitly in the same commit:
`interval.MinDetectableEffect` (with `core/value.minDetectableHarm` restored to its inline
table) and `value.NormalizeTag`'s export. Both are additive, both are covered by the
characterization test, and neither is referenced by anything else — but leaving them behind
as unused exported symbols is the dead-surface trap the debt ledger records elsewhere, so the
rollback removes them.

## Docs impact

- **`docs/evaluation-design.md`**: the closing "The inspect idea" section is rewritten from
  future tense to present, with the actual output and — critically — the separable-effect
  table above, which turns §2's "~10+ Cases per behavior" heuristic into the arithmetic it is
  a shorthand for. §2 keeps the heuristic and gains a pointer to the number.
- **`docs/what-the-numbers-mean.md`**: a new subsection defining `separable_effect` — that it
  is a **two-sided** 95% bound computed from the worst-case paired-binary standard deviation
  and the sample size alone, that it is a bound rather than an estimate from the user's data,
  and that it is therefore printable before any measurement exists. The same subsection states
  why it differs from `Plan.MinDetectableHarm`'s one-sided figure — symmetric question versus
  directional one — so a reader who sees both numbers is not left to guess *(F2)*. If a PR
  changes what that number means, this page changes in the same PR.
- **`docs/cookbook/`**: a new entry, "Check whether your evals can attribute anything",
  linked from the cookbook index.
- **README**: the "Evaluation best practices" section (README:221) gains the one-line command;
  the status table gains no row, because `inspect` is not a stage.
- **CLI help**: `kno eval --help` and `kno eval inspect --help`, substring-asserted.
- **`docs/debt.md`**: one new entry — the score-decomposition line deferred, with the
  trigger *"when the first Goal that populates `Score.components` merges"* and an owner. Not
  "someday": the trigger is a merge event a reviewer can check.
- **CHANGELOG**: `### Added` under Unreleased.
- **DESIGN.md**: `kno eval inspect` appears in `docs/evaluation-design.md` but **not** in
  DESIGN.md's milestone lists (DESIGN.md:397-399). Flagged rather than resolved silently, per
  CLAUDE.md: the PR adds it to the v0.1 or v0.2 line, or the reviewer rejects the command.

## Accepted risks

- **A tag is not a behavior, and this command treats it as one — in both directions** *(F1)*.
  Under-crediting: a user with a disciplined taxonomy in a field the schema does not have gets
  no credit for it. Over-crediting, which is the worse half and the one the first draft
  missed: tags encoding priority, source or date are reported as distinct behaviors with
  specific separable-effect numbers and directive per-tag suggestions, and the tool cannot
  distinguish them from real behavior labels. The residual risk after the standing conditional
  is a user who does not read it — the same class of risk as the demo plan's honesty epilogue,
  and mitigated the same way: the conditional is non-optional, printed above every number it
  qualifies, carried in `--json`, and pinned by a golden. The alternative (a declared behavior
  field on `Case`) is a wire change and a new user-facing contract, and it is not worth one
  for a diagnostic. Revisit if a second consumer needs it.
- **~~The 25% multi-behavior flag threshold~~ — resolved, not accepted** *(F3)*. The threshold
  is deleted rather than documented: `behavior_separation` reports and never flags, leaving
  five flaggable checks. Recorded here because a future contributor will want to add a
  threshold back, and the reason not to is the whole thesis — an invented cut-off in a tool
  built to refuse invented cut-offs. If a principled anchor ever appears in the tree, the
  check can return with it.
- **`separable_effect` is two-sided; `Plan.MinDetectableHarm` stays one-sided** *(F2)*. The
  residual risk is two numbers in one output that answer near-identical-sounding questions with
  different values. Accepted, and mitigated by labeling both at every appearance (human and
  `--json`) and by criterion 24. The rejected alternative — reuse the one-sided figure so one
  name means one number — was cheaper to explain and wrong in the dangerous direction: it
  would report eval sets as more powerful than they are for a symmetric question.
- **The worst-case standard deviation makes the bound conservative on non-binary Goals.**
  `sqrt(0.5)` is the paired-binary maximum. For a continuous Goal with lower variance the
  true detectable effect is smaller, so `inspect` over-warns. Conservative in the recoverable
  direction — it never tells a user their eval set is more powerful than it is — and the
  output names the assumption.
- **Dev-only counts under-report behavior size by roughly the holdout fraction.** Correct for
  the power arithmetic, mildly surprising in the headline. Mitigated by the column header
  (`dev Cases`) and by the total/dev/holdout line above the table.
- **`--json` is a new CLI contract.** Additive, hand-written, ADR-0001-compliant, and pinned
  by goldens — but it is a contract, and the `checks` array's `name` values are the part
  people will pin their CI to. They are treated as stable from the first release; renaming
  one is a breaking change with a CHANGELOG note.
- **Exporting `NormalizeTag` freezes routing's normalization as public API.** Pre-1.0, so
  changeable, but a future routing change to (say) Unicode case folding now has a second
  consumer to update. Cheap, and the alternative — a silently-drifting duplicate — is worse.

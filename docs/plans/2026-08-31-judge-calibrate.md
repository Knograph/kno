# `kno judge calibrate`: the agreement gate CLAUDE.md already promises

`CLAUDE.md:69` and `CONTRIBUTING.md:175` both state, as present-tense fact, that "judges are
tested against the human-labeled calibration set with agreement thresholds — a judge prompt
change that drops agreement below threshold fails CI". `DESIGN.md:287` lists
`kno judge calibrate` in the CLI surface and `DESIGN.md:242` calls judges "the epistemic
foundation". `docs/what-the-numbers-mean.md:46` tells the user to run the command before
trusting a judged number.

**None of it exists.** Verified against `main`:

- `find judge -type f` returns exactly one path: `judge/doc.go`. There is no `judge/baml_src`,
  and `find . -name '*.baml'` returns nothing — the whole BAML toolchain `DESIGN.md:340`
  describes is unbuilt. `docs/plans/2026-08-18-repo-foundation.md:130` already records
  `baml-cli` as "confirmed missing on this machine".
- `goal/` contains one implementation, `goal/exactmatch`. `resolveGoal` (`cli/render.go:28`)
  accepts exactly one name and its own fix line says so: *"only `exact-match` is available in
  this build; judged goals land with the judge"*.
- `examples/` does not exist. `DESIGN.md:242` says "`examples/` ships a small human-labeled
  calibration set"; the examples material is planned as a **sibling repository**
  (`docs/plans/2026-08-30-examples-repo.md`), which cannot gate this repository's CI.
- `Case.rubric` (`proto/kno/v1/case.proto` field 4) is parsed by exactly one adapter
  (`adapters/evals/jsonl/format.go:34`, `jsonl.go:171`) and **read by nothing**. `Score.judge_model`
  (field 6) is written by nothing; `goal/exactmatch` deliberately leaves it unset.

`docs/plans/2026-08-30-good-first-issue-taxonomy.md` §"Judge prompts" reached the same verdict
and proposed correcting the docs. That plan is unmerged: `CONTRIBUTING.md:4` and `:306` still
assert the fiction today. **This plan takes the other branch — it makes the claim true** — and
the doc corrections it does not obviate are listed under Docs impact.

**Phase 0. Not implemented. No code written.**

**Phase-1 re-reviewed 2026-08-31 — verdict: amend; amendments applied.** Three things changed,
and a fourth set of claims is now settled fact rather than hedge. The reviewer verified against
`main` and confirmed: `core.Goal` is exactly `Score` / `Domain` / `Direction`, so this plan
defines no new interface; `resolveGoal` (`cli/render.go:28`) is a literal hardcoded
`if name == "exact-match"`; `judge/` is `doc.go` and nothing else, `goal/` is `exactmatch` and
nothing else, and `DESIGN.md`'s `judge/baml_src/` and BAML judges (`:242`, `:340`, `:374`) are
unbuilt aspiration; **the κ algebra checks out** — at prevalence p = 0.5, `p_o = 1 − ε` and
`p_e = 0.5` give `κ = 1 − 2ε` exactly; and `Goal.Score` genuinely runs *after* `res.Settle` /
`res.Release` in `core/invoke.go`'s `once`, so the reservation does not bracket `Score` in
either caller. Those hedges are deleted below.

The dominant finding is that this plan's own P0 containment was **opt-in**: a registry that
refuses a Goal only when the Goal *declares* itself LLM-backed catches honest authors and
nothing else, which is prime directive 4 hidden behind a guard. It is inverted to default-deny
*(F1)*. The bootstrap this plan lands is new statistical machinery gating a binary decision at
small n, so its property test now sweeps the regime that decision actually lives in *(F2)*, and
the prevalence-tolerance figure is derived in a table instead of asserted *(F3)*. Confirmed
sound and not reworked: the three-verdict INDETERMINATE semantics, the contributor-on-ramp
scoping, the calibration-set provenance controls, and the testability of the acceptance
criteria.

## Problem

Three separate holes, and they are not the same hole:

1. **There is no judge.** A judged Goal — one that scores a Case against `rubric` rather than
   `expected` — has no implementation, no prompt, no registration path, and no way to be named
   on a command line. `Run.goal_name` is a passthrough string from `--goal`
   (`cli/baseline.go:326`, `cli/value.go:293`), matched against a hardcoded `if` in
   `resolveGoal`.
2. **There is no way to know whether a judge is any good.** `docs/what-the-numbers-mean.md:46`
   is explicit that judge disagreement is a source of error the confidence interval does not
   cover, and points at a command that does not exist. Every judged number this project ever
   produces is uncalibrated until that gap closes.
3. **A judge is a spend path with no guard.** `Goal.Score` is invoked *outside* the budget
   reservation in both stages that call it: `core/baseline_invoke.go:78` and
   `core/value_loop.go:803` both call `o.Goal.Score(ctx, c, resp)` after `invokeWithRetry` /
   `iv.withRetry` has already settled the reservation that covered the **agent** call. The
   `Estimator` contract (`core/ring0.go`) further requires `Estimate.Calls == 1` per `Invoke`,
   because "one Invoke settles as one provider call". A judge Goal dropped into that seam
   spends money the guard never authorized and never settles — prime directive 4, verbatim.
   Nothing in the tree does this yet because no judged Goal exists, which is the only reason
   this is a design constraint rather than an open P0.

This plan closes (2) and (3) and deliberately does **not** close (1). See "Scope".

## Design

### Scope: this plan ships the harness, the set, and the gate — not a judge

The command is Goal-agnostic. `kno judge calibrate` takes any registered `core.Goal` and
reports its agreement with human labels. It is landed **before** the first judge prompt, in the
red-then-green shape this repo already uses for contributor on-ramps
(`docs/plans/2026-08-30-good-first-issue-taxonomy.md`: "maintainer lands the checker *failing*,
contributor makes it green"), and for the reason `docs/debt.md#42` records about the
resolved-model gate: *"the check has to exist before the adapter does, or the first run with a
moving alias is the one that discovers the gap."*

Concretely, at merge: the harness runs, the calibration set is committed, the CI gate is armed,
and the only calibratable Goal in the build is `exact-match` — which the set is constructed to
give a *known, pinned* agreement, so the harness has an end-to-end test with no LLM in it. The
day the first judge prompt lands, the gate is already there.

**What this means for CLAUDE.md's promise.** After this plan, "judges are tested against the
human-labeled calibration set with agreement thresholds" is true of the *mechanism* and vacuous
in *content* until a judge exists. The docs say exactly that, in those words, rather than
either asserting the mechanism as coverage or continuing to assert coverage that does not
exist.

### What a judge IS in this codebase — from the code, not from DESIGN.md

**`core.Goal` is the interface, and it is sufficient. This plan defines no new one.** A judge
Goal is a `core.Goal` (`core/ring0.go`) whose `Score` consults `Case.rubric` instead of
`Case.expected`, sets `Score.rationale` and `Score.judge_model`, and declares a `Domain()` of
`SCORE_DOMAIN_BINARY` (a verdict) or `SCORE_DOMAIN_UNIT_INTERVAL` (a graded rubric). Every
field it needs already exists on the wire.

Three things the interface does **not** have, and what this plan does about each:

| Missing | Consequence | This plan |
|---|---|---|
| `Name() string` | Not on the interface. `goal/exactmatch` has one; nothing calls it. `Run.goal_name` is a user-supplied string. | Adds a **registry** (`goal.Registry`), not an interface method — see below. Interface unchanged. |
| Any cost surface (no `Estimator` analogue) | A judge that calls an LLM inside `Score` spends outside the guard. | The calibrate command owns its own guard. The pipeline seam is **ledgered, not fixed here** — see "Cost" — and meanwhile registration is **default-deny** *(F1)*. |
| Any notion of a labeled reference | Nothing in the tree pairs a `Score` with a human verdict. | The calibration record, below. |

**The registry.** `resolveGoal` (`cli/render.go:28`) is a hardcoded `if`. Calibrating a Goal
requires naming it, so this plan replaces the `if` with `goal.Registry`: a map from name to
constructor, populated by explicit registration in `cli/` (not `init()` side effects — an
`init()`-registered Goal is a Goal whose presence depends on an import nobody reads). The
registry lives in `goal/` so that `core` still imports nothing above it. `resolveGoal`'s error
keeps its actionable shape and gains the registry's key list, so an unknown `--goal` names what
*is* available instead of naming only `exact-match`.

**The registry is default-deny, and that is the P0 containment** *(F1)*. This plan's first draft
had `goal.Registry` refuse a Goal that *declares* `LLMBacked() bool { return true }`. That gate
is a self-report and it fails **open**: a judge Goal that calls a provider inside `Score` and
simply does not implement the marker — or implements it returning false — registers cleanly and
spends outside the reservation, which is the exact prime-directive-4 violation the containment
exists to prevent, now wearing a guard that catches only the authors who did not need catching.
The debt entry's trigger ("before any judged Goal is added") fires only if a human reads it. The
code path has to fail closed with no human in the loop.

**The polarity is therefore inverted: registration is refused unless the Goal's name appears in a
compile-time allowlist** — `goal.SelfContained`, a `map[string]struct{}` in `goal/` carrying
exactly one entry today (`exact-match`) and a comment naming the debt entry. `Registry.Register`
returns an error for any name absent from it. Absence is unsafe. There is no marker method, no
declaration, and nothing a Goal's author can say to admit their own Goal.

**Why the allowlist rather than an affirmative `SelfContained() bool` every new Goal must
implement.** The must-affirm marker has the better ergonomics and is the weaker guard, for the
reason the finding itself names: it is still a self-report. Inverting the default fixes the
*careless* author — who forgets the marker and is now refused — and does nothing about the
*wrong* one, who writes `SelfContained() bool { return true }` on a Goal that calls a provider,
because it compiles and the tests go green. The allowlist takes the decision away from the Goal
entirely and puts it in a diff to `goal/registry.go`: one line, in the one file where a reviewer
already knows the line means *"someone is claiming this Goal spends nothing."* It is also
mechanically coupled to the debt entry — adding a judged Goal **requires** editing the allowlist,
and the trigger is written in the comment on the line being edited, so the trigger cannot be
missed by failing to read `docs/debt.md`.

The cost, stated: **an out-of-tree Goal cannot register.** Accepted for v0.2. Goals are in-tree
today (`goal/` holds the only implementation, and the plugin boundary `CLAUDE.md` describes is
Ring-2 adapters, not Goals), and the day an out-of-tree Goal is wanted, the plan that lands it is
the plan that fixes the budget seam — which is the same plan that empties the allowlist's reason
for existing. Both halves move together or neither moves.

### The calibration set

**A calibration record is not a Case.** `Goal.Score(ctx, c, r)` needs a `Case` *and* a
`Response` — you cannot calibrate a judge without the agent output it is judging. So:

```
CalibrationRecord {
  id            string            // stable, referenced by the threshold file and issues
  case          Case              // input, rubric, tags; expected optional
  response      Response          // the output being judged, with tool_calls where relevant
  labels        []HumanLabel      // >= 2, independent
  adjudicated   HumanLabel        // the reference verdict; required
  provenance    Provenance        // source = "authored" | "synthetic"; never a customer trace
}
HumanLabel { labeler_id string; value double; passed bool; note string }
```

**Where it lives:** `judge/testdata/calibration/<set-name>/records.jsonl`, plus
`manifest.json` (set name, version, content sha256, class balance, labeler roster).
`testdata/` is where this repo already keeps deterministic inputs, and it keeps the set inside
the module so `go test ./judge/...` and `make check` can reach it. It does **not** go in
`examples/`: `DESIGN.md:242` says it does, but `examples/` is a sibling repo
(`docs/plans/2026-08-30-examples-repo.md`), and a gate whose input lives in another repository
is a gate that cannot fail on a PR. That DESIGN.md line is corrected in this PR.

**Who authors it:** maintainers seed it; contributions are accepted and are a named on-ramp
(below). Every record needs ≥2 independent labels and an adjudicated reference; a record with
one label is a record labeled by one person's judgement, which is exactly the thing the set
exists to hold a judge to.

**Versioning:** `manifest.json` carries `version` (monotonic int) and `content_sha256` over
`records.jsonl`. The recorded threshold is keyed `(set_name, set_version, goal_name)`. A set
edit and a prompt edit therefore cannot be confused for each other — the gate reports which
operand moved. Same discipline as `pricing.Version` on the price table.

**What must never be in it.** The set is public and permanent, and per the security section of
`CLAUDE.md` traces are customer data:

- **No customer or end-user content.** No records harvested from a user's traces, no
  `kno mine` output, nothing carrying `Provenance.derived` from a real deployment. Records are
  authored or synthetic, and `provenance.source` says which.
- **No PII, no credentials, no secrets.** `make secrets-scan` (gitleaks, working tree *and*
  history) already covers the repo; this plan extends the custom golden-file scan to
  `judge/testdata/calibration/**`.
- **No content under a license that forbids redistribution.** `manifest.json` records the
  license of every non-original record.
- A stated, unavoidable consequence: **a public calibration set is contaminated for training
  purposes the day it is published.** Any model trained after publication may have seen it. The
  set is a *regression* instrument — it detects a prompt change making things worse — and is not
  evidence that a judge generalizes. The docs say this; it is an accepted risk, not a defect to
  be argued away.

### The agreement statistic

Not accuracy. The failure this must catch is the degenerate judge: on a calibration set that is
85% "good", a judge that answers "good" unconditionally scores **0.85 raw agreement** and is
worthless. Raw agreement is reported for interpretability and is **never gated on**.

**Gated statistic: Cohen's κ**, with a bootstrap CI over records.

**The bootstrap does not exist yet, and this plan lands it.** Verified: `grep -rn -i bootstrap stats/ --include='*.go'` outside tests returns two *comments* (`stats/interval/interval.go:173` and `:189`) and no implementation — `stats/interval` ships adjusted-Wald, paired-t and sign intervals only, while `Interval.method`'s godoc (`valuation.proto`) lists `"bootstrap"` among recognized method names. That is a third promise in the schema the code does not keep, found while writing this plan. A **percentile** bootstrap over records lands in `stats/interval` here: κ is a smooth function of the confusion counts, the resample unit is the record (which is what makes it valid), and `interval.go:189`'s own comment already records percentile-bootstrap coverage as adequate. BCa is the named upgrade, not the ship. The `stats/` property-testing rule applies: coverage is a property test against synthetic ground truth, not an assertion in prose.

**And it is new statistical code in a repo whose statistics are property-tested, so it is scoped
as new code rather than as a utility** *(F2)*. Percentile intervals are known to under-cover for
statistics whose small-sample distribution is skewed, and κ at n = 50–200 with a true value in
0.55–0.65 — a bounded statistic at a hard decision boundary — is exactly that regime. Under-
coverage there does not produce a visibly wrong number: it silently flips PASS to INDETERMINATE
or INDETERMINATE to PASS, which is the entire output of the command. The property test therefore
sweeps that box explicitly (acceptance criterion 19) instead of testing coverage "in general",
and the gate does not enter `make check` until it passes. If measured coverage near the floor is
materially below nominal, BCa stops being the named upgrade and becomes the ship — a decision to
be made on the measurement, in the PR, and not on a preference stated here.

    κ = (p_o − p_e) / (1 − p_e)

The constant judge above gets p_o = p_e = 0.85, so **κ = 0 exactly**. That is the property being
bought, and it is a test fixture in this plan, not a claim.

**Always reported beside it, because κ alone cannot say which way a judge is wrong:**

- **Per-class recall** (sensitivity and specificity). A judge that never says "fail" is the
  costly failure mode — it silently attenuates every delta toward the prevalence — and it is
  invisible in a single scalar.
- **Marginals** (the judge's own class distribution vs the humans'). κ is depressed by extreme
  prevalence (the well-known kappa paradox); publishing the marginals lets a reader see
  whether a low κ is a bad judge or a lopsided set.
- **Inter-human κ** on the same records — the ceiling. A judge cannot be held to an agreement
  its own labelers do not reach. When inter-human κ falls below the floor, the command reports
  **INDETERMINATE — the labels do not agree with each other** and gates on that, not on the
  judge.

**Prevalence is fixed by construction, not compensated for.** The set carries a class-balance
invariant checked by the tooling: the minority class is **≥40% of records**. This is the
mitigation that addresses the kappa paradox at its cause instead of swapping in a statistic
(Gwet's AC1, prevalence-adjusted κ) that reports a flattering number on a lopsided set. The 40%
figure is a set-authoring constraint we control, not an inference from data, and **its
consequence is derived here rather than asserted** *(F3)*. With symmetric error rate ε and
minority-class fraction p, the judge's own positive rate is `p* = ε + p(1 − 2ε)`, chance
agreement is `p_e = p·p* + (1 − p)(1 − p*)`, and observed agreement is `p_o = 1 − ε`. At the
invariant's boundary p = 0.40, `p_e` is at most 0.52 — attained at ε = 0, where
`p_e = p² + (1 − p)²` — which is the claim above. The shortfall of κ against the `1 − 2ε`
identity the floor rests on:

| minority class p | p_e at ε = 0.20 | κ | shortfall vs 1 − 2ε = 0.60 | κ at ε = 0.10 | shortfall vs 0.80 |
|---|---|---|---|---|---|
| 0.50 | 0.500 | 0.600 | 0.0% | 0.800 | 0.0% |
| 0.45 | 0.503 | 0.598 | 0.4% | 0.798 | 0.2% |
| **0.40 — the invariant** | 0.512 | 0.590 | **1.6%** | 0.793 | 0.8% |
| 0.30 | 0.548 | 0.558 | 7.1% | 0.771 | 3.7% |
| 0.20 | 0.608 | 0.490 | 18.4% | 0.719 | 10.1% |

So at the boundary the identity holds to **1.6%** near the floor, which is where the decision is
made; the shortfall widens to ~3.6% only as κ → 0 (at p = 0.40, ε = 0.45: κ = 0.096 against an
ideal 0.100), and that worst case over the whole κ range is the loose "~4%" the first draft
asserted without showing. Below the invariant it degrades fast — at p = 0.20 the identity is
wrong by 18% — which is why balance is enforced at authoring time instead of corrected for
afterwards. The table is pinned as a characterization test beside the ε sweep, so a change to the
statistic that breaks the interpretation fails loudly.

**Non-binary Goals are reported and NOT gated in v0.2.** κ is undefined on continuous scores.
For `SCORE_DOMAIN_UNIT_INTERVAL` the command reports quadratic-weighted κ over the human
label's own anchor bins, Spearman ρ, and MAE — and prints `GATE: not applicable (graded
domain)`. Gating a graded judge needs an anchored scale the calibration format does not yet
carry, and inventing one here would be the invented threshold this plan exists to avoid.
Ledgered with the trigger "when the first `SCORE_DOMAIN_UNIT_INTERVAL` Goal is registered".

### The threshold, and the argument for the number

**κ ≥ 0.60**, and the argument is not Landis–Koch. (Their "substantial" band starts at 0.61.
The coincidence is noted so a reviewer does not mistake it for the reasoning; it is not the
reasoning.)

The derivation, in two steps:

**Step 1 — on a balanced set with symmetric errors, κ is approximately the retained fraction of
any true effect.** Let the judge misclassify at rate ε in both directions, independent of arm
(non-differential misclassification). Then the observed pass rate is
`p* = ε + p(1 − 2ε)`, so for any two arms
`p*_treat − p*_control = (1 − 2ε)(p_treat − p_control)`: **every delta measured through this
judge is attenuated by exactly (1 − 2ε)**. On a balanced set the judge's marginals are also
balanced, so `p_e = 0.5`, `p_o = 1 − ε`, and

    κ = (1 − ε − 0.5) / 0.5 = 1 − 2ε

κ **is** the attenuation factor. That is what makes it the right statistic here rather than a
conventional one: it is denominated in the units of the thing the tool sells.

**Step 2 — pick the attenuation the project will pay for, and read the floor off it.** Power for
a paired comparison scales with the square of the effect size, so attenuating an effect by a
requires `1/a²` times as many Cases to reach the same power. Setting the ceiling at **"a judge
may cost at most 3× your eval budget"** gives `a ≥ 1/√3 = 0.577`, rounded up to **0.60**. The
number is therefore a published price — *using a judge at the floor triples what a measurement
costs relative to a perfect scorer* — and `docs/what-the-numbers-mean.md` states it that way,
so a user who thinks 3× is too generous can set `--min-kappa` higher and knows what they are
buying.

**The assumptions are load-bearing and are checked, not assumed.** Step 1 requires balance
(enforced: ≥40% minority) and symmetry (**not** enforceable — so it is *measured*: the command
fails when `|sensitivity − specificity| > 0.20`, because under asymmetric error κ is no longer
the attenuation factor and can even mask a direction-biased judge). Both checks are gates in
their own right, reported separately, so a failure names which assumption broke.

**The boundary.** Three outcomes, not two — the same discipline `core/gaps.go` uses for
IMPROVED / GAP / UNKNOWN, and for the same reason ("we did not really look" must not read like
"we looked and found nothing"):

| Bootstrap CI on κ vs the floor | Verdict | Exit |
|---|---|---|
| CI entirely at or above 0.60 | **PASS** | 0 |
| CI entirely below 0.60 | **FAIL — below the floor** | 1 |
| CI straddles 0.60 | **INDETERMINATE — the set is too small to decide** | 1 |

INDETERMINATE fails. "We cannot tell" is not "it is fine", and the fix is stated in the output:
add records. Gating on the point estimate alone would let a 40-record set flip a gate on noise;
gating on the CI's lower bound alone would make every small set fail for being small, which is
true but useless as a signal about the judge. Reporting all three states says which problem you
have.

### The CI gate

**PR CI never calls a provider.** `make test-integration` hard-fails if `KNO_LIVE_TESTS` is set
(`Makefile:210`), and `CLAUDE.md` puts live tests on the nightly, capped path. So calibration in
CI is a **replay**: judge responses are recorded fixtures under
`judge/testdata/fixtures/<goal>/<prompt_sha>/<record_id>.json`, keyed by the sha256 of the
prompt files plus the judge model plus the record id.

The mechanism, end to end:

1. `make judge-calibrate-check` (folded into `make check`) runs `kno judge calibrate --replay`
   for every `(goal, set)` pair listed in `judge/calibration.baseline.json`.
2. **A prompt change is detected by hash, not by path.** The recorded baseline carries
   `prompt_sha` per goal. A prompt edit changes the sha, the fixture directory for that sha
   does not exist, and the gate fails with `no recorded judge responses for prompt <sha> — run
   'make record-calibration'`. Path-based detection was rejected: it fires on a whitespace edit
   and misses a prompt assembled from a file outside `judge/`.
3. The contributor runs `make record-calibration` — the live path, guarded by the Makefile's
   existing `live_spend_guard` (`Makefile:121`, which requires `KNO_MAX_COST_USD`) — commits
   the fixtures and the regenerated baseline, and CI replays them deterministically.
4. **Two gates on the numbers:** the **absolute floor** (κ ≥ 0.60, above), and a **ratchet**:
   κ may not fall by more than the 95% CI of the *paired* bootstrap difference between the new
   and recorded runs on the same records. Paired, because both runs judge the identical set and
   an unpaired comparison of two independent CIs throws away that pairing and is far too
   permissive.

**How a legitimate improvement gets through.** A prompt change that *raises* κ passes: the
ratchet is one-sided. A change that lowers κ deliberately — a prompt broadened to a new class of
rubric, a cheaper judge model traded against agreement — passes by updating
`judge/calibration.baseline.json` in the same PR, exactly as `.coverage-baseline` and
`make update-coverage-baseline` (`Makefile:246`) already work in this repo, with the same
convention: *"review the diff like code"*. The PR body must link a plan or state the trade. The
gate is a speed bump with an audit trail, not a wall — a wall would be routed around by
deleting records, which the set's content hash makes visible.

`make selftest` (`Makefile:418`, "prove each gate FAILS when its invariant is broken") gains
three cases: the degenerate always-good judge, a fixture-sha mismatch, and a κ regression
larger than the paired CI.

### Cost: this is a spend path, and the guard applies

`kno judge calibrate --live` calls a judge model once per record per trial. It is guarded like
every other spend path:

- The command builds a `budget.Guard` (`stats/budget`) from `--max-cost-usd` / `--max-calls`,
  and reuses the CLI's existing consent machinery (`cli/consent.go`, `confirmThresholdUSD` =
  $1.00 at `cli/render.go:25`). A calibration run over 200 records is quotable up front — the
  record count is known before anything is sent, unlike an eval stream — so the pre-run quote is
  exact rather than a worst-case bound.
- One `budget.Estimate` per judge call with `Calls: 1`, settled from reported usage.
- `--replay` (the default in CI) makes **zero** provider calls and constructs no guard: there is
  no spend to authorize. The help text says "costs nothing" about LLM spend specifically, the
  posture `cli/doctor.go` and the `kno eval inspect` plan already take.
- A calibration run that hits its cap stops and reports `BUDGET_STOPPED` with a partial κ
  **suppressed**: a κ over the records that happened to fit under the cap is a κ over a
  population nobody chose. It reports the count and no statistic. (Same reasoning as the
  purged-run baseline in `docs/what-the-numbers-mean.md`.)

**The pipeline seam is NOT fixed here.** `Goal.Score` runs outside the reservation
(`core/baseline_invoke.go:78`, `core/value_loop.go:803`) and `Estimator` requires exactly one
call per `Invoke`. Fixing that means either a second reservation around `Score` or a
`Goal`-side cost surface, and both are `core` changes with an event and a settlement story that
belong to the plan that lands the first judged Goal. **This plan adds a debt entry** with the
trigger *"before any judged Goal is added to `goal.Registry`"* — a trigger that cannot
self-satisfy, because this plan registers no judged Goal. Until then the registry refuses
**every** Goal whose name is absent from the `goal.SelfContained` allowlist, so the seam cannot
be crossed by accident *or by omission* *(F1)*: the refusal is the enforcement, not the comment,
and it never asks the Goal whether it spends.

### The contributor on-ramp, concretely

What a contributor can do the day this merges, which they cannot do today:

1. `kno judge calibrate --goal <name> --replay` — free, offline, no API key. Prints κ with its
   CI, per-class recall, marginals, the inter-human ceiling, and the verdict.
2. `kno judge calibrate --goal <name> --replay --show-disagreements` — the table of records the
   Goal gets wrong: record id, human verdict, judge verdict, judge rationale, the rubric. This
   is the artifact that makes a prompt edit a directed act instead of a guess.
3. Edit the prompt. Re-run. See the κ delta and which records moved.
4. `make record-calibration` under a capped budget when they have a key; open a PR where CI
   shows the paired κ difference and the ratchet's verdict.

And two issue shapes that satisfy the `good-first-issue` G3 rule ("a test to make pass"):

- *"The `<X>` judge scores these 9 calibration records wrong — here they are"*: the failing
  observable ships with the issue.
- *"Add calibration records for `<behavior>`"*: a set contribution, gated by the balance
  invariant, the ≥2-labeler rule, and the provenance rules. This is the on-ramp that exists
  **before** any judge does, and it is why the set is a deliverable in its own right.

## Acceptance criteria

Numbered, testable, each naming an observable.

1. `kno judge calibrate --goal exact-match --replay` exits 0 and prints a report containing
   `kappa`, a two-sided CI on kappa, `raw_agreement`, `sensitivity`, `specificity`,
   `inter_human_kappa`, the judge and human marginals, `n_records`, and one of
   `PASS` / `FAIL` / `INDETERMINATE`. Golden-file test.
2. A synthetic Goal that returns `passed: true` for every record scores **κ = 0.0 ± 1e-9** on the
   committed set and the command exits **1** with verdict `FAIL`, while its `raw_agreement`
   printed on the same screen exceeds 0.5. Test name:
   `TestConstantJudgeScoresZeroKappaDespiteHighRawAgreement`.
3. A synthetic Goal with 20% symmetric error scores κ within ±0.05 of 0.60 on the committed set
   — the `κ ≈ 1 − 2ε` identity, pinned as a characterization test, not asserted in prose.
4. A synthetic Goal with sensitivity 0.95 and specificity 0.55 exits **1** naming
   `asymmetric error` even when its κ exceeds 0.60. Test:
   `TestAsymmetricJudgeFailsEvenAboveTheFloor`.
5. A calibration set whose minority class is below 40% is **refused by the loader** with an
   actionable error naming the observed balance; the command exits 1 and computes no κ.
6. A set whose inter-human κ is below the floor produces verdict `INDETERMINATE` naming the
   labels — not the judge — as the cause, and exits 1.
7. On a 30-record subset where the κ CI straddles 0.60, the verdict is `INDETERMINATE`, the exit
   code is 1, and the fix line names adding records.
8. `make check` fails when `judge/testdata/calibration/*/records.jsonl` is edited without
   `manifest.json`'s `content_sha256` being regenerated.
9. Changing any byte of a prompt file changes `prompt_sha`; `make judge-calibrate-check` then
   fails with `no recorded judge responses for prompt <sha>` and names
   `make record-calibration`. Driven by mutating a fixture prompt in the test.
10. A recorded baseline κ of 0.80 and a new run of 0.62 with a paired-difference CI excluding
    zero fails the ratchet; the same drop with a CI containing zero passes. Both directions
    tested — the second is what stops the ratchet firing on noise.
11. Regenerating `judge/calibration.baseline.json` in the same commit makes a deliberate
    regression pass, and the diff shows old κ, new κ, and the set version.
12. `kno judge calibrate --replay` makes **zero** provider calls: asserted by a transport that
    fails the test on any outbound request, not by inspection.
13. `kno judge calibrate --live` with `--max-cost-usd 0.01` over a set whose quote exceeds it is
    refused **before the first call**, exit 2, naming the cap. A run stopped mid-set reports the
    record count and **no κ**.
14. `--json` output validates against the documented shape and contains every field in (1);
    per ADR-0001 the JSON is the same data, not a second rendering.
15. **The registry fails closed** *(F1)*. `goal.Registry` refuses to register any Goal whose name
    is absent from the `goal.SelfContained` allowlist, with an error naming the allowlist and
    `docs/debt.md#<new>`. Three cases, and the first is the one that matters: a Goal that calls a
    stub provider inside `Score` and declares **nothing at all** is **refused** — the test asserts
    refusal, not admission (`TestUnmarkedLLMBackedGoalIsRefused`). A Goal that declares itself
    self-contained by any means it likes is **still** refused for being absent from the
    allowlist. `exact-match`, which is on it, registers. `resolveGoal`'s unknown-goal error lists
    the registry's keys.
16. `grep -rn "judge prompt" CONTRIBUTING.md CLAUDE.md` returns no claim that judge prompts are
    an on-ramp *today*; every such site says adapters and calibration records are the on-ramp
    now and judge prompts become one when the first judge lands.
17. `make selftest` fails when any of: the degenerate-judge check, the prompt-sha check, or the
    ratchet is removed from the gate.
18. No calibration record carries `Provenance.derived`; the secrets scan covers
    `judge/testdata/calibration/**` and `make check` fails on a planted key.
19. **Bootstrap coverage is measured in the regime the gate lives in** *(F2)*. The percentile
    bootstrap CI on κ is coverage-property-tested against synthetic ground truth over the full
    grid n ∈ {50, 100, 200} × true κ ∈ {0.55, 0.575, 0.60, 0.625, 0.65} at the nominal 95% level,
    with the measured coverage recorded in the PR description. The grid is not decoration: it is
    the box inside which PASS / INDETERMINATE / FAIL is decided. `make judge-calibrate-check` is
    not folded into `make check` until it passes.
20. The prevalence sensitivity table of *(F3)* is a characterization test: κ recomputed from
    (p, ε) at each of its five rows matches to 1e-3, so the derivation the floor rests on cannot
    drift away from the doc silently.

## Alternatives considered

**A. Gate on raw agreement (accuracy).** Rejected — it is the exact failure mode the plan
exists to catch. A constant judge scores the set's prevalence. Kept as a *reported* number
because it is the only one a non-statistician reads correctly at a glance, and reporting it
beside a κ of 0 is itself the lesson.

**B. Gate on Gwet's AC1 or prevalence-adjusted κ.** Genuinely better behaved under extreme
prevalence, and rejected anyway: they make a lopsided set report a comfortable number, which
removes the pressure to fix the set. Balance is a property we control at authoring time, so it
is enforced at authoring time. AC1 becomes the right answer if a future set cannot be balanced
by construction — named as the upgrade path, not shipped.

**C. Per-class F1.** Rejected as the *gate*: F1 has no chance correction and no interpretation
in the units of the product. Its components (precision/recall per class) are reported, because
direction of error is exactly what κ hides.

**D. Threshold from the literature (Landis–Koch κ > 0.61 "substantial").** Rejected explicitly.
The `kno eval inspect` plan had an invented 25% threshold deleted on review *(F3 there)*; a
threshold imported from a 1977 paper about medical raters is the same defect wearing a
citation. The attenuation derivation above yields a number *and* an interpretation the user can
overrule with a reason.

**E. Ratchet only, no absolute floor.** Tempting — it is literally what `CLAUDE.md` says ("a
change that drops agreement below threshold") and it needs no defended number. Rejected: a
ratchet alone blesses whatever the first committed prompt scored. If the first judge lands at
κ = 0.25, the ratchet protects 0.25 forever and every judged number in the product is
attenuated by 75% with a green build. The floor is what makes the ratchet mean something.

**F. Ship a judge Goal in this plan.** Rejected on scope and on sequence. A judge needs a prompt
toolchain decision (BAML per `DESIGN.md:374`, or Go templates), a provider-agnostic judge
client, the budget seam fixed in `core`, and calibration to prove it works. Landing calibration
first means the judge arrives with a gate already pointed at it, rather than the gate arriving
later and grandfathering whatever shipped.

**G. Put the calibration set in `examples/` as `DESIGN.md:242` says.** Rejected: `examples/`
does not exist and is planned as a sibling repository; a gate whose input lives in another repo
cannot fail a PR here. DESIGN.md is corrected instead.

**H. Live calibration in PR CI.** Rejected — `Makefile:210` already forbids it structurally, and
a gate that spends money per PR is a gate contributors will be told to skip.

## Affected packages

`judge/` (the harness: loader, statistics, replay, fixtures, the committed set), `goal/`
(`Registry`; `exactmatch` unchanged), `stats/interval` (**the repo's first bootstrap** — percentile over records, plus the paired
bootstrap for the ratchet; this is the package that owns interval construction and a second
implementation elsewhere would drift), `cli/` (`kno judge calibrate`; `resolveGoal` moves to the
registry), `docs/` (`what-the-numbers-mean.md`, `evaluation-design.md` §3,
`docs/cookbook/calibrate-a-judge.md`, mental model), `CLAUDE.md` + `CONTRIBUTING.md` +
`DESIGN.md` (the corrections above), `docs/debt.md` (two new entries), `Makefile`
(`judge-calibrate-check`, `record-calibration`, `selftest` cases), `.github/workflows/`
(nightly live calibration on the existing capped path).

**`core/` is untouched.** The command constructs a Goal and a Guard and reads a testdata file;
it is not a pipeline stage, creates no `Run`, and writes nothing to the store — the `kno doctor`
posture (`cli/doctor.go:23`).

## Proto / schema impact

**None required, and that is verified rather than assumed.**

- `Score` already carries `rationale` (field 5) and `judge_model` (field 6);
  `proto/kno/v1/case.proto`.
- `Case.rubric` already exists (field 4).
- `ScoreDomain` already distinguishes `BINARY` from `UNIT_INTERVAL`
  (`proto/kno/v1/common.proto`).
- `Stage` (`proto/kno/v1/run.proto`) has five values and gains none: calibration is not a
  pipeline stage, creates no `Run`, and emits no `Event` — the same call the `kno eval inspect`
  plan makes. The next free `Event.payload` field number is **26** (25 is `export_written`),
  recorded here so a future plan does not have to re-derive it.
- The calibration record is a **testdata format, not a wire type**. It is never returned by the
  API, never streamed, never persisted to the store. Making it a proto message would put a
  schema covenant around a file only the test harness reads. If `serve` ever exposes calibration
  results, that plan adds the message then.

`buf lint` and `buf breaking` are unaffected; `make typecheck-proto` is a no-op for this change.

## Edge cases

| Case | Behavior |
|---|---|
| Calibration set absent or unreadable | Actionable error naming the path; exit 1. Never a silent zero-record κ. |
| Set has fewer than 2 records | Refused: no interval is computable. Names the minimum. |
| Minority class below 40% | Refused at load, naming observed balance. No κ computed. |
| A record has fewer than 2 human labels | Refused at load, naming the record id. |
| Labelers disagree on a record and no adjudicated verdict | Refused at load. The set may not contain an unresolved record. |
| Inter-human κ below the floor | `INDETERMINATE`, cause named as the labels; exit 1. |
| Judge errors on some records | Errored records are **excluded from κ and counted separately**, matching `what-the-numbers-mean.md`'s errors policy. Above a 5% error rate the run reports `not a usable calibration` — the same threshold and the same words as the baseline gate. |
| Judge returns a value outside its declared Domain | The record is an error, not a verdict. Counted as above and named in the output. |
| κ CI straddles the floor | `INDETERMINATE`; exit 1; fix line says add records. |
| Perfect agreement (κ = 1) | Legal. Reported. The bootstrap CI collapses to a point; the method field says so rather than implying precision from a degenerate resample. |
| p_e = 1 (all labels one class) | κ is 0/0. Refused by the balance invariant before it can arise; belt-and-braces guard returns "undefined" rather than NaN. |
| Graded (`UNIT_INTERVAL`) Goal | Reported (weighted κ, ρ, MAE); `GATE: not applicable`; exit 0. Never silently gated on a statistic it does not have. |
| Fixture present, prompt sha changed | Gate fails naming the sha and `make record-calibration`. |
| Fixture missing for one record only | Fails naming the record. A partial replay would compute κ over a subset chosen by which fixtures happened to exist. |
| `--replay` and `--live` both given | Refused: two sources of truth for one number. |
| `--live` with no API key | The existing credential error path; no partial run. |
| `--live` stopped by the cap | Exit 2, `BUDGET_STOPPED`, record count reported, **no κ**. |
| Two Goals share a name in the registry | Registration panics at startup (programmer error, per `CLAUDE.md`'s panic rule). |
| Judge model differs between the baseline and this run | Reported as a distinct cause: the gate says *model changed*, not *prompt regressed*. The baseline records both. |
| Set version bumped and prompt changed in one PR | Both operands moved; the gate reports the ratchet as **not comparable** and requires the baseline update, which is the same audit trail. |
| Holdout | Not applicable and stated: calibration records are not eval Cases, live in `judge/testdata/`, and are never loaded through an `Evals` adapter. The seal is untouched. |

## Test plan

What must fail if it regresses:

- **The degenerate judge.** `TestConstantJudgeScoresZeroKappaDespiteHighRawAgreement`. Verified
  failing against a raw-agreement implementation before the κ implementation is written. This
  is the single test that carries the plan's statistical claim.
- **The κ ≈ 1 − 2ε identity** as a characterization test across ε ∈ {0.05, 0.10, 0.20, 0.30}
  with a seeded synthetic judge — so a future change to the statistic that breaks the
  interpretation the threshold rests on fails loudly.
- **Asymmetry detection**: a judge above the floor on κ and below it on symmetry fails.
- **Boundary triplet**: three fixed sets producing PASS, FAIL, INDETERMINATE, pinned by exit
  code and by golden output.
- **Ratchet, both directions**: a real drop fails; a drop inside the paired CI passes. Property
  test over seeded resamples for the false-positive rate of the ratchet.
- **Paired-bootstrap correctness**: the paired difference CI is narrower than the difference of
  two independent CIs on the same data — the property that justifies the pairing.
- **Set invariants**: balance, ≥2 labels, adjudication present, content hash, provenance not
  derived. Each verified failing with a deliberately broken set in `judge/testdata/bad/`.
- **Registry default-deny** *(F1)*: `TestUnmarkedLLMBackedGoalIsRefused` — a Goal that calls a
  stub provider inside `Score` and declares nothing is refused registration; a Goal that declares
  itself self-contained is also refused while absent from the allowlist; `exact-match` registers.
  The stub provider fails the test if it is ever called, so an admitted Goal fails twice.
- **Bootstrap coverage on the decision grid** *(F2)*: coverage against synthetic ground truth
  over n ∈ {50, 100, 200} × κ ∈ {0.55…0.65}, per criterion 19, with the measured table in the PR.
  Plus the degenerate resamples: κ = 1, a single class, and n = 2.
- **Replay purity**: a transport that fails the test on any request.
- **Budget**: quote-before-spend, refusal under a cap, no κ on a partial run, settlement from
  reported usage.
- **CLI**: help snapshot, `--json` shape, exit codes 0/1/2, `resolveGoal` error listing registry
  keys.
- **`make selftest`** proves the three new gates fail when broken.
- **Docs gate**: `make docs` fails if the command's help changes without the cookbook page
  changing; the link checker covers the new page.
- **Secrets**: planted-key test over the calibration directory.

Coverage: `judge/` is a new package and lands above the 85% floor; `.coverage-baseline` is
regenerated in the same PR per the ratchet.

## Rollback

Deleting `judge/` (harness, set, fixtures), the `kno judge` command, and the three Makefile
targets restores today exactly. Two things do not roll back with them and are reverted
explicitly in the same deletion:

- `goal.Registry` and `resolveGoal`'s move onto it. Reverting restores the hardcoded `if`; no
  data is affected because `Run.goal_name` was always a string.
- The doc corrections. Rolling those back would restore claims known to be false, so they stay
  regardless — a rollback of the code is not a rollback of the honesty.

No schema change, no store change, no migration. Nothing persisted references calibration.

## Docs impact

- **`CLAUDE.md:69`** and **`CONTRIBUTING.md:175`** — the agreement-threshold claim becomes true
  of the mechanism, and says plainly that no judge exists yet.
- **`CONTRIBUTING.md:4` and `:306`** — the two on-ramp assertions. Amended to: adapters and
  **calibration records** are the on-ramp today; judge prompts become one when the first judge
  lands. (`docs/plans/2026-08-30-good-first-issue-taxonomy.md` found both sites; this plan
  performs the correction rather than deferring it.)
- **`DESIGN.md:242`** — `examples/` → `judge/testdata/calibration/`, with the reason (sibling
  repo, cannot gate CI).
- **`docs/what-the-numbers-mean.md:46`** — currently points at a nonexistent command. Gains: what
  κ claims, the attenuation interpretation, the 3×-cost price of the floor, the three verdicts,
  and the contamination caveat about a public set.
- **`docs/evaluation-design.md:44-45`** — "`judge calibrate` is the v0.2 tooling for this"
  becomes present tense with a link to the cookbook page.
- **New: `docs/cookbook/calibrate-a-judge.md`** — run it, read it, fix a prompt, re-record.
- **`docs/status.json`** via `make status` — whether a non-stage command counts as a tracked
  surface is ***(verify)***: `make status-check` runs inside `make docs` (`Makefile:451`) and
  will fail the PR if it does, which is the check rather than the guess.
- **`README.md`'s command table** (`:325` lists `kno purge`) gains a `kno judge calibrate` row.
- CLI help snapshots, CHANGELOG under `Unreleased`. No vhs tape: `tapes/quickstart.tape` runs
  baseline/value/select/export/report and no judge command, so the GIF is unaffected — checked,
  not assumed.

## Accepted risks

Each mirrored to `docs/debt.md` with a trigger.

1. **A public calibration set is contaminated for training the day it ships.** Any judge model
   released afterwards may have seen it. It is a regression instrument, not evidence of
   generalization. *Trigger: revisit if a held-back private set is ever needed to defend a
   published κ — at 1.0 at the latest.*
2. **The `κ ≈ 1 − 2ε` identity, on which the floor rests, assumes non-differential error.** The
   symmetry gate catches gross violations; a judge whose errors correlate with the *content* of
   an arm (it dislikes long outputs, and the treatment arm makes outputs longer) violates it in
   a way no per-class recall reveals. Stated in `what-the-numbers-mean.md` as a named limit.
   *Trigger: when the first judged Goal is used in a Value run.*
3. **Graded judges are ungated.** `UNIT_INTERVAL` Goals report and do not gate. *Trigger: when
   the first `SCORE_DOMAIN_UNIT_INTERVAL` Goal is registered.*
4. **`Goal.Score` still runs outside the budget reservation** — verified rather than assumed: it
   runs after `res.Settle` / `res.Release` in `core/invoke.go`'s `once`, so the reservation
   brackets the agent call and not `Score`, in both callers (`core/baseline_invoke.go:78`,
   `core/value_loop.go:803`), and `Estimator` requires exactly one call per `Invoke`. Contained
   by a **default-deny** registry: a Goal absent from the `goal.SelfContained` allowlist cannot
   register at all, whatever it does or does not declare about itself *(F1)*. *Trigger: before
   any judged Goal is added to `goal.Registry`* — mechanically the same edit as adding a name to
   the allowlist, where the trigger is written in the comment on the line being edited. This
   trigger cannot self-satisfy: this plan registers no judged Goal.
5. **Replay fixtures are a frozen judge.** A provider that changes model behavior under a stable
   name makes the replayed κ describe a model that no longer exists. The nightly live run is the
   detector; PR CI is deliberately blind to it. *Trigger: when the nightly live calibration and
   the replayed κ disagree by more than the paired CI.*
6. **The floor is one number for every judge.** A cheap triage judge and a final-scoring judge
   have different tolerances. `--min-kappa` exists for the user; the *committed* baseline uses
   the single floor. *Trigger: when a second judge Goal lands.*
7. **Two labelers is a thin ceiling.** Inter-human κ from two labelers is itself noisy, and the
   command gates on it. *Trigger: when the set exceeds 200 records, at which point a third
   labeler on a sampled subset is affordable.*
8. **The gate rests on a bootstrap this repo has never shipped** *(F2)*. Percentile coverage is
   measured on the decision grid rather than assumed (criterion 19), but the measurement is a
   synthetic-ground-truth simulation and the real record distribution is not simulated;
   under-coverage in the field shows up as a verdict that flips, not as a wrong-looking number.
   *Trigger: when the nightly live κ and the replayed κ disagree by more than the paired CI, or
   when the set exceeds 200 records — whichever comes first — at which point BCa is re-costed.*
9. **An out-of-tree Goal cannot register at all** *(F1)*, because the allowlist is compile-time.
   Accepted: Goals are in-tree today and the plugin boundary is Ring-2 adapters. *Trigger: when
   an out-of-tree Goal is requested, or when the `Goal.Score` budget seam (risk 4) is fixed —
   whichever comes first, since fixing the seam is what removes the allowlist's reason to
   exist.*

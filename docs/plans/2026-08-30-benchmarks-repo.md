# `uknoAI/kno-benchmarks`: published numbers that survive being checked

A sibling repository that measures the released `kno` binary — N Assets × M Cases → wall-time,
peak memory, dollars, throughput — on a stable machine, repeatedly, and publishes every result it
ever produced. So that the trust and comparison sections of the site cite a measurement with a
spread and a date instead of an illustration.

**Phase 0. Not implemented. No repository created, no hardware ordered.** Phase-1 adversarial
review is next; this plan is not approved until its objections are folded or accepted here and
mirrored to `docs/debt.md`.

**Phase-1 re-reviewed 2026-08-30 — verdict: amend; amendments applied.** Six findings survived
and are folded in and tagged inline. The two that change decisions rather than wording: the
dedicated box is no longer an un-owned line item — a named provisioner, a monthly dollar figure,
a named pager, and a decommission trigger that fires on something other than the hardware dying
are now **prerequisites to spending anything** *(F1)*; and Alternative B's hosted-runner
bootstrap is promoted from "recommended" to a **hard gate before any hardware is purchased**,
because the 30–50% variance figure that partly justifies the purchase is someone else's number,
not ours *(F4)*. The rest: `uknoAI/kno-www` **exists** and its trust section already publishes
illustrative numbers, so the website sections are live requirements with a named cross-repo
workstream rather than hedged futures *(F2)*; anti-cherry-picking is restated as GitHub
**Rulesets** with an empty bypass list plus signed commits, with the residual limitation written
plainly into `METHODOLOGY.md` *(F3)*; the live-provider cost figure gets an explicit `n` and
spread like every other number in the repository *(F5)*; and `docs/debt.md#3` gains a
benchmark-observed-regression trigger so a macro finding actually routes back to the micro gate
*(F6)*.

## Problem

**There are no performance measurements of Kno anywhere.** `grep -rn "func Benchmark"
--include='*_test.go' .` returns nothing across the entire repository. `make bench-diff` is not a
comparison — it is a tripwire that passes precisely because no benchmark exists and hard-fails
the moment one appears:

```make
bench-diff: ## Tripwire: fails once benchmarks exist, until the gate is implemented
	@$(SAFE) if grep -rql '^func Benchmark' --include='*_test.go' . 2>/dev/null; then \
		printf '\033[31m FAIL \033[0m benchmarks exist but bench-diff is unimplemented.\n'; \
```

`docs/debt.md#3` records this honestly, including its own lapse: the original trigger ("before
the first PR touching the scoring loop, routing, or NDJSON framing merges") fired with M1 and was
never disposed, and it was re-dated on 2026-08-29 to *"before 1.0, or the first PR that claims a
performance change to the scoring loop"*.

Meanwhile the project makes performance and cost claims in prose that nothing measures:

- `CLAUDE.md`: *"a 1M-case eval set must not load into RAM — `iter.Seq` is load-bearing"*. No
  memory measurement exists to confirm or refute this.
- `CLAUDE.md`: *"Profiles (`pprof`) checked into `docs/perf/` for the scoring loop as a
  baseline"*. `docs/perf/` does not exist.
- `DESIGN.md`'s cost model is explicitly a **worked example** — "50 assets, 400-case dev set, 60
  baseline failures in 6 clusters, 40 control cases, 3 trials" — hand-arithmetic, correctly
  labelled as an illustration. Nobody has run it.

And there is an epistemic asymmetry worth naming: `docs/what-the-numbers-mean.md` is 19KB of
discipline about numbers Kno reports about **your** data — what a confidence interval claims, why
the dev estimate is inflated, what a cost figure claims ("reported usage at rates as published on
`<date>`"), what Kno cannot tell you. Not one line of that discipline currently applies to numbers
we publish about **Kno**. A project that refuses to report a delta without its interval and then
publishes "10× faster" with no n and no spread has a credibility problem it built itself.

Why a separate repository: honest numbers need a stable machine and repeated runs. GitHub-hosted
runners are shared, noisy-neighbour, and vary by CPU model between runs (the repo uses only
`ubuntu-latest` and `macos-latest` today; there are no self-hosted runners anywhere in
`.github/`). Variance from that source is not measurement error you can characterize — it is a
different machine each time. You would either have to publish it without saying so, or say so and
publish nothing useful. Both are worse than a dedicated box in a repo that can safely host one.

## Design

### 0. The one structural decision everything else follows from

**`kno-benchmarks` contains no `func Benchmark`.** It is not a Go benchmark suite. It drives the
**released `kno` binary as a subprocess**, from the outside, measuring wall-clock, peak RSS,
dollars, and throughput of whole stage invocations.

Three consequences, all deliberate:

- It cannot trip `make bench-diff`, because that tripwire greps `*_test.go` in `uknoAI/kno`.
- It measures a different thing at a different altitude. `bench-diff` is a **micro** regression
  gate on hot paths inside the module, per-PR, comparing against `main`, with a >10% threshold.
  This is a **macro** longitudinal measurement of shipped artifacts, per-release, comparing
  against history, with no threshold at all — it reports, it does not gate.
- It therefore **does not repay `docs/debt.md#3`**. See §8, which is where this plan is most at
  risk of being misread.

### 1. The runner story

**One dedicated bare-metal box — ordered only after the bootstrap gate clears** (Alternatives, B,
promoted to a hard prerequisite by *(F4)*). A rented dedicated server with a named, fixed CPU model — not a
shared vCPU instance, where a neighbour's workload is indistinguishable from a regression. It is
registered as a **repository-scoped self-hosted runner on `kno-benchmarks` only**, labelled
`[self-hosted, kno-bench, <cpu-model>]`. `RUNNER.md` records CPU model, core count, RAM, kernel,
distro, disk type, and the machine's `machine-id`, and that file's history is the machine's
history.

**Before Phase 2 spends a dollar, the box has an owner, a price, a pager, and an exit** *(F1)*.
The first draft named the risk category — cost, operational surface, single point of failure —
and stopped there, which makes it a placeholder rather than a decision. Four fields, filled in as
an amendment to this plan **before** any provisioning happens:

| Field | Requirement |
|---|---|
| **Provisioner** | A named person who holds the hosting account and the runner registration token. Not "the maintainers". |
| **Monthly cost** | A dollar figure for the specific machine at the specific provider, plus who pays it and out of which budget. A plan that cannot state the number is not ready to ask for it. |
| **Pager** | A named person notified when the box drifts — a missed scheduled run, a `RUNNER.md` mismatch, a kernel or microcode update, or a CV that climbs across releases. Drift here is silent by construction: nobody notices a nightly that did not happen. |
| **Decommission trigger** | A condition that fires **without the hardware dying**. Committed as: two consecutive quarters in which no `kno-benchmarks` figure is cited by `kno-www` or the README, **or** the arrival of a hosted option whose measured spread (Alternative B's data, now mandatory) sits under the 5% CV threshold. Either one forces an explicit renew-or-stop decision — `docs/debt.md`'s lapsing-trigger discipline applied to a subscription. |

A box that nobody named, nobody budgeted, and nobody agreed on a condition for switching off is
how a research rig becomes a permanent unowned bill that outlives its own justification.

**Security is not optional here and is the reason most projects should not do this.** A
self-hosted runner executing untrusted code is a persistent-compromise vector.

- The measurement workflow triggers on `schedule`, `workflow_dispatch`, and
  `repository_dispatch` **only**. Never `pull_request`, never `pull_request_target`.
- PR CI runs on `ubuntu-latest` and does validation only: schema lint, summarizer unit tests, and
  a dry-run at N=2, M=2 that proves the harness works and measures nothing citable.
- No provider credential ever reaches the box (§4).
- Runner registration is repo-scoped, not org-scoped, so a compromise cannot pivot into
  `uknoAI/kno`.

**Variance is characterized, not hidden.** *A mean with no spread is a lie*, so no summary ever
emits a bare mean.

- Every configuration runs **k = 7** repetitions. Odd, so the median is a datum; small enough
  that a full sweep finishes overnight; large enough for a defensible IQR.
- Every published figure carries **median, p25, p75, min, max, n, and coefficient of variation**.
- A configuration whose CV exceeds a committed threshold (**5%** for `fake:` runs) is published
  **flagged `unstable`** — never dropped. Dropping a noisy cell is cherry-picking with extra
  steps.
- One **warm-up repetition** runs first and is excluded from the summary. It is still published,
  labelled `warmup`. The exclusion rule is committed in `METHODOLOGY.md` *before* any data exists
  (§3, rule 4).
- Machine state is recorded per repetition: kernel, CPU model, CPU governor, turbo/boost state,
  load average before and after, free memory, and the runner's `machine-id`. Ambient temperature
  and per-core frequency are recorded when the host exposes them ***(verify: availability depends
  on the hosting provider; a rented dedicated box may not expose IPMI sensors, and the plan must
  not promise a field it cannot fill)***.
- The box runs nothing else. No other repository, no other workflow.

### 2. What is measured with `fake:`, and what with real providers

**`fake:` carries the scaling curve.** It is the CLI's default agent, deterministic, local, and
free (verified). With `fake:` there is no network, no provider variance, and no money, so what is
left is exactly **engine orchestration cost**: routing, injection, scoring, checkpointing,
SQLite writes, event emission, iterator plumbing. That is the only thing Kno controls and
therefore the only thing Kno may make a throughput claim about. `fake:` runs every matrix point.

**Real providers carry cost realism only.** A monthly, small, capped run against one provider at
one N and one M, reporting **dollars**, never wall-time and never throughput. Rationale: provider
latency is theirs, varies by time of day and account tier, and is not a property of Kno — a
wall-clock number dominated by someone else's queue depth attributed to our engine would be the
single most dishonest thing this repository could publish. Cost figures reuse
`docs/what-the-numbers-mean.md`'s exact language: *reported usage at rates as published on
`<date>`*, carrying `pricing.Version`, an estimate and not an invoice.

**The live-cost figure carries an `n` and a spread, like every other number here** *(F5)*. The
first draft imposed "no number without n and spread" on the entire `fake:` matrix and then left
the monthly cost run's repetition count unspecified — leaving the one figure most likely to be
quoted in a commercial context as the only one with no stated `n`. That asymmetry is exactly what
§7 exists to forbid. Resolved explicitly rather than by omission:

- **n = 3**, reporting **median and range** (deliberately not IQR — at n=3 an interquartile range
  is theatre). The tempting shortcut — *"n=1 is acceptable for cost because token counts are
  deterministic per response"* — is **wrong, and saying why is the point.** Input tokens are
  deterministic: the prompts are fixed and the same Cases go in every run. **Output tokens are
  not.** Provider sampling is stochastic, response lengths vary run to run, and output is the
  side that dominates cost. A cost figure is therefore a draw from a distribution, and publishing
  n=1 would be publishing a point estimate for a random variable — precisely the thing forbidden
  everywhere else in this repository.
- The published figure **splits input and output cost**, so a reader can see which component is
  fixed and which carries the variance. A single total hides a deterministic half inside a
  stochastic one and tells the reader strictly less than the two numbers do.
- n=3 triples a deliberately small bill and sits inside `RUN_BUDGET_USD` **by construction**: the
  aggregate cap (§4, layer 2) is sized for three repetitions, not one, or layer 2 aborts every
  month and the cap becomes a number people raise reflexively.
- If some future cell genuinely cannot afford n=3, it is written to `results/` with `n: 1` and
  `spread: null` and is **excluded from `latest.json`** — it exists in the record and is not
  citable. There is no third state in which a lone observation is quoted as a figure.

### 3. The matrix, and why each axis is there

Committed as `matrix.yaml`. Every result records the hash of the matrix file that produced it; a
number with no matching matrix hash in history is not citable.

| Axis | Values | Why |
|---|---|---|
| Stage | `baseline`, `value`, `select`, `export` | The four stages that exist. **`validate` is excluded because `kno validate` does not exist** — there is no `cli/validate.go`, and `cli/root.go` says "validate arrives next". A benchmark of an unimplemented stage would be fiction. |
| N Assets (Pool size) | 10, 100, 1 000, 10 000 | The claim is a scaling *shape*. Log-spaced with four points is the minimum that distinguishes linear from superlinear with a fitted exponent and visible residuals. |
| M Cases (Evals size) | 50, 500, 5 000 | Same reason, one decade shorter — `value` cost is roughly N×M, so a full 10 000 × 5 000 cell is a different order of runtime and gets its own treatment. |
| Concurrency | 1, 8, 64 (`--concurrency`, verified; also `KNO_CONCURRENCY`) | This is the axis that separates orchestration overhead from wait time. At `fake:` there is no wait time, so it isolates lock contention, SQLite write serialization, and scheduler overhead. |
| Agent | `fake:` everywhere; one real provider in a separate monthly job | §2. |
| Memory probe | N=1, M=1 000 000, `baseline` only | `CLAUDE.md` says a 1M-Case eval set must not load into RAM. That is the most load-bearing self-claim the project makes, so it gets its own cell and its own metric: **peak RSS**, not time. |

**Deliberately excluded, and stated so an omission is not a lie:** goal type (one goal,
`exact-match`, the verified default); OS and architecture (linux/amd64 only for published numbers
— a second architecture needs a second dedicated machine, and the claim is about scaling, not
about your laptop); adapters other than the JSONL Evals and Pool sources (a vendor adapter's
latency is the vendor's).

**It is an OFAT sweep, not a full cross.** 4 stages × 4 N × 3 M × 3 concurrency × 7 reps = 1 008
runs, which is not a nightly. So: a **base cell** (N=100, M=500, concurrency=8) is measured for
every stage, and each axis is then varied one at a time from the base. Roughly 30 configurations
× 7 reps per stage. **Any published statement must therefore say "varying N at M=500,
concurrency=8", never "across the matrix"** — the latter would be false, and it is the kind of
false that nobody would ever catch.

### 4. Cost control: four independent layers, because one is a single point of failure

1. **Per-invocation, in-process.** Every `kno baseline` / `kno value` in a live run carries
   `--max-cost-usd` **and** `--max-calls` (both verified present on both commands). Two ceilings
   in different units: a wrong price in the table makes the dollar cap wrong while the call cap
   stays right. `CLAUDE.md`'s prime directive 4 says the guard enforces caps *before* the call,
   not at settlement — the benchmark relies on that property and, incidentally, exercises it.
2. **Per-workflow-run aggregate.** The harness sums reported spend from each invocation's
   `--json` document and **aborts the remaining matrix** the moment the running total crosses a
   committed `RUN_BUDGET_USD`. This catches the case that layer 1 structurally cannot: twenty
   individually-capped runs that together cost twenty times the cap.
3. **Provider-side hard limit.** The keys belong to a dedicated billing project whose provider-
   configured spend limit is the last line of defence — the only layer that survives a bug in
   layers 1 and 2. ***(verify: hard-cap semantics differ by provider — some enforce a hard org
   limit, others only notify. The plan must record, per provider, whether the limit is enforcing
   or advisory, and treat an advisory-only provider as having three layers, not four.)***
4. **Human approval.** Keys live in a GitHub **environment** (`bench-live`) with required
   reviewers — the pattern `release.yml` already establishes with `environment: release`. A
   scheduled run cannot reach them. The live job is `workflow_dispatch` plus a monthly schedule
   that requests approval and waits. Secrets are passed per-step, never as ambient job env, per
   `nightly.yml`'s precedent (`KNO_MAX_COST_USD: "5.00"` as a job-level ceiling with keys
   step-scoped).

**The live job runs on `ubuntu-latest`, not the dedicated box.** Its output is dollars, not
wall-time, so it does not need timing stability; and keeping provider credentials off a
long-lived self-hosted machine is worth far more than a timing property that number does not use.

### 5. Anti-cherry-picking: four mechanisms, not a promise

A published benchmark is only as trustworthy as its worst incentive. Four mechanisms, each of
which fails a build rather than relying on virtue:

1. **Append-only results.** The workflow commits every run — including failed, aborted, and
   `unstable`-flagged ones — to `results/<kno-version>/<date>-<run-id>/`. A CI check diffs the
   `results/` tree against the previous commit and **fails on any modification or deletion** of
   an existing file; only additions pass. Enforcement is a GitHub **Ruleset** — not classic
   branch protection — with an **empty bypass list**, required status checks, and **required
   signed commits** *(F3)*. The distinction is the whole finding: classic branch protection is a
   setting a repository admin edits at will, so "we do not force-push" would be a promise made by
   exactly the person most motivated to break it. A ruleset with no bypass actors applies to
   admins too, and changes to it land in the organization audit log — which moves the evasion
   from *invisible* to *visible*.

   **And it is still not a cryptographic guarantee, which `METHODOLOGY.md` must say in those
   words** *(F3)*. An admin can loosen a ruleset; the audit log records that they did, but the
   record is a deterrent, not a lock. Signed commits mean a rewritten history cannot be silently
   re-attributed, and an append-only tree means every result ever pushed exists in every clone
   and every fork. So the honest statement — committed to `METHODOLOGY.md`, not left implied — is
   this: *append-only here is a norm the community can audit from git history and the ruleset
   audit log; it is not a technical impossibility. If you do not trust us, clone the repository
   and diff it yourself. That is the actual guarantee on offer, and it is the only kind any
   self-published benchmark can honestly make.* A benchmark repository that overstated its own
   tamper resistance would have committed this project's founding sin in its own README.
2. **The matrix is committed before the run.** The workflow executes `matrix.yaml` as committed
   and records its hash. You cannot decide after seeing the data which configurations you meant
   to run.
3. **The summary is derived, never written.** `SUMMARY.md` and the site-consumable
   `results/latest.json` are generated from the full result set; CI regenerates and diffs, so a
   hand-edited summary fails the build. This is `make generate-check`'s shape, applied to
   numbers.
4. **No exclusion without a rule that predates the data.** Exclusion rules (the warm-up rule, the
   `unstable` CV threshold, any future "runs during a kernel upgrade window") live in
   `METHODOLOGY.md`, and the summarizer **refuses an exclusion whose rule's commit timestamp is
   later than the excluded result's**. That is the actual teeth: you may exclude on a principle,
   never on an outcome.

`METHODOLOGY.md` is itself a committed artifact and its diff history is part of the published
record: fixtures, generator scripts, the harness, the run logs as structured data.

### 6. Versioning against kno releases

- Results are keyed by exact release tag: `results/v0.1.1/...`.
- Each result records `kno --version` output **and** the SHA-256 of the release archive it was
  extracted from, cross-checked against the published `checksums.txt`. A result whose digest does
  not match a published artifact is not citable — that is what makes "we measured the thing users
  download" a checkable statement rather than an assertion.
- The archive comes from the verified path: `install.sh`'s URL scheme
  (`.../releases/download/<tag>/kno_<tag-without-v>_<os>_<arch>.tar.gz`, verified against
  `.goreleaser.yaml`'s `name_template`), with `cosign` installed so signature verification
  actually happens rather than being silently skipped.
- **Triggering:** ideally a `repository_dispatch` from `uknoAI/kno`'s release workflow. There is
  **no `repository_dispatch` anywhere in `.github/` today**, and `release.yml` is the most
  safety-critical workflow in the repo — it carries `id-token: write`, `contents: write`, cosign
  keyless signing, and SLSA attestation. **Decision: do not touch it.** `kno-benchmarks` polls
  the GitHub releases API on a schedule and starts a run when it sees an unmeasured tag. Slower by
  hours, zero risk to the release pipeline. A dispatch can be added later if the latency ever
  matters, which it does not.
- **Longitudinal:** a per-version index plus `results/latest.json`. The site cites a
  **version-pinned** number, never "latest", so a published page's claim cannot silently change
  underneath it.

### 7. What the site may claim, and what it may not

`docs/what-the-numbers-mean.md` applied to our own numbers. `CITATION.md` states these rules; the
generated `latest.json` carries, per figure, a `claim` string containing the full qualification,
and the consuming site is required to render the qualification adjacent to the number.

**May claim:**

- "Valuing 1 000 Assets over 500 Cases with `fake:` at concurrency 8 took a median of X s (IQR
  [a, b], n=7) on <named CPU>, kno v0.1.1." — every component present: what, at what settings, on
  what machine, with what spread, at what version.
- "Peak RSS stayed under X MB while streaming 1 000 000 Cases through `baseline`." — the memory
  claim, measured.
- "Orchestration overhead per measurement was X ms at the base cell." — because `fake:` has no
  provider time in it.
- "A valuation of N Assets over M Cases cost $X at rates published on `<date>`." — with
  `pricing.Version`, as an estimate, exactly as the CLI reports it.
- A fitted scaling exponent **with its residuals shown**.

**May not claim:**

- **Any comparison to another tool.** We do not run their code on our box, and a competitor
  benchmark written by us is marketing. Non-negotiable.
- "Fast", "faster", "blazing", "scales linearly" — without the fitted exponent and its residuals,
  these are adjectives, not measurements.
- Any figure from an `unstable`-flagged cell without the flag rendered next to it.
- Any number without n and a spread. **A mean with no spread does not ship.**
- Extrapolation past the measured range. N=10 000 was measured; N=100 000 was not, and the curve
  is not an invitation.
- **A `fake:` number presented as end-to-end time.** `fake:` excludes provider latency, which
  dominates every real run. This is the single most likely way this repository becomes dishonest —
  not by fabricating a number, but by quoting a true one in a context that implies something
  false. It gets its own named rule, and every `fake:` figure's `claim` string contains the words
  "excludes provider latency".
- Any dollar figure without the pricing-table date.

**The consumer exists, and it is already publishing illustrative numbers** *(F2)*.
`uknoAI/kno-www` is live, and its trust and comparison section publishes figures that are
**illustrations, not measurements** — the exact situation this repository was conceived to end.
§7 is therefore not a contract offered to a hypothetical future party; it is a set of rules with
a live counterparty and a live violation already on the page. And `CITATION.md` "binding"
`kno-www` is meaningless until an actual PR against `kno-www` exists. So:

**Cross-repo workstream, named and owned:** *"Replace `kno-www`'s illustrative trust numbers with
cited ones."* Owner: named in the PR that creates `kno-benchmarks` — unowned means unstarted, and
this is the workstream most likely to be assumed rather than assigned. Two phases, and the first
waits on no measurement whatsoever:

1. **Interim, immediately, before `kno-benchmarks` produces a single number:** every illustrative
   figure on `kno-www` is explicitly labelled as illustrative *on the page*. This is a small
   `kno-www` PR and it is the only part of this plan that improves honesty on day zero. An
   unlabelled illustration is indistinguishable from a measurement to every reader — the same
   category of error as a mean with no spread, committed about our own product.
2. **On first published results:** each labelled figure is replaced by a version-pinned citation
   carrying its `claim` string, rendered adjacent to the number per §7 — or deleted outright if
   no measured figure supports it. "No number" is an acceptable outcome here; "an illustration
   where a reader expects a measurement" is not.

`kno-www` is consequently in this plan's affected-repos list, and the renderer that must place
the qualification next to the number is a real component in a real repository rather than a
requirement addressed to nobody.

### 8. Relationship to `make bench-diff`, and the `docs/debt.md#3` disposition

**This repository does not replace, duplicate, or repay the tripwire, and the plan must be read
that way or not at all.**

| | `make bench-diff` (in `uknoAI/kno`) | `kno-benchmarks` |
|---|---|---|
| Unit | Go micro-benchmarks on hot paths | Whole released-binary stage invocations |
| Cadence | Every PR | Every release, plus scheduled |
| Compares against | `main` | Its own history |
| Machine | GitHub-hosted (fine — it is a *relative* comparison) | Dedicated box (necessary — it is an *absolute* number) |
| On failure | **Blocks the merge** | Files a report. Blocks nothing. |
| Purpose | Regression tripwire | Longitudinal truth |

**Ledger disposition for `docs/debt.md#3`: it stays OPEN and stays ARMED, re-dated with a written
reason naming this plan.** The tempting reading — "we have benchmarks now, so #3 is repaid" — is
exactly the silent-carryover failure the ledger exists to prevent, and it is the single most
likely misreading of this document. Concretely, after `kno-benchmarks` exists:

- Adding any `_bench_test.go` to `uknoAI/kno` still hard-fails `make bench-diff` and still
  demands the >10% gate be implemented. Nothing here changes that by one line.
- Entry #3's current trigger — *"before 1.0, or the first PR that claims a performance change to
  the scoring loop"* — is untouched by this plan and must not be re-dated *because of* this plan.
- What this plan does give #3 is a **repayment aid**: a known-good workload, a `pprof` baseline
  for `docs/perf/` (which does not exist today), and evidence about which paths actually dominate
  — so whoever repays #3 picks micro-benchmarks from measurement rather than from intuition.

**One amendment to #3 is required after all — an addition, not a re-dating** *(F6)*. The
reasoning above stands: macro subprocess measurement is a different instrument at a different
altitude from an in-repo micro gate, so this plan does not repay #3. But that leaves a hole. If
`kno-benchmarks` observes a release-over-release regression, **nothing routes that finding back
to #3**, and the ledger entry that exists precisely because "a regression can ship unnoticed"
would sit untouched while a regression demonstrably shipped. So #3 gains a third trigger
alongside its existing two: **"a `kno-benchmarks`-observed regression against the prior
release."** Mechanically: when the summarizer finds a configuration whose median moved
unfavourably against the previous release by more than the published IQR of both, it files an
issue on `uknoAI/kno` naming `docs/debt.md#3` and citing the configuration — the macro instrument
telling the micro gate exactly where to point its first `_bench_test.go`. This neither re-dates
#3 nor weakens it; it adds a condition that can actually fire, which is what the ledger rules
demand of every trigger.

**One new ledger entry is required**, at the next free number (**130** is the highest in use
today): *"Published performance numbers exist in `kno-benchmarks` while `uknoAI/kno` still has no
in-repo regression gate; a regression can ship and only be noticed after release."* **Trigger:**
when `docs/debt.md#3` is repaid, cross-link the two and state which failures each catches — or
before 1.0, whichever is first.

## Acceptance criteria

Numbered, each naming an observable.

1. `grep -rn 'func Benchmark' kno-benchmarks` returns nothing, and `make bench-diff` in
   `uknoAI/kno` still reports `PEND` and still exits 0 after this plan lands. The tripwire is
   provably untouched.
2. `docs/debt.md#3`'s **status, scope, and existing triggers are unchanged** by the PR that
   creates `kno-benchmarks` — it stays open, stays armed, and is not re-dated because of this
   plan. The **only** permitted edit to #3 is the *addition* of one trigger, "a
   `kno-benchmarks`-observed regression against the prior release" *(F6)*. The new ledger entry
   (§8) exists with a trigger that can lapse. Verified by reading the diff.
3. `RUNNER.md` names the CPU model, core count, RAM, kernel, and `machine-id` of the measuring
   box, and every published result carries a `machine-id` that matches one recorded there. A
   result with an unrecorded `machine-id` fails the summarizer.
4. No published figure anywhere in `SUMMARY.md` or `results/latest.json` lacks `n`, `median`,
   `p25`, `p75`, and `cv`. A schema check fails the build if any is missing.
5. A configuration with CV > 5% appears in the output **flagged `unstable`**, not omitted.
   Demonstrated with a synthetic result set whose CV is 12%.
6. Deleting or modifying any file under `results/` in a PR fails CI. Demonstrated by a test
   commit that removes one result file.
7. A hand-edited `SUMMARY.md` fails CI, because the regenerate-and-diff check disagrees.
8. The summarizer refuses an exclusion whose rule was committed after the excluded result.
   Demonstrated with a back-dated fixture pair.
9. Every live-provider invocation in the harness carries both `--max-cost-usd` and `--max-calls`;
   a lint over the harness fails if either is absent from any invocation that names a non-`fake:`
   agent.
10. The aggregate cap aborts the run: with a mocked `--json` spend document exceeding
    `RUN_BUDGET_USD` on the third of ten configurations, configurations four through ten do not
    execute. No network involved.
11. No workflow that can reach a provider key runs on `schedule` without an environment approval;
    `grep -L 'environment:' ` over every workflow referencing a provider secret returns nothing.
12. No workflow using `runs-on: [self-hosted, ...]` is triggerable by `pull_request` or
    `pull_request_target`. Asserted by a workflow-lint test, not by inspection.
13. Every result records the SHA-256 of the release archive it measured, and that digest appears
    in the corresponding release's published `checksums.txt`. A mismatch fails the run.
14. The 1M-Case memory probe completes and reports peak RSS. If it exceeds a committed ceiling,
    the run **still publishes** and files an issue on `uknoAI/kno` — a refuted self-claim is the
    most valuable result this repository can produce and must not be suppressible.
15. `CITATION.md` exists and every figure in `results/latest.json` carries a non-empty `claim`
    string; every `fake:` figure's `claim` contains the substring "excludes provider latency".
16. `kno-benchmarks` is not a required status check on any `uknoAI/kno` branch protection rule.
17. The hosted-runner bootstrap has run for at least four weeks and its results are committed
    under `results/bootstrap/` **before** any provisioning request is made, and the provisioning
    request cites the measured CV **by number**. A hardware order with no committed bootstrap
    data is rejected on process *(F4)*.
18. `RUNNER.md` names the provisioner, the monthly cost, the pager, and the decommission trigger;
    a lint fails if any of the four is absent or reads "TBD" *(F1)*.
19. `results/` is protected by a GitHub Ruleset with an **empty bypass list** and required signed
    commits — evidenced by the exported ruleset JSON committed to the repository and diffed by CI
    against the live API — and `METHODOLOGY.md` contains, in plain language, the statement that
    append-only is an auditable norm rather than a technical impossibility *(F3)*.
20. Every live-cost figure in `results/latest.json` carries `n >= 3`, a median, a range, and a
    split of input and output cost. A figure with `n: 1` appears in `results/` and is absent from
    `latest.json`. Asserted by a schema check over a synthetic n=1 fixture *(F5)*.
21. `docs/debt.md#3` carries the additional trigger "a `kno-benchmarks`-observed regression
    against the prior release", and the summarizer, on detecting a release-over-release
    regression outside the published IQR of both releases, files an issue on `uknoAI/kno` that
    names entry #3. Demonstrated with a synthetic two-release result set, no network *(F6)*.

## Alternatives considered

**A. Keep benchmarks in `uknoAI/kno` — repay `docs/debt.md#3`, add `_bench_test.go` files, and
publish those numbers.** The honest case: it is what `CLAUDE.md` already prescribes ("Benchmarks
live next to hot code (`_bench_test.go`); `make bench-diff` gates regressions"); it repays real
debt instead of routing around it; benchmarks next to the code they measure rot far less; there
is no second repo, no hardware, no self-hosted runner and therefore no new attack surface, no
cross-repo versioning, and no operational cost at all.

Rejected as a substitute, endorsed as a **prerequisite that this plan neither performs nor
blocks**. Go micro-benchmarks on shared GitHub runners are legitimate for *relative* comparison
against `main` — that is exactly what the >10% gate needs, and the noise largely cancels because
both sides run on the same class of machine in the same job. They are not legitimate as
*published absolute* numbers: "1 000 Assets valued in X seconds" on an unknown shared vCPU is a
number about a runner, not about Kno. And micro-benchmarks cannot measure what the site actually
needs — dollars, end-to-end wall-time of a real stage, peak RSS streaming 1M Cases, or the
scaling shape across four orders of magnitude of N. Different question, different instrument.
Both should exist; neither replaces the other; §8 keeps them from being confused.

**B. Publish numbers from GitHub-hosted runners and report the variance honestly.** Genuinely
tempting: zero hardware, zero operational cost, no self-hosted-runner security posture to get
right, and it starts this week. Rejected because the dominant variance component is *which
physical CPU you landed on*, which is not measurement noise you can characterize with more
repetitions — repeating on a different machine each time measures the fleet, not the software. A
30–50% spread ***(verify: the exact spread on GitHub's fleet for this workload is unmeasured; the
figure is from general reports of shared-runner variance, not from our own data)*** would make
every scaling claim unfalsifiable, and publishing a wide interval is only honest if the interval
means something. The interval would mean "GitHub's fleet is heterogeneous", which is true and
useless. ***Promoted from "recommended bootstrap" to hard prerequisite gate*** *(F4)*: run exactly this
harness on `ubuntu-latest` for **at least four weeks**, publish nothing, and use the observed
spread to decide whether the box is warranted. **No hardware is ordered and no hosting contract
is signed until that data exists and is committed to `results/bootstrap/`.** The reason is the
hedge two sentences up: the 30–50% figure is load-bearing — it is a substantial part of the
argument for buying a machine — and it comes from general reports of shared-runner variance, not
from one measurement of *this* workload on *this* fleet. Requesting capital to fix a problem
sized by someone else's blog post is exactly the epistemic failure `docs/what-the-numbers-mean.md`
forbids everywhere else in this project, committed about our own spending. The gate has a stated
exit in **both** directions: if the measured per-configuration CV on `ubuntu-latest` is at or
under the **5%** threshold for the base cell and the N-sweep, the dedicated box is **not**
purchased and this plan ships on hosted runners with the spread published; if it exceeds the
threshold, the bootstrap data is itself the justification, cited by number, in the provisioning
request (§1). The bootstrap harness is not throwaway — it is the same code, and its results are
the first entries in the append-only tree.

**C. A one-off benchmark blog post: run it once by hand, publish, move on.** Rejected: this is
the failure mode the whole repository exists to prevent. A number with no history cannot be
checked, cannot be reproduced, silently becomes a claim about a version nobody runs, and creates
exactly the incentive to re-run until the number is pretty. Every anti-cherry-picking mechanism in
§5 is meaningless without a committed history to be append-only *to*.

**D. Fold benchmarks into `kno-examples` (one sibling repo, not two).** Rejected: opposite
requirements at every axis. `kno-examples` wants cheap hosted runners, fork PRs from external
contributors, and per-PR execution; `kno-benchmarks` wants a dedicated machine that must never
execute fork-contributed code. Combining them means either the examples repo inherits a
self-hosted runner it should not have, or the benchmark repo inherits a fork-PR surface it cannot
tolerate. The security boundary is the reason, and it is not negotiable.

## Affected repos and packages

**New: `uknoAI/kno-benchmarks`** — `harness/` (subprocess driver, JSON result writer, budget
accounting), `matrix.yaml`, `METHODOLOGY.md`, `RUNNER.md`, `CITATION.md`, `results/` (append-only),
`SUMMARY.md` (generated), `.github/workflows/{pr,measure,live-cost}.yml`, `LICENSE`
(Apache-2.0), `CONTRIBUTING.md` (DCO, `git commit -s`).

**`uknoAI/kno`** — `docs/debt.md` (one new entry; **#3 untouched**), `README.md` ("Why trust the
result?" may gain a link once results exist — not before), `CHANGELOG.md` under `Unreleased`,
and eventually `docs/perf/` when a `pprof` baseline is produced. `docs/debt.md#3` receives one
**added** trigger and nothing else *(F6)*. **No Go package, no Makefile target, no workflow, and
specifically not `release.yml`, is modified.**

**`uknoAI/kno-www`** *(F2)* — **live today**, and absent from the first draft's list. Its trust
and comparison section currently publishes illustrative numbers. Two PRs, per §7: an immediate
one labelling those figures as illustrative, and a later one replacing each with a version-pinned
citation rendering its `claim` string adjacent to the number. Owner named in the PR that creates
`kno-benchmarks`.

## Proto / schema impact

**None.** `kno-benchmarks` defines no message, imports no generated package, and touches no
`.proto` file. It consumes `--json` documents and exit codes as an external observer — the same
posture as `kno-examples`, for the same reason.

Two things worth stating precisely rather than waving at:

- The harness parses `--json` spend and timing fields, which makes it a **consumer** of that
  document's shape. `CLAUDE.md` already makes `--json`/exit codes covenants post-1.0; this repo
  becomes something that would notice a break. It does not create a new promise.
- `results/*.json` and `results/latest.json` are this repository's **own** schema, versioned with
  a `schema_version` field and a compatibility rule of its own, because the site will consume it.
  Breaking it is a `kno-benchmarks` concern and never a `buf breaking` concern.

## Edge cases

| Case | Behavior |
|---|---|
| CI without keys | Every `fake:` measurement runs (that is the whole scaling matrix). The live-cost job is unreachable without environment approval and does not silently no-op — it is simply not scheduled. |
| Released-binary drift (a release changes `--json` field names) | The harness fails to parse, aborts before measuring, publishes **no** number, and files an issue. A partial parse must never produce a number that looks complete. |
| Release yanked or re-tagged after measurement | The recorded archive digest no longer matches `checksums.txt`; the summarizer marks the result `superseded` and excludes it from citation — under a rule committed in advance. The result itself is never deleted. |
| A benchmark run fails midway | Completed configurations are published with `partial: true` and an explicit list of what did not run. The summarizer will not compute a scaling fit from a partial sweep; it says so. Silent partial data is how a curve gets a fake shape. |
| A single repetition fails (OOM, disk full) | Published with its error. If fewer than 5 of 7 repetitions succeed, the configuration is `insufficient-n` and produces no summary figure. Threshold committed in `METHODOLOGY.md` before any data. |
| Cost overrun | Four independent layers (§4). If layer 2 fires, the run aborts, publishes what completed with `budget-aborted: true`, and files an issue naming the configuration that tripped it. |
| Provider outage during the live run | The run fails; no cost figure is published for that month; the previous month's figure keeps its own date and is not re-dated. A gap in the series is data. |
| Contributor PR | Runs on `ubuntu-latest` only, never the box, never with secrets. A PR touching `results/` fails the append-only check. A PR touching `METHODOLOGY.md`'s exclusion rules is reviewable code and changes nothing retroactively (rule 4). |
| Stale results (kno released, no measurement yet) | The version index shows the release as `unmeasured`. `latest.json` continues to serve the last measured version with its own version stamp — it never advances to an unmeasured release. Staleness beyond 14 days files an issue. |
| Machine replacement or hardware failure | The new box gets a **new `machine-id` and starts a new series**. Old results are never re-labelled or rescaled. Where possible both machines run the same matrix for one release, publishing a **cross-calibration factor** that is reported and **never silently applied** to historical numbers. If the old box dies without overlap, the series simply ends and the discontinuity is documented — a visible break beats an invented bridge. |
| Kernel/firmware update on the box (mitigation changes CPU performance) | `RUNNER.md` records it with a date; results before and after are in the same series but carry a `host-config-epoch` the summarizer surfaces on any plot spanning the boundary. |
| Someone wants a number that has not been measured | There is no mechanism to produce one outside the committed matrix. Add the configuration to `matrix.yaml` in a reviewed PR and wait for the next run. That latency is the feature. |
| A release regresses against the prior one *(F6)* | Both are published; the regression is flagged; the summarizer files an issue on `uknoAI/kno` naming `docs/debt.md#3` and the configuration. It still blocks nothing — it routes the finding to the gate that should catch it next time. |
| The bootstrap measures hosted-runner CV at or under 5% *(F4)* | No box is bought. The plan ships on `ubuntu-latest` with the measured spread published, and §1's hardware sections become unnecessary rather than wrong. The gate is designed to be allowed to say no. |
| A repository admin loosens the ruleset *(F3)* | The organization audit log records it, and the committed ruleset JSON's CI diff surfaces it (AC 19). Detection, not prevention — and `METHODOLOGY.md` says exactly that, in those words. |
| The live-cost cell cannot afford n=3 *(F5)* | It is written to `results/` with `n: 1` and `spread: null` and excluded from `latest.json`. Recorded, not citable. |
| The 1M-Case memory probe refutes `CLAUDE.md`'s streaming claim | Published, issue filed on `uknoAI/kno`, `CLAUDE.md`'s claim amended. AC 14 makes suppression impossible. |

## Test plan — what verifies the verifier

The harness and summarizer are the only new code and the only things that can lie quietly.

- **Dry-run mode** at N=2, M=2, `fake:`, on `ubuntu-latest`, in PR CI: proves the harness executes
  and writes a schema-valid result, measures nothing citable, and is marked `dry-run: true` so it
  can never be summarized.
- **Statistics unit tests**: a synthetic result set with hand-computed median, p25, p75, CV; the
  summarizer must reproduce every figure exactly. An off-by-one in a percentile silently changes
  every published interval.
- **Append-only check tested against a violating commit** (deletes one result) and a
  modifying commit (edits one byte). Both must fail. A check nobody has watched fail is not a
  check.
- **Exclusion-rule test**: a result timestamped before its exclusion rule's commit must be
  refused as an exclusion.
- **Budget test**, no network: mocked `--json` spend documents drive the aggregate accumulator
  past `RUN_BUDGET_USD`; assert remaining configurations do not execute and `budget-aborted` is
  recorded.
- **Machine-fingerprint test**: a result carrying an unrecorded `machine-id` must be refused, and
  results from two different `machine-id`s must not be merged into one series.
- **Claim-string lint**: every figure in generated output has a non-empty `claim`; every `fake:`
  figure's claim contains "excludes provider latency". This is the rule most likely to be
  forgotten, so it is mechanical.
- **Workflow lint**: no `self-hosted` job reachable from `pull_request`/`pull_request_target`; no
  provider-secret job without an `environment:`.
- **Reproducibility**: the same released binary measured twice on the same box in different weeks
  must agree within the published IQR. Persistent disagreement means the machine has drifted and
  `RUNNER.md` is wrong — which is itself the finding.

## Rollback

Fully additive to `uknoAI/kno`: one ledger entry and a CHANGELOG line, both reverted with a
markdown diff. `docs/debt.md#3` and `make bench-diff` are untouched by construction, so there is
nothing to restore there. `kno-benchmarks` is archived read-only; because results are append-only
and version-pinned, archived data remains citable with its existing qualifications rather than
becoming a dangling claim. The dedicated box is decommissioned; the only irreversible act is
having published numbers, and those stay true of the version and machine they name. Any site page
citing them must either keep the version-pinned citation or remove the claim — which is why the
citation format pins a version in the first place.

## Docs impact

`uknoAI/kno`: `docs/debt.md` (one new entry at the next free number — 130 is the highest today —
with a lapsing trigger; and on **#3**, exactly one *added* trigger, *"a
`kno-benchmarks`-observed regression against the prior release"* *(F6)*, alongside an explicit
note that this plan does **not** repay #3 and does **not** re-date it);
`CHANGELOG.md` under `Unreleased`; `README.md`'s "Why trust the result?" section gains a citation
only once measured numbers exist, never before. `docs/perf/` is created when a `pprof` baseline is
produced, which is `docs/debt.md#3`'s work, not this plan's.

`kno-benchmarks`: `README.md` (what is measured and what is not, in that order), `METHODOLOGY.md`
(the committed rules, including every exclusion rule, dated), `RUNNER.md` (the machine and its
history), `CITATION.md` (the may/may-not list from §7, written in the register of
`docs/what-the-numbers-mean.md`), and `SUMMARY.md` (generated, never hand-edited).

## Accepted risks

- **A dedicated box is ongoing cost and ongoing operational surface**: patching, a self-hosted
  runner agent, and a single point of failure for the whole series. Accepted **only once** it has
  a named provisioner, a stated monthly figure, a named pager, and a decommission trigger that can
  fire without the hardware dying (§1) *(F1)*, and **only after** the hosted-runner bootstrap gate
  produces our own variance data (Alternative B) *(F4)*. Until both are done this is not an
  accepted risk — it is an unfunded intention wearing a risk's clothes.
- **One machine means one architecture and one operating system.** Every published number is
  linux/amd64 on one named CPU. Readers on Apple silicon get no number. Stated on every page
  rather than generalized away.
- **OFAT is not a full matrix**, so interaction effects between N, M, and concurrency are
  unmeasured. Stated in `METHODOLOGY.md`; a claim about the interior of the space is one of the
  things §7 forbids.
- **`fake:` overstates real-world throughput by construction.** It is the honest way to isolate
  orchestration cost and the easiest number in the repository to quote dishonestly. Mitigated by
  a mechanical claim-string rule rather than by editorial care, because editorial care is exactly
  what fails under pressure to publish.
- **The cost figures depend on a hand-entered pricing table** that can be up to 90 days stale
  before the drift detector fails (`pricing-check.yml`, weekly, with `docs/debt.md#40`'s 90-day
  trigger). Every dollar figure therefore carries `pricing.Version`, and the same caveat
  `docs/what-the-numbers-mean.md` already applies to user-facing costs applies here unchanged.
- **`kno-www` exists, publishes illustrative numbers today, and is not governed by this plan**
  *(F2)*. `CITATION.md` binds it only to the extent that §7's workstream actually lands and its
  reviewers keep the discipline; a future page author who never reads `CITATION.md` can still
  render a number without its qualification. Mitigated by carrying the qualification *inside*
  `latest.json` as a required non-empty `claim` field, so a consumer must actively discard it
  rather than merely forget it. Accepted residual: no schema can force a renderer to display a
  string.
- **A dedicated self-hosted runner is a genuine compromise vector.** The mitigations (repo-scoped
  registration, no fork-PR triggers, no ambient credentials) are the standard ones and they are
  necessary but not sufficient — a compromise of the box is a compromise of the published numbers.
  Accepted with the mitigations named and asserted by AC 11–12 rather than by intention.

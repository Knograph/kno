# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/). Entries are derived from
[Conventional Commits](https://www.conventionalcommits.org/) by release-please.

Version policy: releases stay in the `0.x` series until 1.0 is a deliberate decision. `feat:`
bumps the patch and a breaking change bumps the minor, so reaching 1.0 requires someone to choose
it rather than a tool defaulting into it. **This is the recorded decision, not a default** —
re-affirmed 2026-08-29 at the v0.1 release (docs/debt.md#19): switching to feat-bumps-minor
would let the version number climb toward 1.0 automatically, which is exactly what the policy
exists to prevent.

**Pre-1.0 compatibility:** minor bumps may break, with notice here and migration notes. After 1.0,
the proto schema, the plugin protocol, exit codes, the `kno.yaml` schema, and the public Go API are
covenants — breaking any of them requires a major version.

<!-- Two sections per release, and they are not redundant.
     release-please writes the one above this comment from Conventional Commit
     subjects: one line per PR, mechanically derived, and it is what the GitHub
     release page shows. The section below is written by hand and is where the
     reasoning lives — what broke, what it cost to find, and why a fix is
     shaped the way it is. CLAUDE.md requires both, and neither substitutes for
     the other: a commit subject cannot carry a paragraph, and a hand-written
     file cannot be trusted to be complete.
     At release time the hand-written heading is renamed from [Unreleased] to
     the version. See docs/debt.md#76 for why that is still a manual step. -->

## [0.1.6](https://github.com/uknoAI/kno/compare/v0.1.5...v0.1.6) (2026-09-01)


### Bug Fixes

* **export:** a tuning set carries an assistant turn to train on ([#183](https://github.com/uknoAI/kno/issues/183)) ([5cf4ab2](https://github.com/uknoAI/kno/commit/5cf4ab23e203515624a36554fa7ae291c75c22af))
* **stats:** a sample with no spread no longer reports certainty ([#182](https://github.com/uknoAI/kno/issues/182)) ([9e88389](https://github.com/uknoAI/kno/commit/9e88389f36fdbee6e2fff048b11b542d3b706221))


### Documentation

* fold the hand-written changelog into v0.1.5 ([#180](https://github.com/uknoAI/kno/issues/180)) ([76fb335](https://github.com/uknoAI/kno/commit/76fb33589109d5be0a5cc2de2b623f032c2cf091))

## [0.1.5](https://github.com/uknoAI/kno/compare/v0.1.4...v0.1.5) (2026-09-01)


### Features

* judge calibrate — a judge is measured before it is trusted ([#177](https://github.com/uknoAI/kno/issues/177)) ([bd2cc17](https://github.com/uknoAI/kno/commit/bd2cc17520e6b939f6493ed65d10c904002ed2a1))


### Bug Fixes

* **release:** fold the changelog correctly, and stop it racing the tag ([#174](https://github.com/uknoAI/kno/issues/174)) ([9112255](https://github.com/uknoAI/kno/commit/91122551c46ba074298fc974202de6fe6822e951))
* **validate:** a resumed run restores the tokens it spent ([#172](https://github.com/uknoAI/kno/issues/172)) ([14291c8](https://github.com/uknoAI/kno/commit/14291c82fe5ea979effea37f65aeb1a9499da5b9))


### Documentation

* **plans:** amend redundancy detection with F5 and the moved cookbook ([#173](https://github.com/uknoAI/kno/issues/173)) ([159d6e1](https://github.com/uknoAI/kno/commit/159d6e1ef7a9e53654d892a860b26032b4ad5702))


### Build & Dependencies

* **release:** notify kno-www when a release ships ([#176](https://github.com/uknoAI/kno/issues/176)) ([01ebc01](https://github.com/uknoAI/kno/commit/01ebc012fea86e25a6ef23627d2ac5edf8a677ee))

## [0.1.4](https://github.com/uknoAI/kno/compare/v0.1.3...v0.1.4) (2026-09-01)


### Features

* every stage reports what it spent, or says it could not ([#168](https://github.com/uknoAI/kno/issues/168)) ([dd5f0e9](https://github.com/uknoAI/kno/commit/dd5f0e9d600fa7cc2206ec10536069555ee41d78))
* kno validate — the holdout finally speaks ([#169](https://github.com/uknoAI/kno/issues/169)) ([e13b558](https://github.com/uknoAI/kno/commit/e13b55849e2e1821b716565ad7d5738d52c7b07a))


### Bug Fixes

* **build:** the ledger gate refuses duplicate entry ids ([#166](https://github.com/uknoAI/kno/issues/166)) ([7055062](https://github.com/uknoAI/kno/commit/70550628f4749a29183bbf7e337524ef81de7346))
* **value:** a resumed run restores the tokens it spent ([#170](https://github.com/uknoAI/kno/issues/170)) ([00f0044](https://github.com/uknoAI/kno/commit/00f0044ea8c34628ad650df2425325d745f6aadf))


### Documentation

* **debt:** dispose four silent lapses and backstop seven vague triggers ([#167](https://github.com/uknoAI/kno/issues/167)) ([706b186](https://github.com/uknoAI/kno/commit/706b186486edc5e3edf268e694fedb8b0de88054))
* fold the hand-written changelog into v0.1.3 ([#164](https://github.com/uknoAI/kno/issues/164)) ([788c09f](https://github.com/uknoAI/kno/commit/788c09faffcaf314fe8288447ff1dfa0fd08a8d6))
* move the cookbook to uknoAI/kno-examples, leaving tombstones ([#163](https://github.com/uknoAI/kno/issues/163)) ([94f32df](https://github.com/uknoAI/kno/commit/94f32dfc5f0bd6d9e49570f02278375d249cb5e4))

## [Unreleased]
### Added

- **`kno bridge` measures instead of refusing to start.** The tuner-bridge
  spine (submit, poll, reconcile, deploy, hosting settle-forward, teardown,
  the resume-time endpoint sweep) shipped in `feat/tuner-bridge` (#184)
  without a production `EvalRunner` — the seam that invokes a deployed
  proxy model over Cases and scores it — so an armed run planned, priced,
  confirmed, and then stopped with an actionable refusal (docs/debt.md#158).
  This closes that loop:

  - **`core.ScorePass`** (`core/score.go`), a new exported seam: invokes an
    Agent once per Case and scores it, under the same budget-guarded,
    retrying, panic-safe path Value and Validate already share
    (`core.invoker`, unexported and unchanged). Takes a `*core.SealedEvals`
    — the holdout cannot be reached through it — and `Skip`/`OnScored` so a
    caller checkpoints per Case as it happens, the unit resume correctness
    needs.
  - **`kno bridge` gained `--evals`**, resolved and sealed once at the CLI
    choke point, filtered to exactly the Case IDs the value.Plan names. A
    Case ID the plan names with no Case behind it refuses the WHOLE run
    before any job is submitted, naming the Case.
  - **The all-in model is actually scored.** It is deployed, invoked over
    the union of every group's dev Cases plus the reserved control
    partition while its endpoint is live (the only moment it exists), and
    its per-Case scores are durably persisted — never held only in memory —
    so every leave-one-out group's delta, computed as
    `Δ = groupScore[id] − allInScore[id]`, pairs against a real number
    instead of a baseline that was torn down before it was ever measured.
  - **`bridge.EvalRunner.Measure`** now returns `map[string]float64` keyed
    by Case ID — raw scores, never deltas, never positional — because a
    Case routed to two failure clusters (`core/value/route.go`'s
    `cluster()` assigns per tag) sits in two groups' dev sets with
    different membership and ordering than the all-in union pass. Pairing
    moved into `bridge.Run`, the component that legitimately holds both
    sides.
  - **Resume never re-pays and never loses a paid-for verdict.** A group
    whose Cases are all durably scored redeploys nothing; a group whose
    Cases are scored but whose `BridgeGroupMeasured` event was never
    recorded (a crash between the two) is recomputed from stored scores and
    emitted, rather than silently dropped.
  - **`BRIDGE_GROUP_VERDICT_INTERFERENCE` is now emitted.** A new exported
    `stats/interval.NetEffect` combines a group's goal delta and its
    control-partition delta into one net judgement — extracted from
    `core/select.go`'s previously-unexported `netInterval`, which now
    delegates to it, characterization-tested against the pre-extraction
    formula for behavior preservation. Bonferroni correction is goal-only,
    with `N` pinned at the group count planned at quote time.
  - **`store.TuningJobRecord` gained `VerdictEmittedAt`** (schema version 7
    → 8) — the durable marker the resume logic above reads.
  - **`adapters/tuner/together`'s poll tests moved to on-disk fixture
    sequences** (`testdata/fixtures/poll-sequence`,
    `testdata/fixtures/poll-failed`): `poll-01.json` … `poll-NN.json`,
    replaying the full `VALIDATING_FILES → QUEUED → RUNNING → DEPLOYING →
    SUCCEEDED` state machine and a `FAILED` branch surfacing the
    provider's error text verbatim, deterministically and with no network.

  Known gap, tracked as docs/debt.md#161: the deployed-endpoint inference
  route this PR's CLI wiring assumes (`together.DefaultBaseURL + "/v1"`,
  OpenAI-compatible) is unconfirmed against a live Together dedicated
  endpoint — this pass did not run a live (`KNO_LIVE_TESTS=1`) bridge run.

### Changed

- **The release gate reads a minor series, not only a patch-complete version —
  and the whole ledger was audited against it before v0.2.**
  `scripts/ledger-check.py` matched the exact version string it was given, so it
  saw a trigger that wrote `0.2.0` and was blind to every trigger that wrote
  `v0.2` or `before 1.0`. That is how the ledger's authors actually write, and
  the cost was not theoretical: at a `1.0.0` tag the old pattern matched
  **nothing**, because not one "before 1.0" entry spells the patch digit, so the
  gate `CLAUDE.md` rests on would have waved through every 1.0 obligation in the
  table. It now also matches the release's minor series, with a lookahead that
  keeps `0.2` off `0.25` (entry #33's feasibility constant is not a version) and
  a numeric-only guard that leaves a pre-release tag like `0.0.0-selftest`
  narrow, so `scripts/selftest.sh`'s "a release nothing names" case still names
  nothing. Seven tests cover the new leg, including the two false-positive
  shapes.

  The v0.2 audit that found it disposed all 65 open entries. Two are repaid
  (`#88`, `#135` — both verified from the consuming repository rather than
  taken on trust), three are partly repaid, two are re-dated with written
  reasons, one (`#83`) is recorded as due work, and 57 are carried with a stated
  reason each. Two entries had
  triggers that had already fired unnoticed: `#139`'s at this release, and
  `#79`'s **before the entry was written** — `costOf` has priced cache writes
  in three adapters since 2026-08-22, so that leg was false the day it was
  recorded. Five more rows (`#3`, `#33`, `#68`, `#70`, `#78`) had been re-dated
  in the *What* column while the *Repayment trigger* cell still held the dead
  trigger, and the gate reads only the trigger cell; those are now consistent.
  `#83`'s parquet deferral is out of deferrals: its trigger has fired twice, a
  third re-date is not available, abandoning it was considered and rejected on
  the record, and it is now recorded as due work scoped in issue
  [#154](https://github.com/uknoAI/kno/issues/154).

- **The cookbook migration is finished: `calibrate-a-judge` has moved, and no
  recipe lives in this repository any more.** `docs/cookbook/calibrate-a-judge.md`
  is now a one-line tombstone pointing at
  [`kno-examples/recipes/calibrate-a-judge.md`](https://github.com/uknoAI/kno-examples/blob/main/recipes/calibrate-a-judge.md),
  which is where every other recipe already was.

  It was the last page held back, and the reason it was held back has a shape
  worth recording. `kno-examples` checks every command on every page against
  the **released** binary, so a page documenting a command that is on `main`
  and in no release can carry no honest tier there — and there is no tier for
  "documents an unreleased command", because inventing one would let any page
  claim verification against a binary that cannot run it. v0.1.5 shipped
  `kno judge calibrate` and the reason expired.

  The page did not settle for a hand-checked tier on arrival. `--replay` is
  the default and makes no provider call, the 60-record calibration set is
  built into the binary, and repeated runs produce byte-identical output — so
  it migrated as `executed`, with a scenario that asserts kappa 0.867, the
  bootstrap interval, the sensitivity/specificity split, and that the gate
  **still exits 1** at two floors it must refuse: one where the interval
  contains the floor, and one where the floor is above the inter-human kappa
  the labelers themselves reach.

  **Reader-visible**: a bookmark or an issue link to
  `docs/cookbook/calibrate-a-judge.md` now lands on a redirect line rather
  than on the page. That is what every tombstone here is for — nothing in
  either repository reports an inbound link 404ing, since `make docs` skips
  `https://` targets and the website's crawl skips external hrefs.

  `CONTRIBUTING.md` and `docs/evaluation-design.md` linked at the old path and
  now link at the real page: a link this repository controls should point at
  the thing rather than at the redirect.

  `scripts/cookbook-stub-check.sh`'s RESIDENT list is now empty, and the
  comment above it says why keeping it empty is the point. A name added there
  must come with the release that will ship its command.

### Fixed

- **`kno value` could not measure an Asset against any real provider.**
  `measureAsset` built its treatment arm from a freshly constructed
  `&Asset{Id: routing.AssetID}` — the ID and no content — while the Pool's real
  Asset was already in scope as its own parameter. Against an adapter that
  validates what it is handed (`openaicompat`, which refuses an empty Asset
  precisely to prevent the other failure) every measurement was refused and the
  stage could not run. Against one that does not validate, the treatment request
  was byte-identical to the control's: every paired difference exactly zero with
  a tight interval around it, which reads in the report as *"measured, and
  inert"* — the one conclusion the stage exists to reach honestly.

  It survived because the pipeline had only ever been driven end to end against
  `fake:`, and because `stubAgent.WithContext` ignores its argument — a test
  double more permissive than every adapter it stands in for cannot fail where
  they would. Found by the first live run against a real provider.

- **`kno value` and `kno validate` sent an explicit `temperature=0` on every
  run.** Their `--temperature` flags defaulted to `0` while `baseline`'s
  defaulted to `math.NaN()`, and `optionalFloat` treats only NaN as unset — so
  the help text's *"unset leaves the provider default"* was true of one stage
  and false of two. Visibly, a model that rejects sampling parameters became
  unusable in both, with a refusal naming a flag the user never passed. More
  seriously, baseline measured a model at the provider's default temperature
  while value and validate measured the same model at 0 — and baseline is the
  reference every later delta is computed against, so a sampling difference was
  attributed to the Asset. All three now default to NaN, asserted across every
  command that declares the flag rather than the three that have it today.

- **A budget cap reached while a dedicated endpoint was hosting did not stop
  the spending.** `bridge`'s hosting ticker settled minutes forward through
  the budget guard and discarded the result as `_, _ =`. When the guard
  refused — the cap reached mid-hosting — nothing observed it: the endpoint
  went on billing by the minute until the measurement finished on its own.
  Prime directive 4, and invisible to `errcheck`, because an explicit
  two-blank discard is not an unchecked error.

  The ticker now reports its first settle error and cancels the measurement's
  context on a refusal, so the in-flight measurement returns and the deferred
  teardown runs immediately rather than after work nobody can pay for. The
  ticker itself keeps the parent context and goes on settling the minutes
  actually consumed between refusal and teardown — those are real charges,
  and recording them as orphan spend is the honest treatment; cancelling the
  ticker too would simply lose them.

  Pinned by `TestReachingTheCapMidServeStopsTheMeasurementAndTearsDown`,
  verified failing without the fix with a measurement that never returns.

## v0.1.6 — in detail

### Fixed

- **`kno export --destination tuning_set` produced files no provider can train
  on.** `renderTuningSet` emitted one `user` message per Asset and no
  `assistant` turn — every line was
  `{"messages":[{"role":"user","content":"..."}]}`. Every hosted fine-tuning
  API requires at least one assistant message per example, because without one
  there is no target to train on; a provider's file-validation step exists
  precisely to reject this. The artifact was a list of prompts wearing the
  JSONL of a training set.

  Content is now rendered one of two ways. Content that is already chat JSONL
  passes through re-marshaled — and is **refused** if it carries no assistant
  turn, rather than shipped as an untrainable line. Anything else is treated
  as the demonstration itself and wrapped as an assistant message. Empty or
  whitespace-only content is refused: a zero-example demonstration is not
  trainable at any price, and paying a provider to discover that is a paid
  no-op.

  **Behaviour change**: the pinned output moves from `"role":"user"` to
  `"role":"assistant"`, and an Asset whose content cannot become a trainable
  example now fails the export with an actionable error instead of writing a
  line that would be rejected later, further from the cause.

- **A confidence interval could collapse to a point, and report itself as a
  Student-t interval.** `stats/interval`'s degenerate-sample guard was
  `variance <= 0`, meant to catch "every pair identical" and hand it to the
  sign bound, which is deliberately wide. Summing `(d-mean)²` over identical
  **small-magnitude** deltas does not give zero: the mean of fifty copies of
  `0.001` is not `0.001` in binary floating point, so each residual is about
  `1e-19` and the variance is a positive number made entirely of rounding
  noise. A positive variance takes the Student-t path, and `sqrt(1e-38/n)` is
  a half-width around `1e-19` — so fifty identical observations produced an
  interval of width `4e-19`, reported as method `t`.

  That is a claim of perfect certainty from a sample with no spread at all,
  which is precisely what the sign bound exists to prevent ("wide, honest, and
  impossible to mistake for certainty") and what `build`'s zero-width guard
  believes it enforces — `build` refuses only `half <= 0`, and `1e-19` is
  greater than zero. It reached every caller: a Value delta, a Validate
  holdout gain and a Select screening interval all compute through this path,
  so any Asset whose per-Case differences came out identical and small carried
  a CI that had collapsed. Prime directive 5 says no reported delta without
  its CI; a CI of width `1e-19` is worse than none, because it reads as a
  measurement rather than as the refusal it should be.

  The guard now tests exact equality — the semantic condition, immune to how
  residuals round — and, separately, whether the standard deviation is below
  the floating-point resolution of the data it was computed from. Verified in
  both directions: identical deltas across five magnitudes now take the sign
  bound, and samples with genuine spread (including one dissenter among
  identical values, and a real spread at `1e-6`) still take Student-t.

## v0.1.5 — in detail

### Fixed

- **The changelog fold no longer races the tag, and leaves an `[Unreleased]`
  heading behind.** `scripts/fold-changelog.sh` renames the hand-written
  `## [Unreleased]` heading to the release that shipped it. It opens that as a
  PR at release time and auto-merges it later, when checks go green — so
  anything merged into `main` in between landed under the very heading the
  rename was about to consume, and got filed under a release it did not ship
  in. That is not hypothetical: v0.1.4's fold PR sat open while two PRs
  merged, and rebasing it swept both of their entries into
  `## v0.1.4 — in detail`. The script now compares the tree's `[Unreleased]`
  block against the tag's, and when they differ it still folds — the heading
  does belong to the release — but withholds auto-merge and prints the added
  entries in the PR body, so a human splits them rather than a script guessing.

  Separately, the fold consumed the `[Unreleased]` heading without writing a
  new one, leaving the file with no such section until somebody recreated it
  by hand. `.github/workflows/changelog.yml` requires *"an entry under
  ## [Unreleased]"* from every `feat:`/`fix:`/`docs:` PR, so the next
  contributor met a red gate with no visible cause. A fresh empty
  `[Unreleased]` is now written above the folded heading.

### Tests

- **The holdout canary is scoped to a run ID, not a method name.**
  `TestSelectHoldoutCanary` asserted holdout isolation by listing forbidden
  reader methods, which was sound only while no holdout row existed
  anywhere Select could read. `kno validate` ended that: it records holdout
  measurements through `RecordMeasurement` and reads them through
  `Measurements`, and `Measurements` was not on the list. Nothing leaked,
  because Select does not call it — but the day it did, the canary would
  have gone green while the guarantee it names was gone. Select may now
  read `Measurements` only for the gated Value run and `CaseScores` only
  for that run's recorded baseline; any other run ID fails. That is
  strictly stronger than before for `Measurements`, which was unguarded.
  `TestSelectHoldoutCanaryCatchesAForeignRun` watches the guard fail, since
  a guard on a reader nobody calls yet proves nothing by passing.

### Bug Fixes

- **A resumed Validate run restores its token spend too.** The fix below
  landed in `core/value_loop.go` and its own note said why it could not
  wait — `validate` had shipped as a second resuming spend stage on the
  same `SettledSpend` path. Then it changed only the Value sink.
  `core/validate_loop.go` was still recording
  `budget.Spend{Calls, CostUSDMicros}`, so a resumed Validate run restored
  zero tokens and under-enforced `--max-tokens` for the rest of the run,
  identically. Nothing caught it because
  `TestValidateMeasuresBothArmsOverTheHoldout` compared cost and calls and
  skipped tokens, and the stage's scripted agent declared
  `TokenCounts: true` while returning none — the dimension was
  unobservable in the one stage that had it wrong. Both are fixed, and the
  assertion is verified failing with the fix reverted. See docs/debt.md#137,
  which is annotated with how a defect gets repaid as literally as it was
  written.

## v0.1.4 — in detail

### Bug Fixes

- **A resumed Value run restores its token spend.** `core/value_loop.go`'s
  sink recorded `budget.Spend{Calls, CostUSDMicros}` and dropped `Tokens`,
  while Baseline's `settledSpend` wrote all three. `Store.SettledSpend` sums
  a `tokens` column Value never populated, and `Guard.Restore` seeds from
  that sum — so a resumed Value run restored zero tokens and a
  `--max-tokens` cap went under-enforced for the whole second process while
  the dollar cap held. Partial enforcement is the worse failure, because the
  run looks guarded. Found while writing the equality test for the
  stage-spend work; repaid ahead of its trigger because `validate` shipped
  as a second resuming spend stage on the same path. `docs/debt.md#137`.

- **The `tuning_set` export now emits a trainable example, not a promptless line. BEHAVIOR
  CHANGE.** `renderTuningSet` (`core/export.go`) wrote one `user` message per selected Asset and
  no `assistant` turn at all — every hosted fine-tuning API rejects a training example with no
  target, so the artifact `kno export --destination tuning_set` produced was a list of prompts
  wearing the JSONL of a training set, not a training set. It now parses content that is already
  chat JSONL (re-marshaling it to compact single-line form) when it carries an assistant message,
  and otherwise wraps the content as a single `assistant` turn. Content that is neither — empty,
  or chat-shaped JSON with no assistant message — is refused at export, naming the Asset, rather
  than silently shipped as an untrainable line. The golden in `TestExportTuningSetPinned` changed
  accordingly; `TestRenderTuningSetRequiresAnAssistantTurn` and
  `TestOldSingleUserMessageShapeIsNeverProducedAgain` pin the fix and its negative case. There is
  no accessible Goal instruction at the Export stage today (`core.Goal` exposes `Score`/`Domain`/
  `Direction` and no instruction text), so the wrapped example carries no `system` message — a
  gap from the tuner-bridge plan's original description, noted rather than papered over.

### Features

* **`kno judge calibrate` — a judge is measured before it is trusted.** `CLAUDE.md` and
  `CONTRIBUTING.md` have both stated, in the present tense, that "judges are tested against the
  human-labeled calibration set with agreement thresholds — a judge prompt change that drops
  agreement below threshold fails CI"; `DESIGN.md` listed the command; *What the numbers mean*
  told you to run it before trusting a judged number. None of it existed. This ships the
  mechanism those sentences describe: the harness, a committed calibration set, and a CI gate
  (`make judge-calibrate-check`, now in `make check`, making no provider call). It deliberately
  does **not** ship a judge — the gate is here first so the first judge prompt arrives with a
  threshold already pointed at it, rather than the threshold arriving later and grandfathering
  whatever shipped. Until then the mechanism is real and its coverage is vacuous, and the docs
  say that in those words.

  **The gated statistic is Cohen's kappa, and the floor is derived rather than borrowed.** Not
  accuracy: on a set that is 85% "good" a judge that answers "good" unconditionally scores 0.85
  raw agreement and is worthless — it scores kappa 0, exactly, and
  `TestConstantJudgeScoresZeroKappaDespiteHighRawAgreement` is the test that carries the claim.
  Raw agreement is still printed, with the constant judge's own score beside it. The floor is
  **kappa ≥ 0.60**, and the argument is not Landis-Koch: on a balanced set with symmetric error,
  kappa *is* the factor by which the judge attenuates every delta measured through it
  (`kappa = 1 − 2ε`), and power scales with the square of the effect, so 0.60 is the point where
  a judge costs at most 3× your eval budget. It is a published price, not a convention, and
  `--min-kappa` is there for a user who thinks 3× is too generous. Both assumptions the
  derivation rests on are checked rather than assumed: balance is enforced at load (the minority
  class must be ≥ 40%, with the prevalence-sensitivity table pinned as a test), and symmetry is
  measured — `|sensitivity − specificity| > 0.20` fails **even above the floor**, because a
  direction-biased judge moves every delta the same way and kappa hides it.

  **Three verdicts, and the third fails.** An interval entirely above the floor is `PASS`;
  entirely below is `FAIL`; straddling it is `INDETERMINATE` and exits 1, because "we cannot
  tell" is not "it is fine" — the same discipline `core/gaps.go` uses for `UNKNOWN`. Two further
  verdicts blame something other than the judge: inter-human kappa below the floor reports that
  the *labels* do not agree with each other, and a judge error rate above 5% reports "not a
  usable calibration", the same threshold and the same words as the baseline gate. A graded
  (`UNIT_INTERVAL`) Goal reports weighted kappa, Spearman's rho and MAE and prints
  `GATE: not applicable` (`docs/debt.md#152`).

* **A percentile bootstrap in `stats/interval` — the repository's first.** `Interval.method`'s
  godoc has listed `"bootstrap"` since the schema landed, beside an implementation that shipped
  adjusted-Wald, paired-t and sign intervals and no bootstrap at all. `interval.Percentile`
  resamples the *units* a caller nominates, which is what makes it valid for kappa (the unit is
  the record) and what makes the ratchet's difference interval **paired**: both runs are
  recomputed on one index draw, so their co-movement is kept rather than discarded. It is
  deterministic by seed — a gate whose verdict flips without a diff is not a gate — and it never
  returns a zero-width interval: a degenerate resample takes its width from the sample size and
  says so in its method name. Coverage is measured on the grid the decision actually lives in,
  n ∈ {50, 100, 200} × kappa ∈ {0.55…0.65}, reading 0.923–0.970 against a nominal 0.95
  (`docs/debt.md#153`).

* **`goal.Registry` replaces `resolveGoal`'s hardcoded `if`, and it is default-deny.**
  `--goal` was matched against `if name == "exact-match"`, whose own fix line could only ever
  name that one Goal. The registry names what is actually available. It also carries the
  containment for a P0 this feature would otherwise open: `core.Goal.Score` runs **outside** the
  budget reservation — `core/invoke.go` brackets the agent call and `Score` runs after the
  reservation settles — so a Goal that calls a provider inside `Score` spends money the guard
  never authorized. Registration therefore refuses any Goal whose name is absent from a
  compile-time self-contained allowlist, **whatever the Goal declares about itself**. A
  must-affirm marker method was considered and rejected as strictly weaker: it is still a
  self-report, so it catches the author who forgot and admits the one who writes
  `SelfContained() { return true }` on a Goal that calls a provider. The accepted cost is that
  an out-of-tree Goal cannot register at all (`docs/debt.md#150`, `#151`).

  **The calibration set is public, permanent, and synthetic.** Question-and-answer records over a
  fictional API, written from a human-authored template, two independent labels and an
  adjudicated verdict each. No customer content, no trace content, no PII — and the format has no
  spelling for a harvested record: the loader refuses any provenance that is not `authored` or
  `synthetic`. A public set is contaminated for training purposes the day it ships, so it is a
  regression instrument rather than evidence a judge generalizes, and *What the numbers mean*
  says so (`docs/debt.md#154`). Adding records is a contributor on-ramp that exists before any
  judge does.

  **Every reported statistic is rounded to four decimal places where it is computed.** Not
  cosmetic, and now a stated rule rather than a local habit ([ADR-0006](docs/adr/0006-the-json-contract.md)
  rule 6): Go may fuse a multiply-add into an FMA, which arm64 does and amd64 does not, and a
  bootstrap bound additionally passes through interpolation and `math.Log`. An unrounded
  `kappa_interval.high` read `0.929508759876331` on darwin/arm64 and `0.9295087598763309` on
  linux/amd64 — one ULP apart, with the human rendering byte-identical, and no golden can hold
  both. Rounding at the source is also the honest reading: a percentile bootstrap over thirty
  records does not carry seventeen significant digits. `kno eval inspect` already did this for
  `separable_effect`; this generalizes the rule and adds a property test rather than trusting a
  golden to catch the next one.

* **`kno bridge` — the tuner-bridge engine (partial; v0.2, DESIGN.md's Tier 3).** Group-ablates
  the tuning set's behavior Assets on a proxy model: plans and prices every leave-one-group-out
  fine-tuning job locally, with zero network calls and zero dollars spent, before anything is
  armed. `AssignGroups`/`BuildGroups` (`bridge/`) compute each ablation group by the exclusive
  intersection rule — the cluster an Asset shares the most **routed** dev Cases with, never by
  cluster size — reusing the failure clusters `kno value` already persisted rather than
  reclustering. `QuoteGroups` renders every group's training file **byte-identical** to what
  `kno export --destination tuning_set` writes for the same Asset subset, and prices it against
  `pricing.TrainPrice`. `SubmitGroup` (`bridge/submit.go`) implements the money-safety sequence a
  fine-tuning job needs that no other spend path in Kno has had to: authorize through the budget
  guard on all three dimensions (Calls/CostUSDMicros/Tokens — the same fix `#170`/`#172` shipped
  for Value and Validate, not reintroduced here), write a durable `submitting` row **before** the
  irreversible request leaves, and never retry a submit blind. `ReconcileTerminal` handles the
  actual-vs-estimate true-up: an overrun goes through `RecordOrphanSpend` + `Guard.Restore`, an
  underrun is never credited back, and an unreported actual leaves the estimate standing, labelled
  `estimated`, never `billed`. `core.Tuner` gains `Deploy`/`Teardown` (additive) for Together's
  second spend shape — a dedicated hosting endpoint billed per minute per replica, idle included,
  which Together does not auto-serve — and `store` gains a `tuning_jobs` table (schema version 7)
  whose two spend dimensions join `SettledSpend`'s existing three-way sum.
  `adapters/tuner/together` is the first `core.Tuner` implementation, following the adapter
  posture of `adapters/agent/anthropic` with a **local** security layer rather than a ported one —
  `adapters/agent/internal/transport` is a Go `internal` package reachable only from
  `adapters/agent/*`, so `adapters/tuner/together` cannot import it; see the package's `security.go`
  for what that cost.

  **This PR's own follow-up** finishes the orchestration loop: `bridge.Run` (`bridge/run.go`)
  actually submits every group's job, polls it to a terminal status (`--bridge-timeout` stops
  WAITING without cancelling, never re-submitting on resume), reconciles the actual-vs-estimate
  true-up, deploys the finished model, and tears the endpoint down on **every** exit path. Crash
  recovery upgrades from "always abandon" to **adopt-by-suffix**: `core.Tuner` gains
  `ListJobs`/`ListEndpoints` (additive, beyond the original plan's "exactly two methods" framing —
  Step 2(d)/2(g) always described listing as the mechanism), and a row a crash leaves
  `submitting` is adopted when the provider confirms the job, abandoned only when it does not. The
  hosting lifecycle is complete: a per-minute settle-forward tick (`SettleServeTick`,
  `pricing.SettleServeMinutes`), a `--bridge-max-live-endpoints` semaphore (`LiveEndpointLimiter`),
  and a resume-time sweep (`SweepEndpoints`) that tears down anything this run's own rows show
  live — by recorded `EndpointID` first, by `ListEndpoints(suffix)` when a crash landed between
  `Deploy` succeeding and that ID being recorded — settling at `--bridge-max-serve-minutes` when
  the provider no longer lists the endpoint at all. `store.LeakedEndpoints` and `kno doctor --db`
  report any row, across every run, carrying a non-null `EndpointID` with a null `TornDownAt`.
  The consent quote gains a separately-labelled, cap-bounded hosting line
  (`pricing.EstimateServeCap`), and `--bridge --help` now names every flag the plan specifies:
  `--bridge-timeout`, `--bridge-cancel-on-timeout`, `--price-serve-per-minute`,
  `--bridge-max-live-endpoints`, `--bridge-max-serve-minutes`.

  **What still does not ship:** the per-group leave-one-out MEASUREMENT itself.
  `bridge.Run` takes an `EvalRunner` — the seam that would invoke a deployed model over dev and
  control Cases and score it — and refuses to start without one rather than deploying a paid
  endpoint with nothing to measure it. No production `EvalRunner` ships in this PR: it needs an
  Evals source `kno bridge`'s flags do not yet accept, and the same budget-guarded retrying invoke
  path Value and Validate already share (`core.invoker`) is unexported from `core` today. So
  `kno bridge --bridge --yes` still plans, prices, and confirms — now including the hosting line —
  and stops with an actionable error, never a fake success. `docs/debt.md#156` (the leak risk) is
  repaid at the mechanism level and stays open only pending this gap; `docs/debt.md#158` records it
  with its trigger. `docs/status.json` still carries `bridge` as `partial`, not `shipped`.

* **`kno validate` — the holdout stage.** The Portfolio ships as a set, so it is measured as a
  set, against the slice nothing has read. Validate runs the holdout twice inside one run: a
  **control arm** with nothing injected and a **treatment arm** carrying the whole Portfolio as
  one ordered payload in the system position. It reports the mean paired difference with its
  interval, and a verdict keyed on that interval and never on the sign — `confirmed` above zero,
  `inconclusive` across it (exit 0, or 3 with `--require-gain`), `not_confirmed` below it
  (exit 3, unconditionally), `unmeasured` when no interval could be formed. This is the first
  code path to return `ExitValidationFailed = 3`, which has existed with that godoc since v0.1
  and had never been returned.

  Three things about it are decisions rather than mechanics, and each is documented where it
  will be read. **The control arm is measured, not read off the baseline:** `core.Baseline`
  takes a sealed dev-only view, so no baseline run has ever scored a holdout Case, and
  subtracting the dev mean instead would fold the Portfolio's effect together with a random
  dev/holdout population difference and provider drift. Two arms cost `n_holdout x 2 x trials`
  agent calls and the consent quote shows that derivation. **The holdout is consumed once per
  Portfolio,** recorded before the first agent call rather than at completion, because a
  crashed validate has already peeked; the same Portfolio a second time is refused with no
  override, and an interrupted run is resumed rather than restarted. **A different Portfolio
  against the same holdout is allowed under `--allow-repeat-holdout`, counted, and disclosed** —
  and not corrected for, because refusing outright would push people to delete the database or
  re-split, turning a counted peek into an invisible one. Pairwise interaction detection does
  **not** ship: `interaction_penalty_detected` is `false` unconditionally, a scope reduction
  against `DESIGN.md:88` recorded as [debt #141](docs/debt.md) rather than quietly resolved.

* **`kno report --validate-run-id <id>`** renders the holdout number, and the *not yet validated
  on holdout* caveat becomes conditional for the first time. Its absence has to be **earned**:
  the caveat is still printed byte-for-byte whenever there is no COMPLETED Validate run carrying
  an interval for this Portfolio — an INTERRUPTED validate keeps the caveat and adds a line
  saying a validation was attempted and produced no number. A partial peek is not a validation,
  and a page that dropped the caveat because someone *started* a validate would be the exact
  dishonesty the caveat exists to prevent.

* **Proto: a new `Validation` message, plus two additive fields.** `kno/v1/validation.proto` is
  new, so `buf breaking` has nothing to compare it against. `PortfolioEntry.content_hash`
  (optional, field 6) is written by Select and checked by Validate, so a Pool edited between the
  two stages is refused **before any spend** rather than producing a number about a set that no
  longer exists. `Capabilities.context_set_inject` (field 7) is how an agent adapter declares it
  can take a whole Portfolio in one call — every `ContextInjector` refuses a second Asset by
  design, and that refusal protects the Value stage and is not being loosened.

* **Store schema version 6** (additive): `validations` records what one Validate run measured,
  and `holdout_uses` records that a Portfolio has met a holdout, keyed on
  `(eval_fingerprint, select_run_id)` and written in the same transaction that creates the
  Validate Run. The second table is the one-shot rule made durable — before it, "the Portfolio
  meets the holdout exactly once" was prose in `docs/mental-model.md` and nothing enforced it.

* **every stage reports what it spent, or says it could not.** `kno value` — the stage
  `DESIGN.md` sizes at $15–40 for a run against a baseline's fraction of a dollar — reported
  nothing about money in either rendering. It now carries the same spend block `kno baseline`
  has printed since v0.1, through one shared renderer, and `kno report` gains the pipeline
  total: what knowing this cost.

### `--json` contract changes (all additive)

New keys. Nothing is renamed, removed or retyped; `spent_usd` keeps its v0.1 value byte for byte.

* `kno baseline --json` and `kno value --json` gain `guarded` (always `true`),
  `spent_usd_micros`, `llm_calls`, and — when they have something to say — `tokens`,
  `usage_estimated_cases`, and `resumed`. `kno value --json` gains `spent_usd` for the first
  time.
* `kno select --json`, `kno export --json` and `kno report --json` gain `guarded: false` and
  emit **no** spend keys at all. This is deliberate and is the point: these stages run no budget
  guard, and a uniform `"spent_usd": "$0.00"` would be indistinguishable from a stage that spent
  money with a missing meter — which is exactly how the `kno value` hole survived v0.1. Absence
  alone is not enough either, because `jq` cannot tell a missing key from an explicit null and
  the repair a consumer reaches for, `.spent_usd // 0`, reinstates the ambiguity on the reading
  side. So the documents say it positively. **The CI idiom is
  `map(select(.guarded) | .spent_usd_micros) | add`, never `// 0`.**
* `kno validate --json` is new: `holdout_gain` with its `low`/`high`, `verdict`,
  `control_score`/`treatment_score`, `dev_estimated_gain` and `shrinkage`,
  `holdout_underpowered`, `holdout_uses`, `context_only`, `interaction_penalty_detected`, and
  the standard `guarded` spend block. `kno report --json` gains a `validation` object, absent
  when no Validate run was named — and `portfolio.validated_on_holdout`, which v0.1 hardcoded to
  `false`, is now computed, so it says positively what the absent object says by omission.
* `kno report --json` gains a `spend` object: one entry per metered run it names (baseline and
  value; select and export are absent rather than zero), plus `total_usd`, `total_usd_micros`,
  `total_llm_calls`, `incomplete`, and `no_metered_spend`.
* `spent_usd` stays a formatted display string, and `spent_usd_micros` exists because the string
  cannot be summed. The string is for eyes; the micros are for arithmetic.

Why `spent_usd_micros` rather than retyping `spent_usd`: retyping a released key breaks every
`jq` pipeline written against v0.1, and the recipe that samples it now lives in a repository this
one cannot update in the same commit.

### Migration notes

- **`store.Store` gains four methods: `WriteValidation`, `Validation`, `RecordHoldoutUse` and
  `HoldoutUses`.** This is a pre-1.0 interface break for anyone implementing `store.Store`
  outside this repository — an existing implementation no longer satisfies the interface and
  will not compile until it grows all four. The in-tree implementation and every fake are
  updated. It is a break rather than an addition because `Store` is an interface, not a struct,
  and there is no honest default: a `RecordHoldoutUse` that silently did nothing would turn the
  one-shot rule into a suggestion, and `Validation` returning a zero value would make an
  unvalidated Portfolio indistinguishable from a validated one that measured nothing.
- **The store schema moves to version 6** (additive): two new tables, `validations` and
  `holdout_uses`. Existing databases upgrade in place on open; no data is rewritten, and a run
  recorded before this version simply has no validation row, which the report reads as "not yet
  validated on holdout" rather than guessing.

### Documentation

* **[ADR-0006: the `--json` contract](docs/adr/0006-the-json-contract.md)** — new, and now the
  **only** statement of that contract in this repository. The previous one was a sentence in
  `docs/cookbook/ci-gate.md`; the cookbook migration (#163) turned that page into a one-line
  tombstone and the sentence left the repo with it, while the behavior it described stayed
  exactly the same. Six rules, plus an explicit note that the ADR records a decision and does not
  enforce one — the goldens and the tests do that.
* `docs/what-the-numbers-mean.md` gains *What a reported spend figure covers*: run-lifetime, not
  per-session; settled, not billed; and why a stage reporting no figure is not reporting zero.
* `kno value --help`, `kno select --help` and `kno export --help` say which of them meters and
  which does not, and where an unmetered stage's cost actually lives.

### Internal

* The repository's first `--json` goldens (`cli/testdata/json/`), one per stage, plus
  `v0.1-shape.json` — a frozen capture of what v0.1.0 emitted unconditionally, asserted as a
  subset of current output so a released key cannot be renamed or retyped unnoticed.
* `core.ValueResult` gains `Spent`, populated on **every** path that returns a result, error
  paths included: a run that settled real charges and then failed for a reason unrelated to
  money still reports what it cost, before the error.

## [0.1.3](https://github.com/uknoAI/kno/compare/v0.1.2...v0.1.3) (2026-08-31)


### Features

* docs/status.json — generated status data for the site, gated in make check ([#150](https://github.com/uknoAI/kno/issues/150)) ([cb50d5b](https://github.com/uknoAI/kno/commit/cb50d5bdf35e65bcb0d11609e42d7561396a7f5c))
* kno eval inspect — whether an eval set can support attribution ([#155](https://github.com/uknoAI/kno/issues/155)) ([18ebb4a](https://github.com/uknoAI/kno/commit/18ebb4a7c7ec865b15b7ce2a4c23b9147a58e4e9))


### Bug Fixes

* **cli:** kno export --json names the Select run it rendered from ([#156](https://github.com/uknoAI/kno/issues/156)) ([074d73f](https://github.com/uknoAI/kno/commit/074d73fbcedca5184cca0a38f1b51c9fe25f9c09))
* **core:** the rejection log prints bounds at four places, not seventeen ([#157](https://github.com/uknoAI/kno/issues/157)) ([350bc01](https://github.com/uknoAI/kno/commit/350bc0152a952f8a685ec3005050014ed3425271))
* **value:** the harm bound is the exact t quantile, not z beyond df=30 ([#158](https://github.com/uknoAI/kno/issues/158)) ([4622b90](https://github.com/uknoAI/kno/commit/4622b90ef06e41938014f860e2ecc0225d291587))


### Documentation

* answer the examples plan's blocking question by reading kno-www ([#146](https://github.com/uknoAI/kno/issues/146)) ([9bc5c47](https://github.com/uknoAI/kno/commit/9bc5c47bfef0058c1c4430ee9b6ac1032b98ddbe))
* **debt:** record two live defects found by the v0.2 Phase-0 workstreams ([#159](https://github.com/uknoAI/kno/issues/159)) ([37967e5](https://github.com/uknoAI/kno/commit/37967e584c221f5722e007cdf804ee2fd85a471a))
* fold the hand-written changelog into v0.1.2 ([#145](https://github.com/uknoAI/kno/issues/145)) ([bd858c3](https://github.com/uknoAI/kno/commit/bd858c3f75a1b785b63eb651c18271cacaa967f4))
* Phase-0 plans for v0.2, all adversarially reviewed ([#161](https://github.com/uknoAI/kno/issues/161)) ([83c7641](https://github.com/uknoAI/kno/commit/83c76416c87d653126bfb979e9cf677d1f994e5b))
* stop advertising an on-ramp that does not exist yet ([#162](https://github.com/uknoAI/kno/issues/162)) ([89ce9ee](https://github.com/uknoAI/kno/commit/89ce9eeb7847c9b1b7b1410135ee4663a686e3c9))


### Build & Dependencies

* Bump anchore/sbom-action/download-syft from 0.24.0 to 0.24.2 ([#135](https://github.com/uknoAI/kno/issues/135)) ([23dcfe0](https://github.com/uknoAI/kno/commit/23dcfe031cc5c00718b3c66d0ccd224f3a35f651))

## v0.1.3 — in detail

### Changed

- **The cookbook moved to [`uknoAI/kno-examples`](https://github.com/uknoAI/kno-examples).**
  Twenty-five recipes left `docs/cookbook/`; each old path keeps a one-line stub pointing at
  its new home, and `make docs` fails if a stub is missing, is longer than one line, or carries
  anything but its link. The reason for the move is narrow and worth stating: **no CI job
  anywhere had ever run a command from this repository's documentation.** `make docs` checks
  godoc coverage and relative-link integrity — both real gates, neither of which executes
  anything — so the cookbook was twenty-five pages of instructions, none of them run, some
  already contradicting each other. `kno-examples` runs them nightly against the binary you can
  download, which is a question this repository cannot answer about itself: a job here can test
  HEAD (which nobody has) or the previous release (which cannot validate the change under
  review).

  Eleven recipes are now executed end to end against committed expectations; the rest declare,
  on the page, exactly what was and was not checked. `docs/cookbook/README.md` stays as the
  index. The README quickstart stays here in full and self-contained — the front door must not
  depend on a second repository being reachable. One page did not move:
  `check-your-evals`, which documents `kno eval inspect`, a command on `main` and in no release.

  The stubs are load-bearing rather than tidy. Twenty-two branch-pinned links to
  `github.com/uknoAI/kno/blob/main/docs/cookbook/*.md` live in `uknoAI/kno-www` alone, plus
  however many are in merged PR bodies, issues, and bookmarks — and **nothing in either
  repository would report them 404ing**: `make docs` skips `https://` targets by construction
  and the website's Playwright crawl skips external hrefs. Ledgering that breakage instead would
  have meant a repayment trigger of "when someone reports a dead link", which fires only after
  the damage and cannot lapse observably.

### Bug Fixes

- **`kno export --json` names the Select run it rendered from.** The
  `select_run_id` field was declared in the contract and never populated, so
  it rendered as `""` on every run — including runs given a
  `--select-run-id`. A consumer holding a tuning set could not say which
  measured Portfolio produced it without re-deriving that from the manifest,
  which is the work the field exists to save. `core.ExportResult` now carries
  the ID and the renderer reports it. Found by `uknoAI/kno-examples`, whose
  scenarios assert on projected `--json` subsets.
### Added

- **`kno eval inspect` — whether an eval set can support attribution, before anything is
  spent.** Kno's promise is *this Asset moved this outcome by this much*, and every mechanism
  that delivers it is bounded by the granularity of the eval set. Until now that bound was
  discovered the expensive way: after paying for a baseline and a value run, in the routing
  mode line or in an `UNDERPOWERED` verdict. `kno eval inspect --evals <source>` reports it
  first. It constructs no Agent, resolves no model credential, makes no LLM call, creates no
  Run and writes nothing — the `kno doctor` posture. (A remote source does call its vendor's
  API with the vendor's credentials, because reading the dataset is the job; the help text
  says exactly that.)

  Five checks, each anchored to a constant the engine already uses rather than to a number the
  command invented: `behaviors_declared` (`cluster()`'s all-failed mode),
  `behaviors_powered` (`core.MinClusterCases`), `behavior_concentration`, `holdout_powered`
  (`split.MinHoldout`), and `attribution_observed`, which reports `unknown` without
  `--value-run-id` rather than passing by default. Per behavior it prints the **separable
  effect**: the smallest effect that many dev Cases can distinguish from zero. That number is
  the arithmetic behind `docs/evaluation-design.md`'s "~10+ Cases per behavior" heuristic —
  ten Cases buys the ability to detect a 51-percentage-point swing on a binary Goal, and
  nothing smaller.

  **The exit code is 0 whether zero or five checks are flagged.** It is a diagnostic, not a
  gate; overloading exit 1 ("something is broken") with "your eval set is coarse" trains
  people to ignore 1, which the README's exit-code table warns against. A CI job that wants a
  gate reads `checks_flagged` from `--json`.

  **Three things it deliberately does not claim.** It emits no adjectival grade — the headline
  is a count (`2 of 5 checks flagged`) and each check reports `ok`/`flagged`/`unknown`, the
  three-state discipline `knov1.GapStatus` already uses, because a single word blending five
  checks with five different fixes is the anti-pattern the command exists to find. It reports
  no score decomposition: no Goal in this build populates `Score.components`, so
  "this Goal accounts for 62% of total score" is not computable and is not printed
  (docs/debt.md#131). And the multi-behavior share is **reported and never flagged** — the
  only threshold ever proposed for it was anchored to nothing in the tree, and a tool built to
  refuse invented cut-offs cannot flag on one.

  **The one thing it cannot know, stated once and prominently in both renderings.** A
  behavior, to the engine, is a normalized tag — `cluster()` groups by tag, routing overlaps
  against them, `ComputeGaps` reports one verdict per tag. But `Case.tags` is free-form, so
  `p0`, `regression-2024` and `source:zendesk` are reported as distinct behaviors with
  specific numbers and directive per-tag suggestions, and nothing in the schema distinguishes
  them from a real taxonomy. The output says so above every number that depends on it, carries
  it as `notes[0]` in `--json`, and introduces the suggestions with "If these tags are
  behaviors you would fix separately". Goldens pin both renderings.

  Holdout Cases are counted and never read: the per-Case analysis goes through `core.Seal`,
  while totals come from `CountSplits`, which counts every Case and retains nothing but
  counters. Two canary tests hold that — one plants a holdout Case with a tag that appears
  nowhere else, one fills every Case's input, expected, rubric and turn content with a
  sentinel that must appear in no output at any verbosity.

- **`stats/interval.MinDetectableEffect(n, sidedness, level)`** — the minimum-detectable-effect
  arithmetic, extracted from `core/value` so that both sidednesses come from one
  implementation. `core/value.minDetectableHarm` now delegates to it.

### Changed

- **`docs/status.json` lists command LEAVES, not namespaces.** `kno eval` is the first
  two-level command in the tree and runs nothing on its own, so `registeredCommands` now
  recurses and publishes `eval inspect`. Before this PR every command was a leaf and the
  distinction did not exist; an artifact whose job is to say what a release does must not
  advertise a name a reader cannot run.

- **`Plan.MinDetectableHarm` is computed from the exact Student-t quantile at every degree of
  freedom.** It previously read a 3-4-digit lookup table that stopped at df=31 and fell back
  to `z = 1.645` beyond it. Two consequences, both in the conservative direction. Within the
  table's range the reported bound moves by at most 0.03% — the table's own rounding. Beyond
  df=31 the bound **widens by up to 3%**: at a 40-Case control arm the old code reported
  0.1839 where the true one-sided bound is 0.1884, which understated the smallest regression
  the run could actually see. A characterization test pins the pre-refactor values, asserts
  agreement inside the table's precision, and asserts the direction of the correction past
  df=31, so neither can change again by accident.

## [0.1.2](https://github.com/uknoAI/kno/compare/v0.1.1...v0.1.2) (2026-08-31)


### Features

* kno demo — the whole loop in one command, for free ([#144](https://github.com/uknoAI/kno/issues/144)) ([6317b9a](https://github.com/uknoAI/kno/commit/6317b9ade6d5369a282014bcd67c9619a5ad0bf3))


### Bug Fixes

- **The harm bound is the exact t quantile, and the underpowered gate clears
  where the power actually arrives.** `minDetectableHarm` read a 3-decimal
  table for df≤30 and then fell back to `z = 1.645`, on the reasoning that "t
  reaches z". It does not — t exceeds z at every finite df — so past m=33 the
  reported bound came back **smaller than the truth**: ~3% optimistic at m=33,
  ~1.6% at m=60. The function's own godoc refuses to report an optimistic
  figure; it was reporting one.
  It reached a safety gate. `ControlUnderpowered` is
  `MinDetectableHarm > HarmMargin`, so understating the bound cleared the gate
  early: control arms of **136 and 137** Cases were reported powered when they
  were not — the "underpowered harm test that looks like a passed one" that
  the regression rule exists to refuse. The gate now clears at 138. Runs with
  control samples in that range may newly report `control_underpowered`, which
  is the correct verdict. Found while implementing `kno eval inspect`, whose
  plan mandated reusing this bound two-sided.
- **The rejection log prints interval bounds at four decimal places.** They
  printed with `%v` — all 17 digits — while the value table and the report
  used four. That was false precision on a bound derived from a t-quantile,
  and not merely inconsistent: `math.Exp` and `math.Log` are
  architecture-specific, so the bisection computing the quantile lands one ULP
  apart on arm64 and amd64 and the tail digits genuinely differed by platform.
  `uknoAI/kno-benchmarks` caught it as a cross-platform diff on identical
  inputs. Four places is more precision than the measurement carries and is
  the same on every machine.

* **ci:** the release commit signs itself, and the DCO check stops failing correct trailers ([#142](https://github.com/uknoAI/kno/issues/142)) ([63cfa20](https://github.com/uknoAI/kno/commit/63cfa2087b5b72c882f1aee144e6c241daf59228))


### Documentation

* **adapters:** package godoc names only the adapters that exist ([#141](https://github.com/uknoAI/kno/issues/141)) ([dec47a8](https://github.com/uknoAI/kno/commit/dec47a85a747547c1f2a8f1230194fea2bcb35c2))
* evaluation best practices in the README, and the deep evaluation-design guide ([#137](https://github.com/uknoAI/kno/issues/137)) ([bfb85c7](https://github.com/uknoAI/kno/commit/bfb85c77b3422fa85795dacde85e3bc2c65bb37c))
* Phase-0 plans for the next body of work, all adversarially reviewed ([#140](https://github.com/uknoAI/kno/issues/140)) ([648effb](https://github.com/uknoAI/kno/commit/648effbcd2a4fe401e99eac33d406873500f6699))
* the quickstart tape shows the intervals it claims ([#143](https://github.com/uknoAI/kno/issues/143)) ([1c6403f](https://github.com/uknoAI/kno/commit/1c6403f9c49ab156b79a5e90dad4cd713d265372))
* the README quickstart Case matches the recorded tape ([#139](https://github.com/uknoAI/kno/issues/139)) ([20c7dfe](https://github.com/uknoAI/kno/commit/20c7dfe7a1cc2c27311a20f72aac238d6f9be42b))

## v0.1.2 — in detail

### Added

- **`docs/status.json` — a generated, committed answer to "what does this release do, and how
  honest is it being about the parts it does not do yet?"** Written by `make status`, gated by
  `make status-check` inside `make docs` and therefore inside `make check`. It carries the
  pipeline stages with their shipped/partial/planned state and milestone, the registered command
  tree, the adapter matrix, the goals, the price-table version, and the Debt Ledger's size. The
  website reads it at a release tag, which retires three hand-maintained copies of the same
  claims in `uknoAI/kno-www`.

  **There is no `kno status` command, deliberately.** A command reports the binary in front of
  you; this file reports a release, and they disagree in exactly the cases that matter — a
  `go install`ed dev build reports `version: "dev"`, and a website rendering that would claim a
  version nobody can install. No consumer for the command exists anywhere in the repo, and
  pre-1.0 CLI surface becomes a post-1.0 covenant, so it is deferred behind a named-consumer
  trigger ([docs/debt.md#85](docs/debt.md)) rather than bought on credit. The renderer and the
  stage declaration ship in `cli/status.go` regardless, so reviving it later is a printer over a
  struct that already exists.

  **The artifact carries no version key at all** — not `version`, not `commit`, not
  `released_version`. Its version anchor is the git ref the site fetches it at. A
  `released_version` derived from `.release-please-manifest.json` would have made
  `status-check` fail on *every release PR*, because release-please bumps that manifest in an
  ordinary PR against `main` that runs `make check` — a gate blocking the release it exists to
  describe. The field is deleted rather than automated around, which makes the hazard
  structurally impossible; `TestAReleasePRProducesNoStatusDiff` keeps it that way.

  Which stages have shipped is one human judgement, declared once in `cli/status.go` and
  cross-checked three ways: exhaustively against the `Stage` enum, against the command tree, and
  against README.md. Full derivation is impossible on purpose — the enum carries
  `STAGE_VALIDATE` before `validate` ships, because proto leads implementation.

- **`kno demo` — the whole loop in one command, against `fake:`, for free.** The onboarding
  stage nobody owned: `baseline`, `value`, `select`, `export` and `report` all shipped, and
  what did not ship was the ten minutes before the first one of them runs. `kno demo` writes
  a twelve-Case eval set and a three-Asset pool into `./kno-demo` (embedded in the binary, so
  it needs no checkout and no network), runs all five stages over them in process, and leaves
  the files on disk so the next thing you do is edit them. No run-ID copy step: the run IDs
  are fixed strings the epilogue names.

  Three flags — `--dir`, `--force`, `--json` — and no others. Deliberately **not** accepted:
  `--agent` (a demo against a real provider is a run, and it spends), `--yes` (a bypass flag
  on a free path is one copy-paste away from a paid one), and `--config`. The command reads
  no configuration at all: neither `kno.yaml` nor any `KNO_*` variable, because `KNO_AGENT`
  resolves onto `--agent` and a demo that honored it would bill the user for the privilege of
  being shown around. The budget guard is exercised rather than bypassed — the quote is
  $0.00, which is below the confirmation threshold, so nothing prompts and nothing is waived.

  **The numbers are unimpressive on purpose, and the command says so in three sentences it
  always prints.** `fake:` answers every Case with what the Case expects, so the score reads
  `1.000`; injection delegates to it unchanged, so every delta is `+0.0000` — with real
  intervals around those zeros; and every corrected interval crosses zero, so the Portfolio
  comes back empty, which is the product refusing to recommend something rather than nothing
  happening. `--json` carries the same three sentences in a `notes` array, and a golden pins
  that the two renderings stay in step.

  `--force` deletes an explicit allowlist of demo-owned filenames — never a directory. It
  refuses any directory without the `.kno-demo` marker the demo writes, refuses `--dir .`
  outright, and names any file it leaves in place rather than silently removing it. The demo
  also writes its own `<dir>/.gitignore`, so running it inside your repository does not
  pollute `git status`.

### Changed

- **README's `## Status` is now two tables, Stages and Commands, both test-pinned.** The single
  table had already drifted at one release: it listed **Report** as a stage, where the `Stage`
  enum has no `REPORT` member — `report` composes recorded stages rather than being one — and it
  omitted `init`, `demo`, `mine`, `doctor` and `purge`, all registered commands. Conflating the
  two meanings in one table is *how* that happened, so the tables are split rather than patched.

- **`scripts/ledger-check.py` gains an additive `--json` mode**, reporting the ledger's `total`,
  `open` and `skipped` counts over the *same* row scan and the *same* dispositions the release
  gate uses. The release gate's exit-code behaviour is byte-identical and has a regression test
  saying so. `docs/debt.md` keeps exactly one parser; a Go reader of the same hand-written table
  would have drifted from the Python one silently.

- **`kno --help`'s prose no longer lists which stages run.** It points at the README's Stages
  table, which is now checked, instead of being a fifth hand-maintained copy of the list.

- **`docs/status.json`, once the website reads it, is a consumed contract.** Keys are added,
  never renamed or removed within a major; `schema_version` bumps only on a breaking shape
  change, and the PR that bumps it owes a CHANGELOG entry and a cross-repo issue in the same PR.
  CONTRIBUTING.md carries the protocol and the never-hand-resolve-a-conflict procedure.

## [0.1.1](https://github.com/uknoAI/kno/compare/v0.1.0...v0.1.1) (2026-08-31)


### Features

* Bedrock and Vertex agent adapters — partner clouds priced ([#128](https://github.com/uknoAI/kno/issues/128)) ([360cabc](https://github.com/uknoAI/kno/commit/360cabcd7c9adf9c5d4a467e4d16675beef6eada))
* Braintrust Evals adapter — fourth core.Evals source ([#124](https://github.com/uknoAI/kno/issues/124)) ([638e3d2](https://github.com/uknoAI/kno/commit/638e3d230820359f2c6c5ef15e3e714b3f02ac97))
* Hugging Face adapters — Evals and Pool ([#125](https://github.com/uknoAI/kno/issues/125)) ([ebd4d4a](https://github.com/uknoAI/kno/commit/ebd4d4aa8c28341169d65c8e662bec0c36eaad56))


### Documentation

* fold the hand-written changelog into v0.1.0 ([#131](https://github.com/uknoAI/kno/issues/131)) ([c86e28b](https://github.com/uknoAI/kno/commit/c86e28bd3b61619057cc32378e803bba1e181170))


### Build & Dependencies

* **deps:** Bump github.com/charmbracelet/glamour from 0.9.1 to 1.0.0 ([#136](https://github.com/uknoAI/kno/issues/136)) ([2514b25](https://github.com/uknoAI/kno/commit/2514b25f74f5ce530d80881f1a0d6ba16ed069bc))

## [0.1.0](https://github.com/uknoAI/kno/compare/v0.0.4...v0.1.0) (2026-08-29)


### ⚠ BREAKING CHANGES

* kno v0.1.0 — the measurement loop is complete ([#118](https://github.com/uknoAI/kno/issues/118))

### Features

* csv and markdown pool adapters ([#100](https://github.com/uknoAI/kno/issues/100)) ([5203566](https://github.com/uknoAI/kno/commit/5203566ed94ba6401f9643a48667b24e6cdd97fd))
* exec agent adapter — any executable as an agent ([#99](https://github.com/uknoAI/kno/issues/99)) ([2b97685](https://github.com/uknoAI/kno/commit/2b97685220f5079509e5d912d4ceb5dd7821cb03))
* kno init, the kno.yaml config layer, and the interactive consent prompt ([#102](https://github.com/uknoAI/kno/issues/102)) ([603053b](https://github.com/uknoAI/kno/commit/603053bb0f5d55388ede04a2a1d6819981cb22d0))
* kno mine — turn production transcripts into a weak-label eval set ([#103](https://github.com/uknoAI/kno/issues/103)) ([957ba8f](https://github.com/uknoAI/kno/commit/957ba8fe9d05d45a37db5a007758620a04c2dc03))
* kno report — the one-page verdict across stages ([#115](https://github.com/uknoAI/kno/issues/115)) ([bfee4a2](https://github.com/uknoAI/kno/commit/bfee4a268740334fe65a556a629e8a38b5024330))
* kno v0.1.0 — the measurement loop is complete ([#118](https://github.com/uknoAI/kno/issues/118)) ([0d2b21a](https://github.com/uknoAI/kno/commit/0d2b21a43462019bb6c5475df869333dd9ad9b3d))
* Langfuse Evals adapter — third core.Evals source ([#109](https://github.com/uknoAI/kno/issues/109)) ([a0d1151](https://github.com/uknoAI/kno/commit/a0d11513e8be9cd4950137c9822367ea9a8e5289))
* LangSmith Evals adapter — datasets as first-class Cases ([#98](https://github.com/uknoAI/kno/issues/98)) ([d568a82](https://github.com/uknoAI/kno/commit/d568a823858d5975fac910f61aa6bf7f008cae5a))
* persist the gaps statistic at export — report plan Step 0 ([#111](https://github.com/uknoAI/kno/issues/111)) ([d1900eb](https://github.com/uknoAI/kno/commit/d1900ebb9d8a28e78b4956badbe3d8a1a7531a3b))
* populate Valuation.delta_per_cost in the Value stage ([#101](https://github.com/uknoAI/kno/issues/101)) ([417cf03](https://github.com/uknoAI/kno/commit/417cf0389436ff11ba84d5b1abfeb91cdfd0734d))
* price the fast-mode variants; dispose the rest of debt [#46](https://github.com/uknoAI/kno/issues/46) with reasons ([#91](https://github.com/uknoAI/kno/issues/91)) ([a67ccbb](https://github.com/uknoAI/kno/commit/a67ccbb98e54108db858b3ef8160daf0d28fa577))
* pricing drift detector — watch the table for staleness, repricing, and unpriced variants ([#87](https://github.com/uknoAI/kno/issues/87)) ([eb5dcd0](https://github.com/uknoAI/kno/commit/eb5dcd095bb2de17633cd8e9c7c71757903c245d))
* select and export — portfolio construction and destinations ([#107](https://github.com/uknoAI/kno/issues/107)) ([e6b8038](https://github.com/uknoAI/kno/commit/e6b803816fecbb3c978c83bff531d02c665d9ef3))


### Bug Fixes

* cli wiring tests manufacture credential absence instead of relying on the scrub ([#90](https://github.com/uknoAI/kno/issues/90)) ([0dcad28](https://github.com/uknoAI/kno/commit/0dcad2895e60302ec93f52d5c7b5cf6b224dfa9a))
* pricing-check workflow bootstraps its own label ([#92](https://github.com/uknoAI/kno/issues/92)) ([66d55c5](https://github.com/uknoAI/kno/commit/66d55c51316ae47d7c82934b4f9258d6c842f9bb))
* **value:** inline the routing shuffle so a recorded seed outlives the binary ([#108](https://github.com/uknoAI/kno/issues/108)) ([0987d96](https://github.com/uknoAI/kno/commit/0987d964a2430bd63443c4ce4559b37a005f3115))


### Documentation

* extend the quickstart tape through select, export, and report ([#116](https://github.com/uknoAI/kno/issues/116)) ([9aaf1f9](https://github.com/uknoAI/kno/commit/9aaf1f99ff5913e3dc04af2d381a31a666f23a86))
* ledger the parquet deferral with a trigger that can lapse ([#104](https://github.com/uknoAI/kno/issues/104)) ([9b084ed](https://github.com/uknoAI/kno/commit/9b084ed22f483703ce3b6d12005dffcc7181a781))
* n8n cookbook entry — scheduled valuation with alerts ([#97](https://github.com/uknoAI/kno/issues/97)) ([13ab363](https://github.com/uknoAI/kno/commit/13ab3636477c1e4d21933e101ea7e84234bcc389))
* plan the Braintrust, Hugging Face, and Bedrock/Vertex adapters ([#119](https://github.com/uknoAI/kno/issues/119)) ([5423116](https://github.com/uknoAI/kno/commit/542311608a4159222a7b58de06bfafd08365685c))
* plan the Langfuse Evals adapter ([#106](https://github.com/uknoAI/kno/issues/106)) ([9775b16](https://github.com/uknoAI/kno/commit/9775b161673dc8fbe4362e6396730c432dd23113))
* plan the report page and minimal TUI dashboard ([#105](https://github.com/uknoAI/kno/issues/105)) ([f03c8a8](https://github.com/uknoAI/kno/commit/f03c8a8bb74795a364f55b3d8c2940f79c4ae448))
* record the v0.1 bump-policy decision — feat: keeps bumping patch ([#113](https://github.com/uknoAI/kno/issues/113)) ([2cf3b72](https://github.com/uknoAI/kno/commit/2cf3b72ac61d025599d2b0c8ae5e538921bc36d1))
* rework the README around the data-valuation pitch ([#94](https://github.com/uknoAI/kno/issues/94)) ([4144be6](https://github.com/uknoAI/kno/commit/4144be63eb7d70c905682f4dc5a4a9eb2ccc7a94))
* sweep the cookbook against the v0.1 CLI; add the Anthropic entry ([#117](https://github.com/uknoAI/kno/issues/117)) ([60d7608](https://github.com/uknoAI/kno/commit/60d7608e0eac7a2c8ae06e553257e25d5f2735a3))
* the v0.1 ledger review — dispose four lapses, amend stale rows ([#114](https://github.com/uknoAI/kno/issues/114)) ([b63079d](https://github.com/uknoAI/kno/commit/b63079d262617a441384ba85d5bff0e36b0f5c63))
* vendor cookbook entries — GitHub, Salesforce, HubSpot, Shopify, Stripe, Jira, Confluence, Notion ([#96](https://github.com/uknoAI/kno/issues/96)) ([b2fcd98](https://github.com/uknoAI/kno/commit/b2fcd982c3499d50aeea055a22075ee87f5392ac))
* Zendesk cookbook recipe and the vendor-swap reference table ([#95](https://github.com/uknoAI/kno/issues/95)) ([83a5c91](https://github.com/uknoAI/kno/commit/83a5c918f479d645c65d2a43b4364158d26844d3))


### Build & Dependencies

* **release:** re-enable the Homebrew brews block — tap and token are live ([#112](https://github.com/uknoAI/kno/issues/112)) ([121440d](https://github.com/uknoAI/kno/commit/121440de23545a42decd09eff2b9a6cfd454815e))

## [0.0.2](https://github.com/uknoAI/kno/compare/v0.0.1...v0.0.2) (2026-08-27)


### Features

* ship the Value stage — routing, paired measurement, kno value ([#66](https://github.com/uknoAI/kno/issues/66)) ([1d96ce9](https://github.com/uknoAI/kno/commit/1d96ce9f82b3abe9d9bb12fe72861fca8c26c342))


### Bug Fixes

* cosign legacy sign format, store coverage determinism ([#68](https://github.com/uknoAI/kno/issues/68)) ([62f7174](https://github.com/uknoAI/kno/commit/62f7174a8dce19bf1916fae5364241a89a2dd9cb))

## [0.0.4](https://github.com/uknoAI/kno/compare/v0.0.3...v0.0.4) (2026-08-28)

### Fixed

- **The release pipeline now self-heals its own state after publishing** — the changelog
  fold and the release-please manifest reconciliation happen in one post-publish PR, so a
  manifest that goes stale (as it did after 0.0.3) cannot survive the next release.
- **A failed release build no longer leaves a published empty release page** — the release
  workflow removes exactly the published-but-empty shape before building (debt #81 repaid).

### Features

- **The README quickstart GIF is recorded** — `make tape` output committed as
  `docs/quickstart.gif` and embedded in the README (debt #64 repaid).
- **The changelog fold is automated** — the release pipeline renames the hand-written
  `[Unreleased]` heading to the release it shipped in and opens an auto-merging PR
  (debt #76 repaid).
- **Value's mid-run model gate arms on the first process** from the baseline's recorded
  models, not only on resumes — an alias re-pointing mid-run is refused in the very run it
  happens in (debt #80 repaid).
- **The Homebrew tap is live.** `brew tap knograph/tap && brew install kno` installs a
  SHA-256-pinned, release-built binary; goreleaser updates the formula on every release.

## [0.0.2](https://github.com/uknoAI/kno/compare/v0.0.1...v0.0.2) (2026-08-27)


### Features

* ship the Value stage — routing, paired measurement, kno value ([#66](https://github.com/uknoAI/kno/issues/66)) ([1d96ce9](https://github.com/uknoAI/kno/commit/1d96ce9f82b3abe9d9bb12fe72861fca8c26c342))


### Bug Fixes

* cosign legacy sign format, store coverage determinism ([#68](https://github.com/uknoAI/kno/issues/68)) ([62f7174](https://github.com/uknoAI/kno/commit/62f7174a8dce19bf1916fae5364241a89a2dd9cb))

## v0.1.0 — in detail

### Features

- **The Braintrust Evals adapter — the fourth `core.Evals` source.**
  `--evals braintrust:<dataset-name>` reads a Braintrust dataset with the
  same split, fingerprint, and weak-label machinery the other sources use.
  Name resolution is the `dataset_name` filter endpoint (a miss is a loud
  refusal naming the dataset, never an empty run); events stream paged with
  the opaque cursor; duplicate ids from the version-history walk are merged,
  keeping the newest `_xact_id`, not fatal; the resume fingerprint folds in
  the dataset id, name, and the newest event's `_xact_id` (Braintrust
  carries no dataset-level revision). An event copied from another object
  (`origin` set) is marked derived per item, matching Langfuse's per-item
  weak-label semantics. Credentials are environment-only:
  `BRAINTRUST_API_KEY`, with `BRAINTRUST_API_BASE_URL` and
  `BRAINTRUST_ORG_NAME` as the self-hosted/org selectors, and the same
  endpoint-security refusals and opt-in flags as the other dataset adapters.
  [docs/plans/2026-08-29-braintrust-evals-adapter.md](docs/plans/2026-08-29-braintrust-evals-adapter.md)

### Features

- **Hugging Face Evals and Pool adapters.** `--evals hf:<org>/<name>/<config>/<split>`
  reads a dataset split as Cases — `input`/`prompt`/`question` maps to the input,
  `expected`/`completion`/`answer` to the expected, `row_idx` to the Case id —
  and `--pool hf:<org>/<name>/<config>/<split>:<kind>` reads the same rows as
  Assets, text-bearing columns composed as sorted `name: value` lines. The
  datasets-server `x-revision` header is the fingerprint: a split whose
  revision moves between pages, or between runs, is a different object and is
  refused, never silently absorbed. The two adapters share a transport client
  (`adapters/internal/datasetserver`) and the shared endpoint-security checks
  (`adapters/internal/endpointsec`) that the LangSmith and Langfuse adapters
  each carried their own copy of. Docs: [cookbook entry](docs/cookbook/huggingface.md),
  [adapter plan](docs/plans/2026-08-29-huggingface-adapters.md), `docs/debt.md#68` fired
  (the HF pool carries the same byte-based `context_tokens` estimate as the
  markdown and CSV pools, acknowledged on the field and in
  what-the-numbers-mean).

### Changed

- **The routing shuffle is inlined, and the seed's meaning is now specified.**
  The Value stage's routing draw and label draw previously called
  `rand.Rand.Shuffle`, whose consumption pattern and bounded-draw loop carry
  no compatibility promise across Go releases — while the Run's recorded seed
  exists to be re-derived years later. The draw is now an inlined
  Fisher-Yates over the raw PCG stream with an owned bounded draw
  (`core/value/shuffle.go`), pinned by a golden permutation test. The stream
  changed once, at this release: seeds recorded by earlier releases
  re-derive only under the earlier binary, and the `Seed` godoc states the
  boundary instead of pretending otherwise. `docs/debt.md#75` repaid.

### Migration notes

- **A checkpointed Value run recorded before v0.1.0 cannot be resumed under
  v0.1.0.** The routing draw's stream changed with the #75 repayment below;
  the recorded plan's sampled IDs can never match the recomputed plan, and
  the resume refusal's fix line says so. Re-run instead; no money is lost —
  the settled spend is checkpointed, and a fresh run starts from the recorded
  baseline.
- **The store schema moves to version 5** (additive): a new `gaps` table
  records the per-cluster improvement verdicts each Export run computes,
  keyed by the Export run that produced them. Runs recorded before this
  version simply have no gaps row — the report reads that as "no cluster
  data for this run" rather than guessing. Existing databases upgrade in
  place on open; no data is rewritten.
- **The store schema moves to version 4** (additive): a new `portfolios`
  table records the Select stage's output, one row per Select run. Existing
  databases upgrade in place on open; no data is rewritten.

### Features

- **Bedrock and Vertex agent adapters — the partner clouds, priced.** `kno`
  can now measure any Claude model behind AWS Bedrock (`bedrock:anthropic.claude-sonnet-4-5-20250929-v1:0`) and Google Vertex AI (`vertex:claude-sonnet-4-5`) with the full Agent surface — Invoke, Capabilities, estimate-before-spend with WorstCase, context injection, token-count settlement against the provider's reported usage. The JWT-to-access-token exchange (Vertex) and SigV4 (Bedrock) are stdlib implementations; credentials come from the environment only (AWS_ACCESS_KEY_ID/SECRET/SESSION_TOKEN/REGION, or GOOGLE_APPLICATION_CREDENTIALS), never from kno.yaml, fixtures, or logs. Both schemes reach a fixed regional endpoint: a base URL is refused at parse, and the insecure/private-address/seed flags do not apply. The table prices the bedrock/vertex namespaces, and the regional +10% multiplier is a committed constant in the estimate and settlement path (docs/debt.md#41(d) repaid); `us.`/`eu.` cross-region inference profiles are refused until priced. pricingcheck gained check 6: AWS's machine-readable Bedrock price list confirms the 110% multiplier every run; vertex has no machine-readable source and is reported every run. Plans: `docs/plans/2026-08-29-bedrock-vertex-agents.md`.
- **Export persists the gaps statistic the report will render** (the report
  plan's Step 0). The Value run's plan now carries a `Clusters` snapshot —
  the failure clusters routing already computed, frozen at planning time,
  dev-only by construction (routing never sees the holdout; a canary test
  plants a holdout Case in the source and pins that it reaches neither the
  routing nor the snapshot). `kno export` reads the source plan, computes a
  per-cluster verdict — IMPROVED when a covering Asset's delta CI excludes
  zero, GAP when well-covered and nothing is significant, UNKNOWN when
  nothing routed to enough of the cluster's Cases or the covering measurement
  is underpowered — and persists it keyed by the Export run. Non-significance
  is not absence: the reported number per cluster is the best covering
  Asset's CI, never a cluster-level threshold game, and multiple testing is
  labeled. Old plan blobs decode with an empty snapshot (gob is
  append-tolerant) and record no row. Plan:
  `docs/plans/2026-08-29-report-tui.md` (Step 0).
- **`kno report` composes the recorded stages into one page** — the report
  plan's Step 1. The Baseline it was measured against (with its score),
  every Asset's verdict with its interval, the Portfolio Select built (dev
  estimate with its interval and the mandatory "not yet validated on
  holdout" caveat, rejection log folded by reason), and the gaps Export
  recorded — or the honest absent-answer, "no cluster data for this run",
  never a guess. The page reads recorded aggregates only: no LLM calls, no
  evals re-read, no trace content. A Baseline that Value's own rules would
  refuse (error rate exceeded, blended models) is refused here too, with the
  fix, before a page can compose around it. `--watch` re-renders every 2
  seconds while the Value run is not terminal and exits 0 the moment it is;
  it needs a terminal and cannot combine with `--json`. The `--json`
  contract is hand-written (ADR-0001) and golden-pinned to the human page:
  two renderers, one composed snapshot. Rendered through glamour — glow
  v1.5 exports no library API, so the report uses the engine glow itself
  renders through, MIT, ~20 charmbracelet modules. Plan:
  `docs/plans/2026-08-29-report-tui.md` (Step 1).


- **Select and Export close the measurement-to-destination loop.** `kno select`
  builds a Portfolio from a recorded Value run: greedy on delta-per-cost,
  honestly labeled (feasible, deterministic, reproducible — no approximation
  guarantee), with every keep/reject decision at a Bonferroni-corrected
  interval and the rejection log in precedence order (regression, no-effect,
  redundant, cost-dominated, wrong-mechanism). The net-loss judgement
  combines treatment and control deltas under the shared-baseline covariance
  (`stats/portfolio`), and a regression verdict is gated on a powered control
  arm. A budget-stopped source run is refused unless `--allow-partial`, and
  the source's status travels with the Portfolio either way; "include nothing
  new" is a legal, first-class outcome. The portfolio-level gain is one
  corrected claim, winner's-curse inflation stated in the field's name and
  godoc. `kno export` renders a Portfolio's selected assets into the
  destination grammar — `context` (pack + manifest), `knowledge_base`
  (manifest + instruction list), or `tuning_set` (OpenAI chat format JSONL,
  the shape the Tuner adapters parse) — refusing an existing target unless
  `--force`, writing temp-then-rename, and re-exporting byte-identically.
  Export never mutates a destination. Debt #65/#66/#67 repaid; #78 split —
  pairing-scheme recording paid, measurement design re-dated to the first
  writable Destination adapter. Plan: `docs/plans/2026-08-29-select-export.md`.


- **Langfuse datasets are a third `core.Evals` source.** `kno baseline --evals
  langfuse:my-dataset` measures against a Langfuse dataset directly, alongside JSONL and
  LangSmith. The adapter resolves the dataset by name first (a typo is refused loudly
  before anything is fetched), streams items with page-numbered pagination (100/page,
  Retry-After-aware 429 backoff, page cap), keeps prompts and expectations as canonical
  JSON (key-sorted, numbers preserved literally — golden-pinned), filters ARCHIVED items
  client-side, and marks trace-harvested items derived per item
  (`sourceObservationId`/`sourceTraceId`), so the run's weak-label count reflects the
  Langfuse dataset's actual mix rather than LangSmith's wholesale marking. Credentials
  are the documented basic-auth pair, `LANGFUSE_PUBLIC_KEY` + `LANGFUSE_SECRET_KEY`,
  environment-only; the endpoint security checks (scheme, userinfo, private/link-local
  refusal, dial-time recheck, redirect refusal) are ported verbatim from the LangSmith
  adapter, deliberately unshared. Fixtures are hand-authored against the documented
  schema with provenance notes (no live key on the build machine;
  `KNO_RECORD_FIXTURES=1` re-records when one exists). Plan:
  `docs/plans/2026-08-29-langfuse-evals-adapter.md`.
 (feat: Langfuse Evals adapter — third core.Evals source)

- **LangSmith datasets are first-class Evals.** `kno baseline --evals
  langsmith:my-dataset` measures against a LangSmith dataset directly — no manual export,
  no snapshot rot. The adapter streams examples with cursor pagination (page 100,
  Retry-After-aware backoff, page cap), maps rows deterministically (named keys first,
  then document order — never Go's randomized map order), handles chat-format and
  LLM-format datasets, refuses plain-HTTP and private-address self-hosted endpoints by
  default, and redacts credentials from every error. Fixtures are hand-authored against
  the documented schema with provenance notes (no live key on the build machine;
  `KNO_RECORD_FIXTURES=1` re-records when one exists). The dev/holdout split moved to a
  shared `adapters/evals/split` package, so the denominator math is compile-time
  identical across sources, and `coretest` gained a duplicate-Case-ID conformance check
  that makes the debt-45 invariant a core property. Plan:
  `docs/plans/2026-08-28-langsmith-evals-adapter.md`.

- **Fast-mode variants are priced, and the rest of the variant debt is disposed with
  reasons.** `claude-opus-5-fast` and `claude-opus-4-8-fast` have rows at the published
  $10/$50 per MTok, so a capped run on fast mode is authorized at its real rate instead of
  being refused once, up front — `docs/debt.md#46` repaid on its trigger. The batch
  variants (published at 50% of base) stay deliberately unpriced because no adapter speaks
  the batch endpoint: a row would authorize a run the Messages endpoint rejects per Case,
  which is worse than the one-time refusal. The OpenRouter-only variants
  (`gpt-5.6-*-pro`, `claude-opus-4-7-fast`) stay unpriced because no price-of-record page
  publishes them. The detector keeps every one of these exclusions honest — it fails the
  run the day any of them gains a row or a published rate — and now selects the fast table
  by its own header literal, so the two new rows are cross-checked rather than
  single-sourced. `pricing.Version` moves to 2026-08-28.

- **The pricing drift detector repays the staleness debt.** `internal/cmd/pricingcheck`
  fetches the three price-of-record sources (OpenRouter's model list, Anthropic's pricing
  page, OpenAI's model comparison page), compares them against the committed table, and
  files deduplicated `pricing-drift` issues that close themselves when the finding is gone.
  Six checks: the table's age (the 90-day trigger of `docs/debt.md#40`, enforced for the
  first time by machine rather than by a date on a row), Anthropic table selection by a
  committed header literal, the providers' published cache-ratio invariants, cross-source
  agreement, discovery, and prefix-colliding variants — the last one reporting the 16 owed
  `docs/debt.md#46` rows every run until they land. The weekly scheduled job holds no write
  token beyond `issues: write`; every check is fixture-tested against real captures with
  provenance, and every error path was verified failing before the check that catches it
  was written. `make pricing-check` runs it live locally; it deliberately is not part of
  `make check`, which stays network-free for PR CI.

### Bug Fixes

- **CLI wiring tests no longer inherit ambient API keys under live mode.** The
  nightly live job was armed with real credentials for the first time, and two
  wiring tests broke: they assert the no-credential refusal, but the `cli`
  TestMain env scrub — which used to guarantee a credential-free process — is
  skipped when `KNO_LIVE_TESTS=1`, so the keys leaked in and the refusal never
  fired. The tests now manufacture ABSENCE themselves (`withoutEnv`), not
  emptiness: the refusal's fix line names the variable to export only when
  nothing is bound to it. Serial, because the CLI runs in-process and reads the
  process env.
- **The pricing-check workflow bootstraps its own label.** Its first post-merge
  run failed with "could not add label: 'pricing-drift' not found" — the label
  had never been created on the repo, and `gh issue create` treats a missing
  label as fatal. The job now creates the label if absent before the lifecycle
  walk, so a fresh repo or a deleted label cannot red the job for an infra gap
  the workflow can close itself.

## 0.0.2 — in detail

### Features

- **`kno value` ships the Value stage.** Each Asset in a pool is routed to the Cases it could
  plausibly affect, injected into the agent's context for the treatment arm, re-measured without it
  for the control, and reported as a delta with its confidence interval — or a named reason why no
  number exists. The harm test over the reserved control slice reports the smallest regression the
  sample could have seen rather than a boolean, and the underpowered flag travels beside it.
- **Money is attributable to the Asset that caused it.** Retry and settlement-overshoot events carry
  the (Asset, arm, trial) measurement key, so the API can answer "what did asset X cost in retries".
- **Resume never re-pays.** Measurements are checkpointed as they complete — budget refusals
  included — and a resumed run consumes the recorded routing plan, refusing a drifted configuration.

### Fixed

- **The pinned cosign identity regexp was case-sensitive** (`knograph/kno`) and never matched the
  certificate's SAN (`Knograph/kno`, the repo owner's case) — the workflow's own documented
  verification command would have rejected its own releases. It now accepts either case, in all
  five places, and the first verification of a real release ran it end to end.
- **The release sign step now uses cosign's bundle format end to end** — the legacy-format pin
  died against cosign 2.4+'s validation (`must provide --new-bundle-format or --bundle`), so the
  config writes one bundle per signature, and install.sh plus the published verification commands
  verify with `--new-bundle-format --bundle`.
- **The first tag's release build failed at signing** — cosign's new-bundle-format silently
  ignores the `--output-signature`/`--output-certificate` flags and demands `--bundle`, so the
  sign step died with `create bundle file: open : no such file or directory`. The sign config now
  pins `--new-bundle-format=false`, the legacy format every published verification command
  exercises.
- **The store package's coverage gate flapped** on the migration re-check branch, which only
  executes when a concurrent open loses the lock race. It is now driven deterministically, and
  the store baseline moved to the lowest stable cross-platform reading.
- The Value stage's estimator was adversarially reviewed before it ever ran: Goal direction is
  applied exactly once (a MINIMIZE goal that got slower now reports a negative delta), deltas ship
  only beside their intervals, and the harm bound consumes per-Case means rather than the flattened
  per-trial shape that narrowed it by sqrt(trials) in the direction that clears harmful assets.

## 0.0.1 (2026-08-27)


### ⚠ BREAKING CHANGES

* **proto:** none at the wire level -- every change is additive and buf breaking passes. Flagged because AgentRef gains a documented grammar (@base-url) that adapters and the CLI must parse consistently from M2-4 onward.

### Features

* **adapters:** carry an Asset in the prompt, for context-injection measurement ([#56](https://github.com/uknoAI/kno/issues/56)) ([a392923](https://github.com/uknoAI/kno/commit/a392923a6267841e1a98b06492acf05b60f65429))
* **adapters:** read Assets from a JSONL pool ([#55](https://github.com/uknoAI/kno/issues/55)) ([db90e92](https://github.com/uknoAI/kno/commit/db90e92ed0d1a712c3a62a84edff57cabbb4f5c0))
* **agentref:** M2-4 — the agent-ref grammar, with the repo's first fuzz target ([#31](https://github.com/uknoAI/kno/issues/31)) ([5fcaacf](https://github.com/uknoAI/kno/commit/5fcaacfa06b8424ad1cde6d07ca8520c2a9a51d7))
* **anthropic:** the Messages API adapter ([#37](https://github.com/uknoAI/kno/issues/37)) ([e05810b](https://github.com/uknoAI/kno/commit/e05810bb0a9c5bc1fd249347d9fd20015c7081c7))
* **build:** enforce coverage floors and godoc coverage ([#9](https://github.com/uknoAI/kno/issues/9)) ([bf924d2](https://github.com/uknoAI/kno/commit/bf924d2e72de1b5edf1ca2d9685f29d0c8f11f69))
* **cli:** kno baseline, the first user-facing command ([#22](https://github.com/uknoAI/kno/issues/22)) ([8911c0a](https://github.com/uknoAI/kno/commit/8911c0a5158cbf9686ad21cb2422c255b4cd9735))
* **cli:** reach a real provider, and refuse to spend a number nobody can state ([#50](https://github.com/uknoAI/kno/issues/50)) ([fabefca](https://github.com/uknoAI/kno/commit/fabefca788ee42d1f2deeb36e7f8bbfeae42cb7d))
* **core:** emit the concurrency decision and a progress heartbeat ([#42](https://github.com/uknoAI/kno/issues/42)) ([1673d41](https://github.com/uknoAI/kno/commit/1673d41460b71acf0cb779af2688cc2977526296))
* **core:** end a run on a failure that cannot change within it ([#49](https://github.com/uknoAI/kno/issues/49)) ([78268c0](https://github.com/uknoAI/kno/commit/78268c0599cd40882de7f27acfc5159d25f066c5))
* **core:** M2-2 — per-Case cost estimation and an observable cap breach ([#29](https://github.com/uknoAI/kno/issues/29)) ([4c69810](https://github.com/uknoAI/kno/commit/4c69810b9c61b2713436ae95097334e266076274))
* **core:** M2-6 — the spend path a real provider call takes ([#33](https://github.com/uknoAI/kno/issues/33)) ([a7b9112](https://github.com/uknoAI/kno/commit/a7b91123877c88e119aad36e3af3b73b8950d2dd))
* **core:** route Assets to Cases, and reserve the controls before routing runs ([#60](https://github.com/uknoAI/kno/issues/60)) ([6226d11](https://github.com/uknoAI/kno/commit/6226d117dc6a198adc7fc8daf3f10806d05b3f1f))
* **core:** the Baseline stage, running end to end ([#21](https://github.com/uknoAI/kno/issues/21)) ([9fca190](https://github.com/uknoAI/kno/commit/9fca1907dbf1b3353a91e2a6a7b9a2eff79382b0))
* **core:** the event spine — sequence discipline and RunResumed ([#40](https://github.com/uknoAI/kno/issues/40)) ([c27ea03](https://github.com/uknoAI/kno/commit/c27ea0301215bf67ab5f48f304f082b0ec7d12f0))
* **core:** the Ring-0 contracts, error grammar, and iterator conformance harness ([#10](https://github.com/uknoAI/kno/issues/10)) ([4893379](https://github.com/uknoAI/kno/commit/4893379128dc8f5e5e82af5960e2191fff58874e))
* **core:** the spend path, whole ([#43](https://github.com/uknoAI/kno/issues/43)) ([fbdaf46](https://github.com/uknoAI/kno/commit/fbdaf469aba1b5f4131f71c926e7458a968f629f))
* **core:** write CaseExecution from what is durably recorded ([#46](https://github.com/uknoAI/kno/issues/46)) ([6bb14a8](https://github.com/uknoAI/kno/commit/6bb14a8bda39e7f88426e90ef911513843f0d062))
* **evals:** JSONL adapter, deterministic split, and the holdout seal ([#19](https://github.com/uknoAI/kno/issues/19)) ([1560ce3](https://github.com/uknoAI/kno/commit/1560ce32a2b258777af383397ceb9dd7e6420863))
* **executor:** bounded worker pool with a written shutdown protocol ([#20](https://github.com/uknoAI/kno/issues/20)) ([f9b62d2](https://github.com/uknoAI/kno/commit/f9b62d2f117e7740cc2ca4a6a8925f54796d95b4))
* **observe:** trace every run, Case, and provider call without carrying content ([#51](https://github.com/uknoAI/kno/issues/51)) ([13171e4](https://github.com/uknoAI/kno/commit/13171e4df675ca2a0c5021bcdd87d12abc356cc1))
* **openaicompat:** the first provider adapter ([#36](https://github.com/uknoAI/kno/issues/36)) ([d0bf7f1](https://github.com/uknoAI/kno/commit/d0bf7f15bd3d7ab5e0b8c15a3778952528d60bfb))
* **proto:** add Goal Direction so the sign of every reported number is interpretable ([#8](https://github.com/uknoAI/kno/issues/8)) ([e5d4b98](https://github.com/uknoAI/kno/commit/e5d4b98780dfbd9159757cbd16161e1a846f2a65))
* **proto:** ConcurrencyReduced and Run.scheduling ([#41](https://github.com/uknoAI/kno/issues/41)) ([b5f49e4](https://github.com/uknoAI/kno/commit/b5f49e434ab08d73e5a580cb72989f4b01c43c03))
* **proto:** M2-0 — the schema for real provider adapters ([#25](https://github.com/uknoAI/kno/issues/25)) ([35f77a7](https://github.com/uknoAI/kno/commit/35f77a7b834e7c8bf6fccb66352f66721b84ddc8))
* **proto:** report an overshoot's contribution, name a billed retry, and attribute orphaned spend ([#45](https://github.com/uknoAI/kno/issues/45)) ([042eefd](https://github.com/uknoAI/kno/commit/042eefdfc31631f0bdcc387879c663d5ac0fc631))
* **proto:** Run and the event spine ([#17](https://github.com/uknoAI/kno/issues/17)) ([0db0990](https://github.com/uknoAI/kno/commit/0db0990469cda84bd755b701ff8f9fef767f16b1))
* **proto:** the kno.v1 contract — Ring-0 schema and generated types (M0b) ([#2](https://github.com/uknoAI/kno/issues/2)) ([8361e2a](https://github.com/uknoAI/kno/commit/8361e2a4b69fffcbad25f9550f3c3b5470d3c2e5))
* **proto:** the wire contract for the Value stage ([#53](https://github.com/uknoAI/kno/issues/53)) ([02e8809](https://github.com/uknoAI/kno/commit/02e8809c6e1f75afafbfa037cb61a994c8442a1b))
* **stats:** confidence intervals on paired differences ([#54](https://github.com/uknoAI/kno/issues/54)) ([938202b](https://github.com/uknoAI/kno/commit/938202bdfc8f5309e2c4a74c2d99aa7b53512e77))
* **stats:** the budget guard ([#14](https://github.com/uknoAI/kno/issues/14)) ([2f0562e](https://github.com/uknoAI/kno/commit/2f0562e2f534a4596416340c7815dd4c2d83f377))
* **store:** hold a Value run, and stop the readers double-spending on it ([#57](https://github.com/uknoAI/kno/issues/57)) ([52fed2d](https://github.com/uknoAI/kno/commit/52fed2db8583793286b62094e49aa072609698a2))
* **store:** M2-1 — schema migration, outcome columns, and kno purge ([#27](https://github.com/uknoAI/kno/issues/27)) ([68fdfaa](https://github.com/uknoAI/kno/commit/68fdfaab5ada57da37f421572a966dab4333f59a))
* **store:** SQLite persistence for runs, outcomes, and events ([#18](https://github.com/uknoAI/kno/issues/18)) ([da0b95b](https://github.com/uknoAI/kno/commit/da0b95b3ee5392ff6130916d1d6939f8e196a642))
* **transport:** M2-3 — the shared HTTP layer and its security boundary ([#30](https://github.com/uknoAI/kno/issues/30)) ([eda666e](https://github.com/uknoAI/kno/commit/eda666ed4eefff84558fa5272fa77de0b370fcea))


### Bug Fixes

* **build:** guard record-fixtures against unmetered live spend ([#23](https://github.com/uknoAI/kno/issues/23)) ([976a42b](https://github.com/uknoAI/kno/commit/976a42bc8dd7e4a0f547849c83f5f693be1736f1))
* **build:** make gate recipes enforce failure without relying on .SHELLFLAGS ([#7](https://github.com/uknoAI/kno/issues/7)) ([199ea63](https://github.com/uknoAI/kno/commit/199ea63d8b3f50924d92a8543d18f7066c6f31eb))
* **ci:** make the coverage baseline the floor across platforms ([#26](https://github.com/uknoAI/kno/issues/26)) ([b029d55](https://github.com/uknoAI/kno/commit/b029d55948d7e180bdaa893ab70dba19220b6589))
* **ci:** one toolchain, a correct cache key, and actions off Node 20 ([#11](https://github.com/uknoAI/kno/issues/11)) ([c9fa189](https://github.com/uknoAI/kno/commit/c9fa1898e0ff2d0de1946e48fbe595cbc38654ae))
* **core:** quote a resumed run against the money it has left ([#39](https://github.com/uknoAI/kno/issues/39)) ([ecd8b68](https://github.com/uknoAI/kno/commit/ecd8b682b0e8d164a87a397e26ebfa7e03266d9f))
* **executor:** bound the drain after cancellation, not the run ([#47](https://github.com/uknoAI/kno/issues/47)) ([e63628f](https://github.com/uknoAI/kno/commit/e63628f72c0f44252215975d3bf53910062dc9a1))
* **pricing:** refuse a variant suffix rather than pricing it as its base ([#35](https://github.com/uknoAI/kno/issues/35)) ([0d49f0d](https://github.com/uknoAI/kno/commit/0d49f0d7e7e08b6776277b5f7dfc119fde563274))
* report a resumed run's mean over the whole run ([#34](https://github.com/uknoAI/kno/issues/34)) ([8a30dbe](https://github.com/uknoAI/kno/commit/8a30dbe2fee27930249fd1a349ffaf764b600a26))
* **stats:** restore budget state on resume ([#16](https://github.com/uknoAI/kno/issues/16)) ([1cc8ffc](https://github.com/uknoAI/kno/commit/1cc8ffc2a1f0e30a745f9a6694b9a4d9759c3844))
* **store:** make spend durable when a Case never produces an outcome ([#44](https://github.com/uknoAI/kno/issues/44)) ([937294f](https://github.com/uknoAI/kno/commit/937294fb71df0349445de94f11286a56766d5708))
* **store:** two kno processes opening one database could fail to start ([#28](https://github.com/uknoAI/kno/issues/28)) ([acdc9b1](https://github.com/uknoAI/kno/commit/acdc9b146ac7dbf21c35ab072f32e2c4fd6bb074))


### Refactoring

* **core:** split baseline.go along its seams ([#38](https://github.com/uknoAI/kno/issues/38)) ([2a149ca](https://github.com/uknoAI/kno/commit/2a149ca78372b6116ed8c78ec4f9943909a6cd10))


### Documentation

* initial repo scaffold with design and operating manual ([b5ea55c](https://github.com/uknoAI/kno/commit/b5ea55c861f97d274d90e6cd1d151436fa699906))
* plan M2-11, the PR that makes the adapters reachable ([#48](https://github.com/uknoAI/kno/issues/48)) ([b7984f2](https://github.com/uknoAI/kno/commit/b7984f21415a5aa96b943ec221ee60c884b93459))
* plan the Value stage ([#52](https://github.com/uknoAI/kno/issues/52)) ([3ced7e0](https://github.com/uknoAI/kno/commit/3ced7e04243ac516a93c592bfef3b47b00ccbdc3))
* **plan:** M1 Baseline plan, amended after Phase-1 review ([#15](https://github.com/uknoAI/kno/issues/15)) ([c854f0a](https://github.com/uknoAI/kno/commit/c854f0aaf0bb1591c8fc1f091be1a0c0d4300771))
* **plans:** M2 — the first real provider adapter ([#24](https://github.com/uknoAI/kno/issues/24)) ([9bdc951](https://github.com/uknoAI/kno/commit/9bdc95120b39f3da604bff55c4dee928bab18d31))
* stop the 0.0.1 changelog shipping under an Unreleased heading ([#63](https://github.com/uknoAI/kno/issues/63)) ([7a2d94f](https://github.com/uknoAI/kno/commit/7a2d94fd7e7d7958a5c1a5ae4976e845f7b5d63d))


### Build & Dependencies

* repository foundation — gates, governance, and process machinery (M0a) ([#1](https://github.com/uknoAI/kno/issues/1)) ([fb55946](https://github.com/uknoAI/kno/commit/fb559461eac9481fe0be4242e31eb1b6f6e97a53))
* sign, attest and publish what a tag produces ([#59](https://github.com/uknoAI/kno/issues/59)) ([7b4971a](https://github.com/uknoAI/kno/commit/7b4971aca34c808b2a30ab3a50fb088fc4736ad2))

## 0.0.1 — in detail

### Added

- **release-please signs off its own commits**, so the DCO gate applies to the release PR like any
  other. It is not an exemption: a release commit contains no authored code — it is a generated
  CHANGELOG section and a manifest bump, both derived from commits that were each already signed off
  — so the certification the DCO asks for is satisfied by the commits it summarizes, and recording
  that with a trailer is more honest than carving out a second `if` in the check beside dependabot's.

  Found by the gate itself while cutting 0.0.1: the release PR sat red on `sign-off`, and it was the
  last thing between the tree and the tag.

- **A completed run no longer points at a command that does not exist.** `kno baseline` closed with
  *"Next: `kno value` …"*, and `kno value` is the next stage rather than a command in this release —
  so the last line of a first successful run named something the binary rejects with `unknown
  command`. A next step is a promise about this binary, not about the roadmap. README carried the
  same line and now carries the same correction.

- **`tapes/quickstart.tape` and `make tape`** ([debt #64](docs/debt.md#64), partly). The Definition
  of Done has asked for a re-recorded vhs tape since it was written, and there was no tape anywhere
  in the repo to re-record — so every PR satisfied that clause vacuously. The tape records `kno
  doctor`, a nine-Case eval file, and one `kno baseline` run against `fake:`, with the theme and
  typing speed pinned so a re-recording after a CLI change differs only where the CLI changed. The
  GIF itself is still owed and README deliberately does not reference one yet: a missing image is
  worse than no image.

  The tape says in a comment that the 1.000 score is honest rather than flattering — `fake:` answers
  every Case with what the Case expects — so nobody later "improves" it by picking an eval file that
  makes the number look earned. README says the same thing under the sample output.

- **`make ledger-check`, and the release refuses to build without it.** CLAUDE.md has said since the
  beginning that *"CI fails a release tag if any ledger entry's trigger has lapsed without a
  disposition"*. Nothing implemented it, so the rule the whole Debt Ledger rests on was enforced by
  remembering — and at the 0.0.1 close-out it caught a real lapse ([#64](docs/debt.md#64)) that had
  been missed by eye.

  It runs against the **tag**, before anything is built and before any credential is used, and it
  refuses to run without a version rather than defaulting to `.release-please-manifest.json` — that
  file holds the *last released* version, so checking against it asks whether an already-cut release
  is clear, which nothing names and which therefore always passes. Verified in both directions:
  green as the ledger stands, red with one disposition removed.


- **`core/value`** — which Cases measure which Asset, and what that will cost, decided before any
  money is spent. Nothing calls it yet.

  Routing clusters the dev Cases the baseline failed by tag and measures an Asset against the
  clusters its own tags overlap. Three fallbacks, because the common case is that nobody tagged
  anything: an **Asset** with no tags is measured against a sample of every failed Case (unlabelled
  is not irrelevant); when **no Case** carries a tag — the default state of a real eval file — there
  are no clusters to overlap and every Asset is measured against a sample of the failed Cases; and
  when nothing failed there is no failure signal to route on at all. The mode is fixed for the run
  **before the consent quote**, so the number a user approves is the number that path costs.

  **A slice of the dev split is reserved at random before routing runs**, and the harm test is drawn
  from it. Routing to the Cases a baseline failed and then comparing against those same recorded
  failures reuses one draw twice — every recorded score on that slice is zero by construction, so an
  Asset that does nothing measures **+0.70** at a 70%-pass agent, with a tight interval. On routed
  Cases the control arm is therefore measured **fresh**; on the reserved slice the recorded baseline
  is a valid control because the reservation saw no outcome, so it costs one measurement per Case
  instead of two.

  `--route none` (`Options.DisableRouting`) switches routing off without touching the reserved
  slice, so the flag a user reaches for when they distrust their tags cannot silently remove the
  regression check — and it drops the fresh control arm, because a random sample was never
  conditioned on the baseline's outcomes.

  The router's entire view of a Case is an ID, its tags, and a `Failed` bool: no Store, no Score. A
  reflection test fails if a field carrying a score value is ever added, because the routing path
  and the delta path sharing a source is what manufactures the effect.

  The harm test reports **the smallest regression it could have detected** — 0.18 at 20 control
  Cases, 0.11 at 60 — rather than only a pass/fail badge. A bare `underpowered` bool answers "is
  this absurd", which a reader turns into "is this safe"; a run reporting no regression with a
  detectable bound of 0.18 has not cleared an Asset that costs 0.10. The bool remains as a floor
  below 20 **Cases** (not measurements: repeat trials of one Case are not independent observations,
  and counting them would inflate the threshold the same way flattening trials inflates an
  interval).

- **The store can hold a Value run** (`schemaVersion` 3). Nothing writes to it yet.

  `kno purge` now says **"recorded row(s)"** rather than "outcome(s)", and clears both tables in
  one transaction. The count always spanned both; for a Value run it is entirely measurements, so
  the old noun reported outcomes about a run that has none. Two separate statements also let the
  second fail after the first destroyed content, returning an error with no count — telling a user
  nothing was removed, after removal.

  A `measurements` table keyed `(run_id, asset_id, case_id, arm, trial)`, because `outcomes` is
  keyed `(run_id, case_id)` and `RecordOutcome` is `INSERT OR IGNORE`. Value measures the same
  Case against many Assets in one run, so 200 Assets over 50 Cases would have written 50 rows and
  **silently discarded the other 9,950** — every one of them paid for — while `CompletedCases`
  reported the whole run finished and a resume skipped it.

  Every field of that key is load-bearing, and dropping any one reproduces the same bug one level
  down: two Assets measured on one Case, the two arms of one pair (the control arm is measured
  **fresh** whenever routing may have conditioned on the baseline —
  [ADR-0005](docs/adr/0005-value-cannot-see-user-side-conditioning.md)), and the trials bought to
  reduce variance. All three collisions are asserted, each verified against a primary key with
  that field removed.

  **The readers that assumed `outcomes` was the whole record now span both tables**, which is a
  double-spend rather than a reporting gap. `SettledSpend` is the only durable record of money
  spent and the budget guard is reseeded from it on resume: a Value run killed after $8 of a $10
  cap would have restarted the guard at zero and authorized another $10. `CaseObservations` and
  the distinct-model set behind the mid-run model gate span both for the same reason — otherwise a
  Run reports zero Cases beside real spend, and the gate compares against an empty set and reports
  success.

  `Purge` and `PurgeableCount` cover measurement content. A purge that cleared only outcomes would
  have printed a count larger than what it removed and reported success over content still on disk.

  New readers: `CompletedMeasurements` (what a Value resume consults — `CompletedCases` returns
  empty for every Value run, so a resume driven by it re-pays for everything), `CaseScores`,
  `WriteValuation`, and `Valuations`. A `Valuation` is written only once every measurement behind
  it is durable, so a run stopped mid-Asset leaves the paid measurements and no `Valuation`:
  resume finishes the Asset without paying twice, and nothing downstream reads a delta over half a
  sample.

- **CI requires a behavior-changing PR to document itself.** A new `Changelog` workflow reads the
  Conventional Commit type out of the **PR title** — the string squash-merge turns into the commit
  and release-please turns into the release notes — and fails the PR unless `CHANGELOG.md` is in
  the diff. `refactor:`, `chore:`, `test:`, and `build:` are exempt.

  The escape hatch is the **`no-changelog` label**, not a commit trailer or a phrase in the body:
  an exemption has to be visible on the PR, or it is indistinguishable from a check that did not
  run. Applying or removing the label re-runs the check, so using the hatch never costs an empty
  commit — an escape hatch people work around is worse than none.

  Repays [debt #49](docs/debt.md#49), which was opened when a branch rebuilt onto `main` merged
  with its CHANGELOG entry, its ledger repayment, and its plan file all silently dropped: `make
  docs` checks that links resolve, not that documentation is present. The entry's second half —
  asserting a PR title still describes its diff after a rename — it calls cheaper as a reviewer
  checklist item than as CI, so that is where it lands, in the PR template beside this check.
- **The release pipeline** — goreleaser, cosign, syft, SLSA provenance, an install script, and a
  `Makefile` target that cannot publish. Repays [`docs/debt.md#13`](docs/debt.md#13), whose trigger
  was *"before the first tagged release"*; with `release-please` holding a `0.0.1` PR, this is the
  last change that could land before it.

  A tag now produces six archives (darwin/linux/windows × amd64/arm64), one `checksums.txt`, a
  **keyless** cosign signature over that file, an SPDX SBOM per archive, and build provenance
  attested over every artifact the checksum file names. Keyless is the point: there is no private
  key in existence, so there is none to steal or rotate, and nothing in the workflow is a secret.

  The signature covers `checksums.txt` rather than each archive. One verification then covers
  everything, instead of six verifications that each say nothing about the other five.

  `kno --version` reports the real tag, its commit, and its build date, stamped into `cli`'s
  existing `version` variable by `-X` at link time. **`kno doctor --json` deliberately keeps
  reporting the bare version**: that field is a jq contract, and appending a commit hash to a value
  consumers parse as a version is a breaking change wearing a cosmetic disguise.

  A `go install` binary is no longer stuck at `dev` either: the identity falls back to the module
  and VCS metadata the toolchain already embeds, so it reports its module version, or its revision
  with `-dirty` when the tree was not clean. `doctor --json` exists to be pasted into a bug report,
  and a version field reading `dev` for every non-release install is a support burden dressed up as
  a contract.

  The release is created as a **draft** and published only after the provenance is attested. Every
  failure boundary in a release leaves something public behind, and an empty release — or one
  signed but not attested — is indistinguishable from a finished one to anyone reading the page.

  `make release` refuses to run outside GitHub Actions, and the local dry run passes `--snapshot`,
  under which goreleaser cannot publish at all. The guard checks an environment variable, so it is
  a safety catch against typing the wrong target rather than a control; the boundary is credential
  scope, the `release` environment, and the ancestor-of-`main` check — *nothing built on a laptop ships* is backed
  rather than trusted. `make release-check` validates the config on every PR, because a tag is the
  worst possible place to learn that the config is malformed.

  The Homebrew formula is written and **disabled**: no tap repository exists yet, and an enabled
  block pointing at a missing repository would fail the release after the artifacts were built and
  signed ([`docs/debt.md#73`](docs/debt.md#73)).

- **`stats/interval`** — confidence intervals on paired differences, the machinery prime directive 5
  requires before any delta can be reported. Nothing calls it yet.

  The method is chosen by a Goal's **declared** `ScoreDomain`, never by the data observed:
  dispatching on the sample makes the confidence level hold only conditional on a branch that is
  itself a function of that sample, and across many measurements some would land in each branch by
  luck while a consumer compared their intervals as one claim.

  Paired binary data uses an **adjusted-Wald** interval on the discordant counts, chosen by
  simulating coverage at the sample sizes this stage runs at. MOVER-Wilson under-covered in every
  cell measured (0.907–0.932 against 0.95), and a percentile bootstrap covered on average while
  returning a **zero-width interval in 13.6% of runs** at a 95% agent pass rate and 20 pairs — an
  inert asset against a strong agent, which is the most common thing in a real pool. A zero-width
  interval reads as certainty, and `Interval` exists as a message precisely so that absence cannot
  be mistaken for precision.

  The adjustment is a unit rather than the published half: measured, the half-adjustment
  under-covers where the variance is highest (0.938 at 20 pairs, p=0.50). The unit form is
  conservative in every cell measured. Over-covering is a wide interval; under-covering is a claim
  of confidence the data does not support.

  Continuous data uses a **Student-t** interval — with an actual t quantile. The first version of
  this package stamped `"t"` onto an interval computed with the normal quantile, which covered
  0.880 at five pairs and 0.930 at fifteen, and fifteen is the sample size DESIGN.md's own worked
  example produces.

  Two invariants are enforced at the single point every bound is written: **no interval is ever
  zero-width**, and **no bound is ever NaN or infinite** — a NaN renders as blank, and a reader
  fills a blank in themselves.

  `HarmBound` returns a one-sided upper bound, because "did this break something" is a one-sided
  question and a two-sided interval at a small control sample spans zero for a real regression —
  rendering, under the report's coloring rule, as "no regression".
- **`adapters/pool/jsonl`** — Assets from a JSONL file, the first `core.Pool`. Nothing calls it yet.

  `id` and `content` are required; `title`, `tags`, `kind`, and an acquisition cost are optional. A
  malformed record, a duplicate id, or an unknown field is **fatal**, not skipped — the same rule
  the Evals adapter states and for the same reason: if one adapter skipped bad records and another
  halted, the denominator behind every confidence interval would silently vary by adapter.

  `destination` and `context_tokens` are refused as unknown fields rather than quietly ignored,
  because the file carries what only its author knows and the adapter computes what it can measure.

  `context_tokens` is bytes over a fixed divisor, deliberately **not** the pricing path's
  `countTokens`: that reserves ~2.7x on prose against ~1.1x on base64, so two Assets of identical
  true cost would differ ~2.4x in the ranking denominator and greedy selection would order a
  portfolio by content type. It also takes a *model*, and an Asset's cost is read before a model is
  chosen — a denominator that moved with the run's model would make two pools' rankings
  incomparable. The residual bias is documented with its direction ([debt #68](docs/debt.md#68)).
- **`core.ContextInjector` on both provider adapters.** `WithContext(asset)` returns an Agent
  carrying the Asset in its prompt, and `Capabilities().ContextInject` is now true. Nothing calls
  it yet.

  It returns a **shallow copy of the Agent**, not a wrapper. The receiver stays usable as the
  control arm of the same measurement — get that wrong and every Value measurement compares an
  Asset against itself — and because the result is still the same type, `Estimator` and `Capable`
  forwarding cannot be forgotten. A hand-written wrapper that forwarded only `Invoke` would leave
  every reservation made against a run-scoped constant while the prompt carried the Asset, which
  `core/ring0.go` records having already quoted $0.06 for a run whose real exposure was $12.00.

  The Asset goes **after the system prompt and before the Case**. Providers cache on prefix, and
  `[system][asset]` is constant across an Asset's whole sample while the Case varies — so position,
  not which field each adapter uses, is what makes the Asset's tokens cacheable.

  Refused before any spend: a nil Asset, an empty one (byte-identical to the control, so every
  difference is zero with a tight interval and "inert" is indistinguishable from "not injected"), a
  non-UTF-8 one (JSON substitutes U+FFFD, so the model sees something other than what was priced),
  and a second injection into an already-injected Agent.

  `--max-prompt-bytes` now bounds the **Case**, and an injected Asset is bounded by it separately
  and charged **on top** — one meaning across both adapters. It was previously a total on
  `openai:`, which meant a Case large enough to fit under the ceiling alone but not beside the
  Asset was measured by the control arm and refused by the treatment arm: attrition correlated
  with the treatment, dropping exactly the long-prompt Cases and more of them as the Asset grew.
  A delta over the survivors rises with Asset size, in `delta_per_cost`'s numerator against a
  denominator that grew for the honest reason. Both arms now accept the same Cases by
  construction. `WorstCase` rises by the Asset accordingly, which is the run's real exposure.

- **Proto for the Value stage** (additive; `buf breaking` clean). Nothing emits these yet — this is
  the wire contract the stage is built against, landed first per CLAUDE.md's coordination rule.

  `Valuation` gains `not_measured` (an Asset that routed to nothing is a cheap, valuable answer and
  had nowhere to be recorded), `n_routed` / `n_dev` (a delta is the mean effect over the Cases an
  Asset was routed to **and nothing else** — these are what let a reader scale one Asset's delta
  against another's), `control_underpowered`, and `n_comparisons` (`delta_interval` controls a
  *per-comparison* error rate; with 200 Assets roughly ten null ones have intervals excluding zero
  by construction, and a consumer cannot correct for that from `level` alone).

  `Interval` gains `sidedness` and `n_pairs`. `SIDEDNESS_UNSPECIFIED` reads as **two-sided**,
  because every `Interval` ever written decodes as zero and they are all two-sided — a zero value
  that condemned them would make the field unreadable on exactly the records it describes. A harm bound is one-sided — "this Asset costs you no more
  than X" is a different question from "is the effect distinguishable from zero" — and written into
  a two-sided field it is read as two-sided by `RejectionReason.NO_EFFECT`, whose shipped definition
  is "the interval crosses zero".

  **`AssetRouted` and `AssetValued`**: every existing payload is Case- or Run-scoped, so Value's
  unit of work had no event at all. `AssetRouted` fires *before* any measurement, so a watcher sees
  the shape of the work before the money is spent.

  **`Run.sampling_seed`** — not `Generation.seed`, which is the provider's sampler. The control
  partition is drawn before routing, so whether a control set was outcome-independent is a claim
  only the seed can substantiate afterwards.

  **`ScoreDomain`**, carried on `Run` and `RunStarted` and declared by a Goal — `core.Goal` gains
  `Domain()`, so a new Goal cannot land without answering — rather than inferred from the scores
  observed. Inferring it
  is method selection from the sample: the confidence level would hold only conditional on a branch
  that is itself a function of the data.


- **`kno baseline` can reach a real provider.** `--agent openai:<model>` (and any OpenAI-compatible
  endpoint via `--base-url`) and `--agent anthropic:<model>` now resolve to the adapters built in
  M2-3 through M2-8, which until now were unreachable from the command line. This is the first
  release in which the command can spend money.

  New flags: `--base-url`, `--key-env`, `--allow-insecure-base-url`, `--allow-private-address`,
  `--max-output-tokens`, `--max-prompt-bytes`, `--temperature`, `--seed`, `--system`,
  `--generation-params`, `--use-legacy-max-tokens`, `--timeout`, `--price-input-per-mtok`,
  `--price-output-per-mtok`, `--accept-unknown-cost`.

  **No `KNO_*` environment mirrors.** `DESIGN.md` specifies three layers — flag, env var,
  `kno.yaml` — and none of that machinery exists. Shipping a mirror column would be specifying a
  config system in a flag table. Tracked as [debt #62](docs/debt.md#62).

- **`kno doctor`** prints the adapters, which of them cost money, the goals, and the price table's
  date. `errs.ErrCapabilityUnsupported`'s fix line has always told users to run it and no such
  command existed. It contacts nothing and reads no credential.

- **`--accept-unknown-cost`.** A run whose per-Case cost cannot be computed is now **refused**
  rather than run silently. With an agent that cannot price itself and no `--cost-per-call-usd`,
  the quote arithmetic collapsed to zero and `confirmRun` returned before ever asking — so the
  configuration we know *least* about was the only one that skipped consent, while a priced model
  with no cap did prompt. Refusing rather than prompting is deliberate: a confirmation that cannot
  state a dollar figure gives a human no basis to decide, and a flag someone had to type is
  greppable in a CI config.

- The reduced-concurrency **report**: a `width` line when the engine narrowed the run, and
  `concurrency` / `concurrency_requested` / `concurrency_reduced_reason` in `--json`. Partly repays
  [debt #44](docs/debt.md#44); the consent-prompt half stays open.

- **OpenTelemetry spans**, correlated by run ID: one per run, one per Case, one per provider call.
  Instrumentation is **unconditional** — the OTel API's global provider is a no-op until something
  registers a real one, so a run that is not tracing allocates nothing. `--trace-spans` writes
  them to stderr for local debugging. The local exporter's queue holds 2048 spans and drops
  silently past that, so it is for reading a run, not for auditing a million-Case one.

  **Spans carry IDs, counts, and money only** — never a prompt, an answer, or a system prompt.
  `docs/retention.md` tells users their conversation content lives in the local store and that
  `kno purge` removes it; a span is shipped to a collector, which is the one place purge cannot
  reach. Enforced by the `observe` package's attribute constructors (nothing there accepts
  content) and by a test that drives a real run — with an agent whose *errors quote the Case* —
  and scans every attribute, event, and status description on every span. `span.RecordError` is
  deliberately unused: it writes the error's text into an event, and a wrapped provider error can
  carry the prompt that produced it.

  **OTLP export is not here.** `DESIGN.md:399` places OTel *export* at v0.3 and `CLAUDE.md` says
  tracing is *built in* — read precisely those agree, and separating instrumentation from export
  honors both without editing either. Measured cost: both OTLP exporters pull 21 modules including
  `google.golang.org/grpc`; the API + SDK + stdout exporter pulls 11 and no grpc. Partly repays
  [debt #37](docs/debt.md#37), which stays open for the export half.

  New dependencies: `go.opentelemetry.io/otel`, `/trace`, `/sdk`, and
  `/exporters/stdout/stdouttrace` (Apache-2.0, CNCF-governed, the standard tracing API for Go).
  Nothing in stdlib expresses distributed tracing, and a homegrown span format would be a format
  no collector reads.


- `Run.case_execution` is now written for every run that executes Cases, with the counts
  aggregated from what is durably recorded rather than from in-memory counters — so they survive a
  crash and stay correct across a resume. Presence is set by the stage: absent means "this stage
  does not execute Cases", never "this run scored nothing". Repays
  [debt #26](docs/debt.md#26).

  `--json` and the human report both read it, **falling back to the flat counters when it is
  absent**. It is composed from a store read at close; a read that fails leaves it absent, and the
  chained getters then return zero — printing `0 scored, 0 errored` for a run that scored every
  Case, with the correct number in the flat counter beside it. A CI gate reads that as a total
  failure. The flat counters are still written on every path and are still correct.

  The counts stay non-pointer: the absent case is unreachable while Baseline is the only front
  end, so making them nullable would break a `jq` pipeline and buy nothing.

  Composing it is **not** allowed to fail the run. It is a read, and it was sequenced ahead of
  `FinishRun`: one transient store error left the `Run` in `RUN_STATUS_RUNNING` with no
  `finished_at` — indistinguishable from a crash — suppressed `RunFinished`, which the schema
  promises is always the last event and which an SSE consumer waits on forever, and replaced the
  run's real error, so a budget stop reported "reading case observations" and exited with the
  generic failure code. The failure is now surfaced only after the run is durably closed, and only
  when nothing worse happened.

  The dev/holdout split is carried forward from the `Run` rather than re-read from this process's
  options. `checkResumable` does not compare the split — `InputFingerprint` covers the eval
  *source* only — so a resume declaring a different `--holdout-frac` passes every check, and
  re-reading would have put two contradictory splits on one message, with the presence-carrying
  copy describing a split the run was never measured under.


- **`SettlementOvershoot`**, emitted when a settlement pushes spend past the cost cap. `Overshoot()`
  has made the excess computable since M2-2 and nothing reported it. Gated on the per-settlement
  delta, so the event count is bounded by concurrency rather than by Case count — once the cap
  binds, only reservations already in flight can overshoot. Surfaces [debt #32](docs/debt.md#32).
- **`RetryAttempted`**, emitted *before* the backoff wait, so a watcher can tell a run obeying a
  provider's `Retry-After` from a hung one. Emitted after the sleep it announces, it would report
  idleness only once idleness had ended.
- **`SpendRecorded`**, on the progress heartbeat rather than per settlement. **Not emitted in any
  shipped configuration yet** — the heartbeat is off by default and nothing turns it on until the
  M2-11 flag, so a default run's stream still does not report spend. All three of its
  totals are cumulative — the message was shaped for a heartbeat — and per-settlement emission
  would put another fsync behind every agent call.

- A resumed run no longer emits a second `RunStarted` carrying the original total, which made a
  live view reset its progress and jump backward on every resume. It emits `RunResumed`, carrying
  what was already completed, what remains, and the spend restored from disk — so a consumer can
  see the run did not believe it had spent nothing. The opening event is chosen by whether the
  stream already has one, not by `--resume`: a run whose first process died before emitting
  anything still opens with `RunStarted`, which is the only payload carrying the run's identity.
  Repays [debt #29](docs/debt.md#29).
- Event sequence numbers are allocated immediately before the write rather than at event
  construction, and allocation and write happen under one lock. `Event.sequence` exists so a
  consumer can tell a lost event from none; a number taken on a path that returns without writing
  burns it, and two emitters can otherwise commit 6 before 5, which an insertion-ordered consumer
  reads as a gap. Both are unreachable while every emitter is serialized — they become reachable
  with the progress ticker that follows, which is why the rule lands first.
- `RunFinished` is refused a successor. The payload has always promised it is the last event and
  nothing enforced it.
- `RunResumed` cannot report negative progress. Its operands are each bounded by the eval set but
  their difference is not, and a resume with a larger holdout fraction completes more Cases than
  the new dev count — measured at `remaining=-35`, which is the denominator of session progress.

- The confirmation prompt no longer quotes a figure the guard will never permit. The total was
  bounded against the static `--max-cost-usd` rather than what the run could actually still
  spend, so a run resumed with $0.10 of a $5.00 cap left was quoted at **$5.00** — and the CLI
  prints both numbers in one sentence, so the user read "would spend about $5.00 ($0.10
  remaining)". Measured at 50x. A `--max-calls` cap was not applied to the dollar figure at all:
  200 Cases against `--max-calls 10` quoted $10.00 for $0.50 of permitted spend, 20x.

  **This changes when the prompt fires.** It is compared against the bounded figure, so a run
  that can only spend $0.10 no longer asks about a `--confirm-threshold` of $1.00 — it proceeds
  and spends the $0.10. Previously it quoted the whole cap, crossed the threshold, and (since
  the current prompt always declines) refused. That refusal was an accident of a wrong number,
  not consent: the threshold means "ask before spending more than this", and the cap still
  binds regardless.

- A spend-cap 429 (`enforced_spend_limit_reached`) is terminal rather than retried. It never
  clears within a run, so retrying burned each Case's whole retry budget and settled one call
  per attempt against `--max-calls`.
- A failed call the provider billed for is no longer observed as free. A 200 carrying both an
  `error` object and a `usage` block — a shape several OpenAI-compatible gateways produce — was
  parsed, its usage discarded, and settled at $0 against the cost cap. The reported charge now
  rides on the error. Partly repays [debt #43](docs/debt.md#43); the remaining halves are in
  `core` and `transport` and are named in that entry.
- `latency_ms` no longer includes the rate limiter's own hold, so the figure describes the
  provider rather than our pacing. Measured before the fix: `1002ms` for a call the server
  answered instantly, after one 429 with `Retry-After: 1`.
- The provider's `error.code` is bounded and flattened like `error.message`, so it cannot forge
  a `fix:` line in the CLI's error grammar. Demonstrated before the fix with an 8470-byte error
  whose `code` field carried a newline and a fabricated `fix:` instruction.

- A model whose name extends a priced one is no longer priced at that model's rate unless the
  extension names a **version** rather than a variant. `claude-opus-5-fast` exists on the
  provider's model list and resolved to `claude-opus-5` by longest-prefix match, authorizing
  runs at a fraction of fast mode's published rate — a cost cap that is not the cap the user
  set.

  The rule is orthographic: versions are numbers, variants are words. Every published pin still
  resolves — `-20250805`, `-2026-03-01`, `-0613`, `-1-20250805` for a point release, `@20250929`
  on Vertex, `-20250929-v1:0` on Bedrock, and `-latest`. Suffixes that name a different product
  (`-fast`, `-pro`, `-preview`) are now **unpriced**, which under `--max-cost-usd` refuses the
  run visibly rather than reserving against the wrong rate. Without a cost cap, nothing changes:
  an unpriced model still falls back to `--est-cost-per-call`.

- A resumed run's baseline score now spans the whole run. Previously the case counts spanned
  the run and the mean spanned only the Cases the resuming process scored, so the two described
  different populations — a run resumed halfway reported `0.48` where the whole run's mean was
  `0.5`. Repays [debt #27](docs/debt.md#27).
- A run holding Cases whose scores were purged by a pre-`score_value` build now reports **no**
  baseline score, with a reason naming the purge, rather than the mean over whichever Cases
  still have numbers.
- The CLI no longer warns "no cases scored" on a run that scored every Case. A nil
  aggregate now has two meanings, and `BaselineResult.AggregateUnavailable` (plus
  `"score_unavailable"` in `--json`) distinguishes them: nothing scored, versus scores that
  cannot be read back. The counts stay accurate in both.
- A run that is both unscoreable and over the error-rate threshold reports both reasons.
  `IncompleteReason` was assigned twice, and the half that lost is the one with no other
  signal in the report.
- `ErrorRateExceeded` and `IncompleteReason` are cleared before being recomputed. Both are
  derived from the whole run, but were only ever set — so a process that errored past the
  threshold and stopped stamped the run permanently, and no amount of clean resumed work
  could clear it.
- A NaN or infinite score no longer becomes the run's permanent aggregate. NaN propagates
  through SQL `SUM`, so a single bad Goal result would have been read back by every
  subsequent resume of that run.
- `make check`'s coverage and godoc gates no longer descend into dot- or
  underscore-prefixed directories. A nested
  checkout of this module — an agent worktree under `.claude/`, for instance — was scanned as
  our own source, reporting 998 undocumented symbols and every package as uncovered. The
  built `covercheck` and `godoccheck` binaries (3.5 MB each) were also tracked in git by
  accident; they are removed and ignored.


- **`errs.ErrTransportTransient`**, and `core` retries it. A stale pooled connection is not the
  agent failing — at concurrency, any pause in a long run produces a handful, and treating them as
  terminal marked a healthy baseline unusable over an idle timeout. The transport classified them
  already; the sentinel had to live in `core` because `core` cannot import an internal adapter
  package. Repays `docs/debt.md#38`.
- **A retry budget bounded by time as well as attempts.** Three attempts at 500ms doubling is a
  1.5-second window, and a real provider's sustained 429 window is minutes — so a rate-limited
  account had a perfectly good baseline marked `error_rate_exceeded`. Time alone is also wrong:
  each attempt takes its own reservation, so a long window lets one Case consume dozens of calls
  against `--max-calls`. Both bounds, whichever binds first. A provider-supplied `Retry-After`
  replaces our guess, because the provider is the authority on its own limits.
- **A feasibility check** that reduces concurrency rather than letting the guard deny its way to a
  halt. A pessimistic reservation holds `concurrency × estimate` in flight; when that exceeds the
  cap, nothing settles and the run stops having done almost nothing — measured at a 32k output
  ceiling against a $1.00 cap at concurrency 8, the **fourth** Case denied with **$0.00 spent**. It
  runs after `Guard.Restore`, so a resume is judged against the headroom it actually has, and an
  unaffordable run exits 2 rather than 1: an exhausted cap is a resumable stop, not a broken build.

  It plans for the concurrency the run will **actually use** — `--concurrency` defaults to 0 and the
  executor turns that into `min(NumCPU, 8)`, so treating zero as "unset, skip the check" bypassed
  the guard on the path almost every user takes. And both refusals happen **before** the `Run`
  record is created: refusing afterwards left a row permanently in `RUNNING` with no outcomes, and
  since the interactive path declines by default, every above-threshold invocation minted a fresh
  orphan that a CI gate reading exit 2 reported as green.
- **`core.Estimator` gains `WorstCase`.** Planning needs a number and per-Case estimates need a
  Case, so both the feasibility check and the consent prompt were computing against
  `EstCostPerCallUSDMicros` — a scalar an `Estimator` adapter does not use. Measured with an
  adapter pricing at $0.20 against a scalar of $0.001: the prompt quoted **$0.06 for a run whose
  real exposure was $12.00**, and the feasibility check found headroom for 250 in-flight Cases
  while the run stalled at 0 of 60.
- **`Guard.PreConfirm`** — the human is asked about the **whole run**, once, before any of it is
  authorized. The per-operation prompt showed one call's estimate and recorded agreement for the
  life of the run, so a user shown `$0.04` for the first Case that crossed the threshold consented
  to 10,000 Cases at that price. The quote is computed in `core` after the completed set is known,
  so a resume asks only about what is left, and it is bounded by the cap, because quoting more than
  the guard can spend is false in the direction that teaches people to dismiss the prompt.

  A decision **disarms the per-operation prompt**, including when the run falls below the threshold
  and is not asked about at all. Otherwise a run could still be stopped at its first expensive Case
  and asked about that one Case — which counted as consent for every Case after it, which is
  verbatim the failure this replaces. A refusal is recorded too, so a later `Authorize` cannot
  re-ask and authorize what was just declined.
- A retried Case now persists **every call it paid for**, including on the retry-*exhausted* path
  where there is no Response — which is the branch a 429 storm actually takes. `store.Outcome.Spend`
  was documented as including failed attempts and did not: measured 5 persisted against 15 settled,
  so `Guard.Restore` let a resume re-authorize 10 calls that were already made and paid for.
- A resume is refused when the **resolved model** changed — a ref like `openai:gpt-4.1` is a moving
  pointer, and a run resumed after the alias re-points would blend two models into one
  `AggregateScore`. The provider's build identifier is recorded but never refused on: it changes
  routinely with no model change, and a false refusal costs a full re-run.

  **The check cannot fire yet.** It reads `Run.case_execution.resolved_models`, and nothing writes
  that field until M2-10 — so this lands ahead of the data it needs, deliberately, because the
  adapter that produces a resolved model arrives in M2-7 and the check has to exist before it does.
  See `docs/debt.md#42`.
- **`adapters/agent/pricing`**, the dated price table and the pessimistic estimate the budget guard
  reserves against. Prices as published on 2026-08-21 for Anthropic and OpenAI models; the table is
  **static and never fetched at runtime**, because an endpoint that is down leaves the engine
  choosing between refusing to run and running with no ceiling, and one that is wrong is a spend
  path with no ceiling at all.
  - **An unknown model is unpriced, not free.** A zero estimate makes a dollar cap unenforceable —
    the failure that overshot a cap in M1 — so a model with no row is refused under a cost cap
    rather than authorized against a guess. Models reached through a base URL (OpenRouter, a
    self-hosted server) are deliberately absent: their prices are not these.
  - **Every term is pessimistic.** Input is charged at the fresh rate, never the cached one, because
    whether a prompt hits the provider's cache is not knowable before the call — and assuming a hit
    under-reserves exactly when a run repeats similar prompts. Output is charged at the full
    ceiling, because the ceiling is what the request permits.
  - **Claude 4.7 and later, and Mythos, are priced for their denser tokenizer**, which produces
    roughly 30% more tokens for the same text. Applying the old ratio under-counts every input by
    about a quarter.
  - **A dated model identifier resolves to its base row by longest prefix.** `claude-sonnet-4-5-20250929`
    is the canonical API ID and `claude-sonnet-4-5` is the alias; pricing only the alias meant every
    user who pinned a version had their run refused under a cost cap.
  - Token counting is bytes divided by a constant, with a stated safety margin — not a vendored
    tokenizer. The divisor is set from measurements against the real tokenizer, and it is set by the
    **tail** rather than the average: base64 runs 1.47 bytes/token against English prose at 3.6, and
    machine-shaped text is exactly what an Asset embedded in a Case looks like. It bounds a
    reservation; settlement reconciles against the provider's own reported usage.
  - `Prompt` names the parts the provider bills — system, context, history, input — rather than
    taking one string. The injected Asset is the largest term and the entire point of the product,
    and a single `input` parameter made omitting it the path of least resistance.
- **`adapters/agent/agentref`**, the parser for `scheme:target[@base-url]` — one grammar for flags,
  `kno.yaml`, the API, and the SDKs. Parsing is separate from resolution, so a typo and an
  unsupported provider produce different errors rather than the same one.
  - The scheme ends at the **first** colon, because a model name may contain its own: Ollama spells
    them `llama3:8b`, OpenRouter spells them `vendor/model:free`.
  - The base URL begins at the first `@` whose remainder starts `http://` or `https://`, matched
    case-insensitively and after trimming — not the first `@` (which breaks `openai:my-model@v2`)
    and not the last (which breaks `openai:m@https://user:pass@host/v1`, splitting *inside* the
    credential and hiding it from the check meant to catch it). A post-condition that does not
    depend on the split being right refuses a URL in the model slot outright, because when the
    split misses, the whole URL is reconstructed into `AgentRef.Ref`.
  - `AgentRef.Ref` is **canonical, not verbatim**: the scheme and the base URL's host are
    lowercased, a default port and trailing slashes dropped, whitespace trimmed. `Ref` is what a
    resume compares, and stored byte-for-byte two spellings of one agent read as two — telling the
    user the agent changed and pointing them at a setting they never altered.
  - A base URL carrying userinfo, a fragment, or a query is refused. `AgentRef.Ref` is persisted on
    the Run, emitted on `RunStarted`, and rendered in `--json` and logs, so a credential there would
    reach all four.
- **The repository's first fuzz target**, `FuzzParse` — `make fuzz-short` now runs instead of
  reporting PEND, repaying part of `docs/debt.md#4`. It asserts invariants rather than absence of
  panics, and found **five** real defects: a base URL carrying a fragment, and four in the
  canonicalization added during review — a host rewrite that produced an unparseable URL, IPv6
  brackets dropped, and a trailing-slash trim that was not idempotent.
- **`adapters/agent/internal/transport`**, the shared HTTP layer every provider adapter will sit
  on. It owns what must be identical across adapters and is dangerous to reimplement: which hosts a
  request may reach, which credential may travel there, how rate limits are honored, and what an
  error may say.
  - **A credential bound to one host never reaches another** — not through a redirect, not through
    a misconfiguration, not with a flag. Cross-host redirects are refused outright rather than
    filtered, because Go's `net/http` strips only `Authorization`, `WWW-Authenticate`, `Cookie`,
    and `Cookie2` on a cross-domain redirect — and **not `x-api-key`**, which is how Anthropic
    authenticates.
  - **Key bindings are explicit, never derived** — a host is bound to the *name* of an environment
    variable, so the key itself never appears in a flag. (The CLI flag that exposes this lands with
    the adapters.) A derived
    scheme is not injective — env var names permit only `[A-Za-z0-9_]`, so `api.groq.com` and the
    typosquat `api-groq.com` would collapse to one variable. A scheme's default applies only to
    that scheme's own host, so pointing an `openai:` model at Groq does not mail the user's OpenAI
    key to a third party.
  - **Link-local (`169.254.0.0/16`) is refused with no override** — that is where cloud instance
    metadata lives, and Kno persists response bodies. Loopback and RFC1918 are opt-in, because
    local vLLM and Ollama are a real use.
  - **A URL carrying userinfo is refused rather than stripped.** Silently rewriting it would hide
    that the credential is already in the user's shell history.
  - **The transport does not retry, and that is enforced rather than asserted.** `Request.GetBody`
    is cleared, because `net/http` replays a request on a reused connection when it is set — which
    an `Idempotency-Key` (see `docs/debt.md#20`) would turn on silently. A replay is invisible at
    the server, so the test asserts the invariant on the request itself. Because the body cannot
    survive a redirect once `GetBody` is nil, **all** redirects are refused — a 307 would otherwise
    return as an empty-bodied answer with no error, and a 302 would deliver a bodyless GET.
  - **Private-address rules are enforced against the RESOLVED address**, in the dialer, not against
    the URL as typed. `net.ParseIP` rejects `127.1`, `2130706433`, and `0x7f.1` while the resolver
    accepts all three as loopback, and any hostname can be pointed at link-local. Checking only the
    typed URL was bypassable by spelling.
  - `Retry-After` is honored in both RFC 9110 forms and clamped, so a misconfigured gateway cannot
    hang a run for a day.
- **`core.Estimator`**, an optional Ring-0 interface: an adapter can price a Case before the call
  is made. A cost cap the guard checks only at settlement is a cap discovered after the money is
  gone, and what a Case costs depends on the Case — a single run-scoped scalar cannot express that.
  Optional, like `Capable` and the injectors, so the fake agent and every existing caller are
  unaffected. **When a cost cap is set**, an adapter that cannot price a Case — or that answers
  zero, or reserves more than one call — has that Case refused rather than authorized against a
  cheaper guess; the priced Cases still run, and the refused ones are left unrecorded so a resume
  with a fixed pricing table picks them up. Without a cost cap the run proceeds on the scalar,
  because refusing when the user asked for no dollar cap would be worse than running uncapped.
  `Estimate` must be local and must not call the provider: it runs before the guard authorizes
  anything, so a network call there would spend outside the guard entirely. The engine bounds it
  with a timeout regardless.
- **`budget.Guard.Authorize` now validates the Estimate it is given.** A negative value does not
  under-reserve, it *credits* the budget — measured at $6.00 of headroom on a $1.00 cap, and 60
  calls authorized against a cap of 2. `cli/baseline.go` already refused a negative
  `--cost-per-call-usd` for this reason; moving the number from a validated flag to adapter code
  meant the defense had to move to the choke point every spend path shares.
- **`Guard.Overshoot`** reports how far settled spend has passed the cost cap. `Remaining` clamps at
  zero, so a Guard that blew its cap read identically to one exactly consumed — the breach was not
  merely unenforced but unobservable. This is observability, not enforcement: by settlement the
  money is spent, and making `Settle` fail would turn a successful, paid, scored call into an
  errored Case and lose work already paid for.
- **`kno purge`** — delete stored agent output and judge rationales for a run, keeping the scores,
  costs, and completion records. The database is opened with `secure_delete`, and a purge
  checkpoints the WAL and `VACUUM`s, so the content is gone from the bytes on disk rather than
  merely unlinked from a column — without this, `strings kno.db` recovered 14 of 16 occurrences of
  a Case's output from a purge that reported success. It NULLs the trace columns and never deletes a row: the recorded
  outcome IS the done-marker Kno resumes from, so a purge that removed rows would make a purged run
  pay for every Case a second time. A privacy feature that costs money is not one. Repays
  `docs/debt.md#25`, including the test that entry required by name.
- **A schema migration path.** `store` now tracks `PRAGMA user_version` and applies numbered steps.
  Fresh and existing databases take the *same* path, so the migration runs on every open rather
  than only on files old enough to need it — a migration only old files run is a migration nobody
  tests. Opening a database written by a newer build is refused rather than guessed at.
- **Seven columns on `outcomes`**: `score_value`, `score_passed`, `refused`, `truncated`,
  `usage_estimated`, `provider_build_id`, `resolved_model`. Every fact a later stage reads now lives
  outside the protobuf blobs that `kno purge` clears. Existing rows are backfilled from
  `score_proto`; a row purged before the column existed keeps a NULL `score_value` and is reported
  as unrecoverable rather than counted as zero, because averaging in a zero would drag the mean down
  and present the result as the run's actual aggregate.
- **`docs/cookbook/retention.md`** — what Kno stores, what purge removes, and what it does not cover.
  CLAUDE.md requires retention stated plainly; this is that.
- **M2-0, the proto surface for real provider adapters.** All additive; `buf breaking` clean.
  - `Price`: a four-rate vector (input, cached-read, cache-write, output) rather than an
    input/output pair. Both target providers price cached input differently, and a two-field model
    settles a cache read at full input price — a systematic overstatement in the direction a user
    notices as divergence from their invoice.
  - `Generation` on `Run`: temperature, `top_p`, seed, and `max_output_tokens`, all optional so
    that "the adapter's default" is distinguishable from a deliberate zero. `max_output_tokens` is
    recorded because it is load-bearing twice — the output term of every cost prediction *and* the
    truncation threshold. Named `Generation`, not `Sampling`, on the precedent DESIGN.md set when
    it rejected `bootstrap`: in a tool that reports confidence intervals, "sampling" already means
    something else.
  - `Run.pricing_table_version`: which dated table produced this Run's cost figures, so a report
    can say the spend number is reported usage at a dated price rather than an invoice.
  - `Capabilities.generation_params`: whether an adapter accepts temperature at all. Assuming it does
    breaks reasoning models, which reject any non-default temperature with a 400 — every Case would
    error and the run would report "too many cases errored" while naming nothing about the cause.
  - `Response` gains `cached_tokens`, `usage_estimated`, `refused`, `stop_reason`, `resolved_model`,
    and `provider_build_id` (not `backend_id` — `store/` already uses "backend" for a Store
    implementation). A refusal is scored *and* recorded: an account whose safety settings refuse
    every Case would otherwise produce 100% scored Cases, an aggregate of 0.000, and a clean error
    rate — a usable-looking baseline for a run in which the agent was never measured. A
    `STOP_REASON_LENGTH` response is a well-formed 200 with a truncated answer, which would
    otherwise let Kno's own output ceiling silently depress the score.
  - `AgentRef.base_url` and the `scheme:model@base-url` grammar, so one `base_url`-configurable
    adapter reaches every OpenAI-compatible provider. Parsed into its own field because the
    security rules key on the host.
  - `CaseExecution` on `Run` (see ADR-0004), and four event payloads: `RunResumed`,
    `RetryAttempted`, `RateLimitWaiting`, `SettlementOvershoot`. `RetryAttempted.reason` is an
    enum rather than a string, so "never provider text on the event stream" is a wire constraint
    rather than a comment nothing enforces.
  - A second schema gate, `TestRateFieldsAreDistinguishableFromAmounts`: `Price`'s fields are
    micro-USD *per million tokens*, and the existing money gate would happily certify one being
    summed into a cost accumulator — a silent factor of 1e6.
- **ADR-0004**: per-run observations live in a submessage derived from persisted rows, not from
  in-memory counters. Settles the open design question the M2 plan's third adversarial review
  raised and the plan deliberately left open rather than answering in prose.
- Exit code `4`, `ExitInterrupted`: a run ended by a signal or a deadline is resumable, not broken.
  Additive to the exit-code contract; `2` and `4` are the two resumable outcomes.
- README: quickstart with verified output, the exit-code contract, and a stage-by-stage status
  table. It had gone untouched from the first commit through a working `kno baseline`.
- `kno baseline`: the first user-facing command. Runs the agent over the dev half of an eval set,
  scores it, reports what it cost, and names the next step. Exit codes distinguish a budget stop
  (resumable) from a failure, so CI can gate on the difference. `--json` for pipelines.
- `core.Baseline`: the first pipeline stage. Runs an agent over the dev Cases, scores each
  Response, persists every outcome, and survives interruption. Takes a `*SealedEvals`, so a stage
  that could read the holdout does not compile.
- `adapters/agent/fake` and `goal/exactmatch`: a deterministic agent and Goal, so the whole
  pipeline is exercisable without a provider or a bill.
- `executor`: a bounded worker pool with a written shutdown protocol. Cases are cloned in the
  producer before dispatch, so a source reusing its buffer cannot be read by a worker mid-rewrite
  — the borrow contract becoming enforcement rather than documentation. A fatal source error
  drains in-flight work rather than discarding results already paid for.
- `core.Seal` and `SealedEvals`: the holdout seal, as a distinct type. A stage that requires a
  sealed source cannot be handed a raw adapter — forgetting to seal is a compile error rather than
  a review lapse. Unassigned splits are filtered out too, since treating "unknown" as "dev" is how
  a holdout leaks one Case at a time.
- `adapters/evals/jsonl`: the first Ring-1 adapter. Reads Cases from JSON Lines, assigns the
  dev/holdout split deterministically at ingestion, and passes `coretest.ConformIterator`.
- `Run` and the event spine (`run.proto`, `event.proto`). The event stream is the single spine the
  TUI, logs, and API all render, and it carries IDs and metrics only — conversation content is
  structurally unrepresentable, enforced by a schema test rather than by reviewer vigilance.
  `CaseErrored` is a distinct payload from `CaseScored`, so a live view and the persisted result
  cannot disagree about the sample. (Correction: the M1-1 notes said `Run`'s Case counters track
  presence. That edit did not apply and shipped unfixed — see `docs/debt.md#26`.) Events use `EventError` rather than `Actionable`, because
  `Actionable.cause` carries upstream provider errors verbatim and providers echo request content
  in them. Retires debt 2.
- `stats/budget`: the spend guard. Every path that can call an LLM or a fine-tuning API goes
  through it — estimate, authorize, settle or release — with reservations counted against the caps
  while outstanding, so concurrent workers cannot each pass a check only one of them should.
- The Ring-0 contracts in `core`: `Agent`, `Capable`, `ContextInjector`, `KnowledgeInjector`,
  `Evals`, `Pool`, `Goal`, and `Tuner`, plus type aliases making the generated `kno.v1` messages the
  domain types (ADR-0001). These carry a stability promise from 1.0.
- `core/errs`: the `what failed → why → fix` error grammar, the four exit codes CI gates branch on,
  and sentinels whose identity survives serialization — `errors.Is` matches on `Code`, so an error
  rebuilt from the wire still compares equal to its sentinel.
- `coretest`: the iterator conformance harness every adapter must pass.
- `covercheck` and `godoccheck`, retiring the two gates that had reported `PEND` since the
  foundation landed. Coverage floors (85% on `core`/`stats`/`bridge`/`plugin`, 70% repo-wide) and
  the no-decrease ratchet are now enforced, as is godoc coverage on every exported symbol.
- `Direction` enum (`DIRECTION_MAXIMIZE` / `DIRECTION_MINIMIZE`) and `Report.goal_direction`.
  `DESIGN.md` defines a Goal as having a direction and both `Score.value` and
  `Valuation.delta_goal` document their sign as relative to it, but direction was represented
  nowhere — so the sign of `holdout_gain`, the headline number, was uninterpretable.
- Repository foundation: Go module, Apache-2.0 license, governance and community files, the quality
  gate machinery (`make check` and every gate `CLAUDE.md` requires), CI workflows, and the
  [Debt Ledger](docs/debt.md).
- Gates distinguish `SKIP` (a tool is missing — a hard failure under `KNO_CI`) from `PEND` (the
  implementation lands in a named later milestone), so a gate can never pass quietly without
  running.
- `make test-live` is the only path that can spend money, and it refuses to start unless a budget
  cap is set *and* some code actually reads it.
- Build tools pinned in an isolated `tools/` module via Go's native `tool` directive, so
  contributors and CI run byte-identical versions and tool dependencies never enter the shipping
  module's dependency graph.
- [ADR-0001](docs/adr/0001-proto-as-domain-types.md): generated proto messages are the domain types.
- [ADR-0002](docs/adr/0002-generated-code-layout.md): generated code is checked in under `gen/`.
- [ADR-0003](docs/adr/0003-platform-schema-boundary.md): the platform never adds fields to `kno.v1`.


### Changed

- **The budget-and-retry core is one implementation, not one per stage** (`core/invoke.go`).

  Reviewed after merging rather than before, because a branching mistake carried it onto `main`
  inside a commit titled `docs:` (7a2d94f) — so 0.0.1's generated release notes file it under
  Documentation. That is the second instance of [#49](docs/debt.md#49)'s title-describes-the-diff
  half, whose own trigger text argued it was cheaper as a checklist item than as CI; two instances
  is the argument for CI. The review was done line by line against the pre-extraction file and
  confirmed behaviour-preserving: `invokeOnce` is byte-identical modulo its receiver, every
  difference in the retry loop is a comment rewrap or the hook indirection, both detached contexts
  are unchanged, emit-before-wait ordering holds, all six error-classification helpers moved
  verbatim, the panic path still formats `%T` rather than `%v`, and the three hoisted getters are
  pure functions of value-receiver fields so hoisting them out of the loop cannot change what they
  return.

  It found one thing, recorded as [#77](docs/debt.md#77): a stage that forgets to wire an event hook
  is silent about it, and the two hooks in question are the ones that explain where money went.
  Unreachable in this release — Baseline wires both — and nil-safety is kept deliberately, since a
  panic on a money path is worse than a missing event. `TestBaselineWiresEveryInvokerHook` is the
  enforcement, verified failing with a hook removed. No
  behaviour change: `BaselineOptions.invokeWithRetry` is now a wrapper that supplies Baseline's two
  event hooks, and everything else moved verbatim.

  It is extracted rather than duplicated because the Value stage needs the same "authorize, call,
  settle, retry" and a second copy is how two stages come to disagree about money. What moved is not
  generic plumbing — it is six separately-discovered defects held in place by their fixes: the retry
  budget measured against a real clock rather than an injectable one (a frozen clock turned a
  cumulative bound into a per-sleep one, 40 provider calls for one Case against a 50ms budget);
  billing accumulated across attempts, since the guard settles each one but only the last error
  survives the loop; settled calls counted apart from attempts, since a refused `Authorize` returns
  before settling; a recovered panic carrying its spend out, so a Case the guard charged cannot be
  persisted as free; saturating arithmetic matching `Guard.Settle`; and an overshoot **recorded**
  rather than returned, because returning it discarded a paid, scoreable answer and then skipped the
  Case forever on resume.

  The hooks are handed a context that is already detached and already carries its grace, so neither
  stage can get that wrong independently — an overshoot is emitted exactly when a budget stop has
  cancelled the worker, and a hook using the live context would drop the one event explaining where
  the money went.

  Verified the way a no-behaviour-change refactor should be: Baseline's existing suite, plus three
  mutations to the extracted core, each confirmed applied and each fatal —
  `TestABilledFailureBeforeASuccessIsStillPersisted`,
  `TestAKilledRunResumesWithTheMoneyItAlreadySpent`, `TestAPanicDoesNotTakeTheMoneyWithIt`, and
  `TestAnEventWriteFailureDoesNotDestroyThePaidWorkItReportsOn` all fail against a broken core, so
  the suite genuinely covers what moved.


- **`Store.ScoreSum` returns a `ScoreSummary`** rather than `(float64, int, int, error)` — a
  public Go API break, permitted pre-1.0. It repays [debt #31](docs/debt.md#31): a scored row with
  no readable number has two possible causes, a purge before the score lived in a column of its
  own, or a binary that predated the column, and reporting both as a purge sends a user looking
  for a deletion nobody performed.

  The discriminator is the **Score blob**. `score_proto IS NULL` means a purge took it before the
  number had a column of its own to survive in — the migration that added `score_value` backfills
  it out of the blob, and a purged row is the one row the backfill cannot reach. A surviving blob
  with no `score_value` is what a binary predating the column leaves behind in an already-migrated
  database, since nothing re-runs the backfill.

  `CaseScores` returns `map[string]CaseScore`, not `map[string]float64`, for the neighbouring
  reason: absent must mean *never scored*, because a pair built against a Case whose number was
  purged is not a pair with a zero in it — it is a pair that cannot be formed, and a zero there is
  indistinguishable from a real score of zero.

- **The CLI test suite runs on an environment allowlist instead of a credential denylist.**
  `cli/main_test.go` used to unset eleven named provider variables; it now names the three
  variables its tests may see (`PATH`, `HOME`, `TMPDIR`, each with a written reason) and clears
  everything else before the first test runs. `KNO_LIVE_TESTS=1` still leaves the environment
  alone.

  A denylist protects against the providers somebody thought of and stops protecting silently at
  the one they did not — which is the same failure mode as the bug it was added for. It also could
  not cover `--key-env host=VAR`, which accepts *any* variable name, so any variable a test could
  read was a variable a test could mail to a provider.

  Two tests defend the list: one asserts the surviving environment is a subset of it (set
  membership, not a guess at what a credential looks like), and one refuses an allowlist entry
  whose name could plausibly hold a secret — so "the test failed, so I allowlisted it" is not a
  two-line fix for the exposure. A canary variable planted before the scrub makes the guard
  self-verifying: delete the scrub and both tests go red. Repays
  [debt #63](docs/debt.md#63).

- **The release pipeline could not have released.** Two defects, each fatal on the first tag, both
  found in Phase-3 review and both reproduced. `make release` was the only one of four release
  targets with no goreleaser prerequisite, so the workflow — which installs cosign and syft but
  never builds the repo's own tools — would have exited 127 with no archives, no signature and no
  attestation. And `"draft": true` in `release-please-config.json` meant **no git tag was ever
  created**: GitHub creates the ref when a release is published, so the tag trigger could not fire,
  `workflow_dispatch` had no tag to select, and the `RELEASE_PLEASE_TOKEN` that repays
  [#74](docs/debt.md#74) would not have helped, because it changes the actor on a ref push that was
  not happening. The draft now lives in `.goreleaser.yaml`, where it hides the release until the
  artifacts are on it.

- **`--cost-per-call-usd` is no longer required alongside `--max-cost-usd`** for an agent that
  prices its own calls. `--agent anthropic:claude-opus-5 --max-cost-usd 5` was refused even though
  the adapter prices every Case exactly — and the scalar the user was then forced to supply is
  *ignored*, because the estimator path never falls back to it. The flag was mandatory, inert, and
  the only way to run the flagship invocation.

- **BREAKING (pre-1.0): `openaicompat.Options.KeyBindings` and `.Policy` are replaced** by
  `KeyEnv map[string]string`, `AllowInsecureBaseURL bool`, and `AllowPrivateAddress bool`. Both old
  fields were typed by the `internal/transport` package, which made the whole struct
  **unconstructible from outside `adapters/agent/`** — including from `cli`. `anthropic.Options`
  never had this problem; this brings the two adapters into line rather than weakening the
  transport's internal boundary. **Migration:** `KeyBindings: transport.KeyBindings{…}` becomes
  `KeyEnv: map[string]string{…}`; `Policy: transport.Policy{AllowInsecureHTTP: a,
  AllowPrivateAddress: b}` becomes the two bools.

  The map is **not** pre-normalized: the adapter runs it through
  `transport.ParseKeyBindings` itself, so host casing and ports are handled and the
  looks-like-a-secret and bound-twice refusals still apply. A caller that previously built a
  `transport.KeyBindings` by hand was getting that for free and still is.

- **`anthropic.Options` gains `Price`**, so `--price-input-per-mtok` / `--price-output-per-mtok`
  reach this adapter. They were accepted, validated as a pair, and then discarded for this scheme,
  while the cookbook, the CI recipe, and `kno doctor` all named them as the remedy for an unpriced
  model — a silently ignored flag on the money path.

- **An explicit `--cost-per-call-usd 0` asserts the calls are free.** Read from whether the flag
  was passed, not from its value: 0 is also the default, so the documented local-model-server
  recipe passed it and was refused with a fix line naming the flag it had just supplied.


- **BREAKING (pre-1.0): `BaselineOptions.ResolvedModel` is removed.** It was caller-supplied and
  read at run open, before any request, so the only value it could ever hold was one a previous run
  had recorded — `checkResumable` compared it to itself and the gate never fired once. **Migration:
  delete the field; nothing replaces it.** The check now runs at first-response time and needs no
  caller input. Repays [debt #42](docs/debt.md#42).

- **A provider failure that cannot change within a run now ends the run at the first Case.** Six
  conditions escalate: a rejected credential (401/403), the provider's own spend cap, a user's
  self-set spend limit, an unpaid account (402), a model that does not exist (404), and a refused
  destination or key-binding mismatch. A wrong `ANTHROPIC_API_KEY` on a 10,000-Case run previously
  made 10,000 requests and settled 10,000 calls against `--max-calls` before saying anything.
  Repays [debt #47](docs/debt.md#47).

  A plain 429, a 5xx, a truncation, and an oversized response are deliberately **not** escalated —
  each may succeed on the next call, and escalating them would convert a recoverable run into a
  dead one. That direction has its own test.

- **A capped run against a model with no price row is refused once**, naming pricing, instead of
  erroring every Case and reporting "too many cases errored" — a verdict naming nothing about
  pricing, after taking consent for a figure that was never going to apply. **Partly** repays
  [debt #46](docs/debt.md#46): this is the refusal shape only. The price rows and the per-token
  override land with the CLI wiring.

- **A timed-out request reports `RETRY_REASON_TIMEOUT`.** A 408 and a 5xx share the
  `ErrTransportTransient` sentinel, so `core` — which could classify only from sentinels — reported
  a timeout as `RETRY_REASON_PROVIDER_UNAVAILABLE`, whose schema definition is "the provider
  returned a 5xx". The enum value existed and nothing ever emitted it. Repays
  [debt #53](docs/debt.md#53).

- **Orphaned spend from a run-fatal stop is no longer attributed to the budget.** The
  discriminator was a bool, so every in-flight charged Case on any fatal stop reported "the cost or
  call cap could not admit another attempt" — sending a user whose credential was rejected to raise
  a cap that was never binding.

- **`OrphanReason.ORPHAN_REASON_RUN_FATAL`** (proto, additive). A budget stop is resumable as-is; a
  run-fatal stop is not, and resuming without fixing the condition pays for the same answer again.

- **A run-fatal refusal leaves its Case re-attemptable**, like a budget refusal and an unpriceable
  one: refused by a condition the user then fixes. Recording it as a terminal outcome put the Case
  in `CompletedCases`, so a resume skipped it forever — and `closeRun` recomputes
  `ErrorRateExceeded` over the whole store, branding the **corrected** run "not a usable baseline".
  Measured: 20 Cases, a bad key, then a resume with a healthy agent — completed, 8 errored, error
  rate exceeded, recoverable only by paying for all 20 again. The remedy every escalated error
  advertises is "fix this and re-run", and it now works.


- The fix line on a stale-checkpoint refusal no longer names a cause that is never tested. It
  offered "the goal, agent, or split configuration changed" for every non-eval mismatch: the split
  is not compared at all, and the resolved model — which is — was not named. A user whose provider
  re-pointed a moving alias was told to restore a setting they had never touched; they are now
  told to pin the model in the agent ref, which is the only thing that prevents it.

- The resume check for a changed provider model now compares **set membership** rather than the
  first recorded element. With concurrency there is no "first response", and during a provider
  rollout two workers in one run legitimately see different builds — so a run that saw `{A, B}`
  and is now served by `B` has not changed, and comparing against whichever element sorted first
  would have refused it.

  **The check still does not run.** Writing `case_execution` fills the *recorded* half of the
  comparison; nothing populates the model this process is about to use, because that is a property
  of a response and the check runs before any call. See [debt #42](docs/debt.md#42).

- **`OrphanSpend`**, naming the Case a charge belonged to when no outcome could carry it. The
  amount is recorded against the run, so without this event the money is an integer nothing
  describes — a side channel. Carries a reason, because a run stopped by a human is not a run that
  ran out of budget. Repays [debt #52](docs/debt.md#52).

- A concurrency the engine chooses is now reported rather than silent. `checkFeasible` narrows the
  width when the cost cap cannot admit what was asked for; it did so with no event, no log line,
  and no field on the `Run`. A `ConcurrencyReduced` event now says so while it is happening, and
  `Run.concurrency` records the decision afterwards — for every Case-executing run, reduced or
  not, so two runs can be compared. **Partly** repays [debt #44](docs/debt.md#44): the engine now
  records and emits it, and no surface reads it back yet. The CLI report that entry requires is
  M2-11.
- `StageProgress` heartbeats, **off by default**. Every event is one fsync under
  `synchronous=FULL` on the same serialized writer as the outcome row that prevents double-spend,
  so a heartbeat nobody watches is write contention in front of the write whose loss costs money.
  A failed heartbeat write ends the run rather than being swallowed: the append allocates a
  sequence number immediately before writing, so a silent failure leaves a permanent hole that
  `MaxEventSequence` cannot heal, and a consumer reading the stream correctly concludes it lost
  events. `--concurrency` and the interval are both bounds-checked; an unbounded `--concurrency`
  recorded a negative width on the wire.

  Nothing turns it on yet — the flag is M2-11. `core.DefaultProgressInterval` is 1 Hz. The rate is
  averaged over **this process's** work and clock: not over the last interval, because a window
  shorter than one LLM call swings on nothing, and not over the whole run, because a resume's
  counts span both processes while its clock does not. The counts themselves stay whole-run, since
  they pair with `total_cases`.

- `ConcurrencyDecision`, carried by `Run.concurrency` and by a new `ConcurrencyReduced` event, for
  a concurrency the engine chooses rather than the user. **Nothing emits or writes them yet** —
  the emitter lands with M2-10c, and until then an absent `Run.concurrency` means "not recorded",
  not "ran at what it asked for".

  The event embeds the same message the `Run` records rather than restating its fields, so the two
  cannot drift apart. It carries **both** terms of the arithmetic — the cost-cap headroom and the
  per-Case estimate — because the engine divides a fraction of the first by the second, and a
  consumer given only one can solve for what they were not told rather than check the result.
  `requested` is optional, so a width nobody asked for is distinguishable from one that was
  overridden.

  Proto only, additive, `buf breaking` clean. Toward [debt #44](docs/debt.md#44).

- **The `anthropic` provider adapter** (`adapters/agent/anthropic`) for the Messages API,
  implementing `core.Agent`, `core.Capable`, and `core.Estimator`. Not the OpenAI-compatible
  adapter with a different base URL: the system prompt is a top-level field, `max_tokens` is
  required, `input_tokens` counts only what follows the last cache breakpoint (so billed input
  is the sum of three fields), `stop_reason: "refusal"` is scored rather than errored, and
  authentication is `x-api-key`, which Go's redirect handling does not strip. Each difference
  produces a wrong number rather than a loud failure. Capabilities are static and per model, so
  `New` refuses `--temperature` for a model that rejects sampling parameters instead of failing
  every Case with a 400.

- **The first provider adapter.** `adapters/agent/openaicompat` speaks the OpenAI Chat
  Completions shape and is `base_url`-configurable, so it also reaches OpenAI-compatible
  servers. Static capabilities (no probing), a pessimistic per-Case `Estimate` with a bounded
  `WorstCase`, a prompt ceiling enforced on both the estimate and the call, and recorded
  fixtures. Repays [debt #11](docs/debt.md#11), [#18](docs/debt.md#18), [#23](docs/debt.md#23),
  [#38](docs/debt.md#38), [#39](docs/debt.md#39); investigates [#20](docs/debt.md#20) and
  [#43](docs/debt.md#43).


- `make record-fixtures` and `make test-live` no longer grep Go source for `KNO_MAX_COST_USD`.
  The cap is now read and enforced by code ([debt #11](docs/debt.md#11)), and a grep that a
  comment could satisfy would only mislead once real enforcement existed.

- A malformed `--agent` now exits with `INVALID_INPUT` rather than `CAPABILITY_UNSUPPORTED`. Both
  exit 1, so no CI gate changes, but the message now distinguishes a typo from a provider this
  build has no adapter for.
- Go toolchain bumped to `go1.25.13`. Making the first real HTTP call turned seven standard-library
  advisories from unreachable into reachable — `crypto/x509`, `net/http`, and `golang.org/x/net/idna`
  — and `govulncheck` failed the build for them. Working exactly as intended.
- Go toolchain pinned to `go1.25.8` (from 1.25.5), which `govulncheck` flagged for `GO-2026-4602`
  in the standard library, reachable from `covercheck`. `GOTOOLCHAIN` is pinned in the Makefile so
  the toolchain is as reproducible as every other tool.

### Fixed

- **`KNO_LIVE_TESTS=0` opted *in* to spending money.** `openaicompat`'s live gate tested "is the
  variable non-empty" where its two siblings test "is it exactly `1`", so the value every shell
  writes for a false boolean ran the tests that call a real provider. Found while confirming, for
  [debt #63](docs/debt.md#63), that the packages outside `cli` are still gated — they are, and this
  was the one seam where the gate read the opposite of what its value said.

- **`run.proto` said Value "works over Assets" and has no concurrency; ADR-0004 said Value "also
  executes Cases".** Both could not stand, and proto comments are the single source for the
  published OpenAPI. Value does execute Cases — it injects an Asset and re-runs them — so ADR-0004
  is right and the two comments are corrected. Its `CaseExecution` counts **measurements**, since
  200 Assets over 50 Cases attempts 10,000 measurements over 50 Cases, and a denominator of 50
  beside the spend for 10,000 calls would be two numbers in one message describing different
  populations.


- **A missing credential is refused before any request.** `openaicompat` omitted the
  `Authorization` header and let every Case collect a 401 — now bounded by run-fatal escalation,
  but still a paid round trip and a message about a rejected credential that was never sent. Only
  for the provider's default host: a self-hosted endpoint legitimately needs no key. Repays
  [debt #57](docs/debt.md#57).

- **`--yes` prints the estimate for every run, not just above the prompt threshold.** It printed
  from inside the confirmation callback, which the guard short-circuits below $1.00 — so the flag
  was silent for exactly the runs small enough not to prompt, while its help text, the cookbook,
  and the CI recipe all promised a figure unconditionally. In `--json` mode the figure travels as
  `estimated_usd` instead, because a prose line ahead of the document makes stdout unparseable.

- **A narrowed run no longer claims a width the user never asked for.** With no `--concurrency`,
  the report read `width 1 (asked for 0; cost-cap)`. Fixed in the renderer rather than by recording
  the defaulted width as a request: `core` deliberately does not, and a test pins that a report
  saying "you requested 8, we gave you 5" to someone who requested nothing is how a report earns
  distrust.

- **The test suite could bill you.** `cli`'s tests drive the real command, and a subtest asserting
  `--agent openai:gpt-4.1` was refused for having "no adapter" started making live API calls the
  moment the adapters were wired, on any machine exporting `OPENAI_API_KEY`. `cli` now has a
  `TestMain` that unsets eleven provider credential variables unless `KNO_LIVE_TESTS=1`. Tracked as
  [debt #63](docs/debt.md#63), because the list is a denylist.


- **`executor.RecordGrace` bounded the whole run instead of the drain after cancellation.** It was
  a `context.WithTimeout` built before the first item was dispatched, so on any run longer than the
  grace (30s by default, and nothing ever set it) the first sink write failed, `sinkBroken` latched
  so every later result was discarded without being asked, and a resumed run paid for all of them
  again. Invisible until now only because every test in the tree uses a sub-second in-process fake
  agent — which stops being true the moment a provider adapter is reachable from the CLI.

  The grace is now armed **by** the caller's cancellation, which is what its godoc always described.
  A hung sink is bounded separately by `PerRecordTimeout` (new, exported, 30s default) **per call**,
  because the hazard is one write that never returns, not a budget the run draws down. Repays
  [debt #54](docs/debt.md#54).

- **`executor.Options.AfterRecord`** (additive), the only path from a *successful* item to shutdown. `IsFatal`
  is consulted only on a work error, so a condition discovered in an answer the caller has already
  paid for had nowhere to go: failing the item would discard a paid, scoreable result and record it
  as an error, and returning an error from `SinkFunc` would latch `sinkBroken` and discard every
  result after it. `AfterRecord` runs once the result is durable and counted, and ending the run
  there keeps it — and a panic inside it is recovered, because unguarded it unwound out of the loop
  that drains results and deadlocked the run permanently.

  It receives the `Result` boxed as `any` rather than splintered into `(item, value, err)`:
  `Result` documents that exactly one of `Value` and `Err` is meaningful, and three loose
  parameters discard that invariant and hand the caller a non-nil `any` wrapping a nil pointer on
  the failure path.

- A sink failure that happens **after the grace has expired** now joins the caller's cancellation
  cause. Without it, a store surfacing its own error text instead of a wrapped `context.Canceled`
  turned a Ctrl-C into `RUN_STATUS_FAILED` with a generic exit code — so a CI gate keying on the
  interrupted code would flip the day a driver reworded.


- **Money spent on a Case that never produced an answer is now durable.** A budget refusal on a
  retry — or a Ctrl-C during backoff — discarded every charge the earlier attempts incurred:
  measured across a kill and resume at guard $0.36 against store $0.32. `SettledSpend` is the only
  durable record of money spent
  and `Guard.Restore` reads it, so a resumed run got the difference back as headroom and spent it
  again. Repays [debt #50](docs/debt.md#50).

  The spend is recorded against the run, not as an outcome row, so the Case stays absent from the
  completed set and a resume still re-attempts it.

  `store.Store` gains `RecordOrphanSpend`, which is a compile break for any out-of-tree
  implementation.

  **This migration cannot be downgraded past.** `kno` refuses to open a database whose schema is
  newer than the binary understands, so an older build will not start against a migrated file.

- `SettlementOvershoot` reports how much **this** settlement contributed. The figure was not
  derivable from the payload: subtracting `reserved` from `settled` over-counts by whatever
  headroom was still under the cap — 450k where the true contribution is 300k — so a consumer
  summing across events inflated the overshoot.
- A **billed** retry is reported as `PROVIDER_UNAVAILABLE` rather than `TRANSPORT_TRANSIENT`,
  which the schema defines as having *no evidence the provider processed the request*. A charge is
  evidence it did, and it is the only signal that separates the two — an adapter wraps a reset
  connection and a billed 5xx as the same sentinel. The one retry reason that costs money was
  being reported as the one that means nothing happened.

- **A provider's charge for a failed call is no longer recorded as free.** The guard settled it and
  the store persisted zero, and `SettledSpend` is the only durable record of money spent — so a
  resumed run got the difference as headroom and spent it again. With `--max-attempts 3` the guard
  could settle three charges for one Case where the store recovered at most one.

  Both paths, not just the obvious one. A Case whose first attempt is charged and fails and whose
  second succeeds is persisted by the sink's *scored* branch, which derived cost from the final
  `Response` alone — measured at $0.25 settled against $0.05 persisted. The sink now records what
  the guard settled in every branch rather than re-deriving it. Repays the core half of
  [debt #43](docs/debt.md#43); the transport half remains.
- **`Reservation.Settle` clamps what an adapter reports.** A negative charge is refused rather than
  subtracted, and a saturating one pins rather than wrapping. Unclamped, two `MaxInt64` settlements
  against a $1.00 cap left spend at **-2**, `Remaining` reporting more than the cap, and the guard
  authorizing again. `fitsLocked` and `Restore` saturate too — clamping only `Settle` moved the
  overflow into the cap comparison, where a pinned total wrapped and the guard authorized without
  limit. Repays [debt #48](docs/debt.md#48).


- A budget stop lost the Cases that were in flight when it landed. The drain cancels them mid-call,
  and each was recorded as terminally errored — which marks it complete, so `--resume` skipped it
  forever and the run reported a smaller denominator than it measured, with nothing saying why.
  Measured at concurrency 8 with a 50ms agent: two lost Cases on every run, and a resumed run
  scoring 51 of 52. CI surfaced it as an intermittent CLI failure; it was not intermittent, just
  timing-dependent, and the CLI's fake agent has no latency.

  A Case cancelled *by* the shutdown is now left unrecorded so the resume picks it up, exactly like
  a budget-refused one. A per-Case provider timeout against a healthy run is still recorded — the
  distinction matters, and collapsing it would hide a broken provider behind a shrinking
  denominator.
- `make fuzz-short` is now bounded by **executions rather than wall-clock**. `-fuzztime=30s` failed
  intermittently on both CI runners with "context deadline exceeded" — the fuzzing coordinator
  timing out on a worker as the deadline lands, not a failing input. A count also makes the gate do
  the same work everywhere, so "passes locally" and "passes in CI" stop meaning different things.
- Two `kno` processes opening the same database at the same moment could fail to start with a raw
  `SQLITE_BUSY`. Creating a database converts its journal to WAL, which needs an exclusive lock,
  and the process that loses that race is told the database is locked rather than made to wait —
  `busy_timeout` does not cover it, and setting the pragma after `journal_mode` in DSN order meant
  it was not even in effect yet. The base schema now applies under the same write lock migrations
  use, `busy_timeout` is set first, and the open path retries on a locked database with bounded
  backoff. Measured 9 of 10 runs failing before, 0 of 18 after.
- The coverage ratchet compared a platform-dependent measurement against a single-platform
  baseline, so it failed CI on Linux for code that had not changed. `executor` measures 96.0% on
  darwin and 94.9% on linux for the same commit with every test passing on both, and the 1.0pp
  jitter tolerance is not the right instrument for a systematic gap — widening a tolerance until
  the gap fits is how a gate stops detecting what it exists for. `.coverage-baseline` now holds the
  lowest reading across the platforms CI runs, and `make update-coverage-baseline` refuses to run
  anywhere but Linux, because writing it elsewhere raises the floor above what CI can meet.
- `make record-fixtures` set `KNO_LIVE_TESTS=1` itself while checking neither condition
  `make test-live` enforces: that `KNO_MAX_COST_USD` is set, and that some Go code actually reads
  it. It was an unguarded live-spend path that would have armed the moment the first adapter
  fixture recorder was written. The guard is now a shared `live_spend_guard` define both targets
  call, and both were verified to fail closed on both conditions. See `docs/debt.md#11`.
- A resume compared only the caller-supplied input fingerprint, which covers the eval file and the
  split but not the Goal or the Agent. Resuming a run with a different `--agent` or `--goal` was
  accepted, blending Cases scored under two different configurations into one `AggregateScore`
  presented as a single homogeneous number. `core.Baseline` now compares the recorded Goal, Goal
  direction, and Agent directly and refuses, naming which one changed.
- The dev/holdout refusal — a run that can never produce a holdout number — was enforced only in
  `cli/`, so any other caller of `core.Baseline` could run against an empty holdout with no refusal
  at all. The check now lives in the stage, where the docs already claimed it was.
- An interrupted run returned a bare `context.Canceled` and exited `1`, indistinguishable from a
  broken build. It now returns `errs.ErrInterrupted` and exits `4` (see Added).
- A second Ctrl-C during shutdown was silently swallowed. `signal.NotifyContext` keeps intercepting
  signals until `stop` is called, and `stop` was deferred to the end of `Execute`; it now runs as
  soon as the first signal lands, restoring the default behavior for the next one.
- A negative `--max-cost-usd` or `--max-calls` disabled the cap instead of tightening it, because
  the guard treats a limit as active only when positive. Both are now refused.
- A cost cap without `--cost-per-call-usd` failed with a bare error carrying no fix line and exit
  `1` by fallthrough. It now follows the CLI error grammar.
- A failure to write the report — a closed stdout pipe — replaced the run's own outcome, so a
  legitimate budget stop exited `1` instead of `2`. The run's error now wins.
- `budget.Guard` had no persistence, so a resumed run started at zero spent regardless of what the
  killed run had actually spent — a run near its cap could authorize nearly the whole cap a second
  time, for up to twice the intended spend across one kill/resume cycle. `Guard.Restore` reseeds
  settled spend from the store, which is the only thing that outlives the process.

[Unreleased]: https://github.com/uknoAI/kno/commits/main

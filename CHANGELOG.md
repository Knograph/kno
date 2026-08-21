# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/). Entries are derived from
[Conventional Commits](https://www.conventionalcommits.org/) by release-please.

Version policy: releases stay in the `0.x` series until 1.0 is a deliberate decision. `feat:`
bumps the patch and a breaking change bumps the minor, so reaching 1.0 requires someone to choose
it rather than a tool defaulting into it.

**Pre-1.0 compatibility:** minor bumps may break, with notice here and migration notes. After 1.0,
the proto schema, the plugin protocol, exit codes, the `kno.yaml` schema, and the public Go API are
covenants — breaking any of them requires a major version.

## [Unreleased]

### Changed

- Go toolchain pinned to `go1.25.8` (from 1.25.5), which `govulncheck` flagged for `GO-2026-4602`
  in the standard library, reachable from `covercheck`. `GOTOOLCHAIN` is pinned in the Makefile so
  the toolchain is as reproducible as every other tool.

### Fixed

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

### Added

- **`core.Estimator`**, an optional Ring-0 interface: an adapter can price a Case before the call
  is made. A cost cap the guard checks only at settlement is a cap discovered after the money is
  gone, and what a Case costs depends on the Case — a single run-scoped scalar cannot express that.
  Optional, like `Capable` and the injectors, so the fake agent and every existing caller are
  unaffected. An adapter that cannot price a Case errors that Case rather than authorizing it
  against a cheaper guess; the priced ones still run, and a run where nothing can be priced is
  marked unusable rather than quietly cheap.
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

[Unreleased]: https://github.com/knograph/kno/commits/main

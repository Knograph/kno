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

### Added

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

  A tag now produces twelve archives (darwin/linux/windows × amd64/arm64), one `checksums.txt`, a
  **keyless** cosign signature over that file, an SPDX SBOM per archive, and build provenance
  attested over every artifact the checksum file names. Keyless is the point: there is no private
  key in existence, so there is none to steal or rotate, and nothing in the workflow is a secret.

  The signature covers `checksums.txt` rather than each archive. One verification then covers
  everything, instead of twelve verifications that each say nothing about the other eleven.

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
  under which goreleaser cannot publish at all — so *nothing built on a laptop ships* is enforced
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

[Unreleased]: https://github.com/knograph/kno/commits/main

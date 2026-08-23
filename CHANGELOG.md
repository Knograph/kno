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

- **The first provider adapter.** `adapters/agent/openaicompat` speaks the OpenAI Chat
  Completions shape and is `base_url`-configurable, so it also reaches OpenAI-compatible
  servers. Static capabilities (no probing), a pessimistic per-Case `Estimate` with a bounded
  `WorstCase`, a prompt ceiling enforced on both the estimate and the call, and recorded
  fixtures. Repays [debt #11](docs/debt.md#11), [#18](docs/debt.md#18), [#23](docs/debt.md#23),
  [#38](docs/debt.md#38), [#39](docs/debt.md#39); investigates [#20](docs/debt.md#20) and
  [#43](docs/debt.md#43).

### Fixed

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

### Changed

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

### Added

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

[Unreleased]: https://github.com/knograph/kno/commits/main

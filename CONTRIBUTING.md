# Contributing to Kno

Thanks for being here. Kno is built so that external contributors are the point, not an
afterthought — Ring-1 adapters and judge prompts are the designed on-ramp, and every external PR
gets a first response within 48 hours, even if it's "reviewing this week."

This is `CLAUDE.md` (our engineering operating manual) distilled for humans. Where the two
disagree, `CLAUDE.md` wins and the disagreement is a bug — please file it.

## The short version

```bash
git clone https://github.com/knograph/kno && cd kno
make tools     # installs pinned build tools into ./bin
make check     # runs every gate CI runs on a PR
```

**macOS note:** the stock `make` is GNU Make 3.81, which predates `.SHELLFLAGS`
and ignores it silently. Every multi-command recipe therefore sets `set -euo
pipefail` itself — if you add one, start it with `$(SAFE)`. Without that, a
recipe reports the exit status of its *last* command, so a failing gate in the
middle passes.

`make check` is the only command you need to remember.

## Before you write code

**Anything over ~50 lines, or any change touching a schema or an interface, starts with a plan.**
Not bureaucracy — it is how we avoid compounding interest on shortcuts. Reviewing a design costs an
hour; unwinding it after three packages depend on it costs a week.

1. **Write a plan** in `docs/plans/YYYY-MM-DD-<slug>.md`: problem statement, proposed design,
   alternatives considered (at least two, with reasons rejected), affected packages, proto/schema
   impact, edge cases with mitigations, test plan, rollback story, docs impact.
2. **Get it adversarially reviewed.** The mandate is *"attack this plan"* — find correctness holes,
   missed edge cases, cheaper designs, hidden costs, API-compat breaks, security exposures,
   statistical validity problems. Amend until the remaining objections are explicitly accepted as
   known tradeoffs, recorded under "Accepted risks" **and** mirrored to [the Debt Ledger](docs/debt.md).
3. **Then** implement.

Small, obvious fixes (typos, a clear one-line bug) skip straight to a PR. Use judgment; if you're
unsure, open an issue and ask — that's cheaper for both of us than a rejected PR.

**Proto first.** Any change touching wire types lands the proto diff, with `buf breaking` passing,
before dependent work begins. Don't invent types; consume the generated ones.

## The vocabulary is law

Ten words, used identically in code, CLI, API, proto, tests, and docs:

**Case · Evals · Asset · Pool · Goal · Valuation · Portfolio · Destination · Bridge · Holdout**

A PR that introduces a synonym — "dataset", "item", "metric", "sample" — is wrong even if it works,
and will be sent back. Definitions are in `DESIGN.md`.

## Rules that are not negotiable

These exist because breaking them produces bugs that are expensive, silent, or dishonest:

1. **`core/` imports nothing above it.** `cli`, `tui`, and `api` are thin shells over identical
   engine calls. A PR leaking upward dependencies into `core` is rejected regardless of quality.
2. **Never spend the user's money silently.** Any path that can call an LLM or fine-tuning API goes
   through the budget guard (`stats/budget`), with estimate, confirmation, and checkpointing. A new
   spend path without them is a P0 bug.
3. **Statistical honesty is a feature.** No reported delta without its confidence interval. No
   selection without dev/holdout separation. No holdout access before Validate. Tests enforce this
   — do not weaken them to make something pass.
4. **Proto and plugin-protocol changes are breaking until proven otherwise.** Schema compatibility
   is a security boundary.
5. **Every bug fix ships with the test that would have caught it.** No test, no fix.
6. **The iterator contract is binding on every adapter.** `Evals.Cases` and `Pool.Assets` return
   `iter.Seq2`, and four rules come with it:
   - **A yielded error is fatal.** The consumer stops. If your source has malformed records, handle
     them internally and report counts via `Provenance` — never yield a skippable error. This is one
     rule rather than two on purpose: if one adapter skipped bad records and another halted, the
     denominator behind every confidence interval would vary by adapter, invisibly.
   - **Defer cleanup *inside* the iterator closure.** Early `break` is legal and expected; cleanup
     registered outside the closure will not run.
   - **Check `ctx.Err()` before each yield.** `iter.Seq2` carries no cancellation of its own, and a
     producer that ignores it keeps spending after the user hits Ctrl-C.
   - **Yielded values are borrowed for one iteration.** Clone before retaining or mutating.

   Prove it by calling `coretest.ConformIterator` in your adapter's tests, and
   `coretest.CleanupProbe` for the resource-cleanup half the harness cannot observe from outside.

## Quality gates

`make check` runs all of these, in fail-fast order. CI runs exactly the same target, so a green
`make check` locally should mean a green CI. Some gates are honestly incomplete this early — the
table says which, and each names the ledger entry that retires it. A gate that quietly passes when
it did not run is worse than no gate, so they announce themselves as `PEND`.

| Command | What it checks |
|---|---|
| `make lint` | golangci-lint. Zero tolerance. No `//nolint` without a justification and a linked issue |
| `make test` | Unit tests, always `-race` and `-shuffle=on`. Coverage floors: 85% on `core`, `stats`, `bridge`, `plugin`; 70% repo-wide, and coverage may not decrease |
| `make test-integration` | Adapter tests against recorded fixtures. Runs inside `make test`, so it gates every PR. It **refuses to run** if `KNO_LIVE_TESTS` is set |
| `make test-live` | Integration tests against live providers. Spends real money, never runs in PR CI, and refuses to start unless a budget cap is set *and* some code actually reads it |
| `make typecheck-proto` | `buf lint` + `buf breaking` against main |
| `make fuzz-short` | 30s fuzz on parsers. No fuzz targets exist yet; it discovers them automatically, so it starts working the moment you add one ([docs/debt.md#4](docs/debt.md)) |
| `make vuln` | `govulncheck` over the shipping module. The `tools/` module is **not** scanned yet ([docs/debt.md#12](docs/debt.md)) |
| `make docs` | Will regenerate OpenAPI and check godoc coverage. **Both are pending** — `godoccheck` lands with M0c and OpenAPI needs a proto service to exist. It reports what it did not run rather than passing quietly |
| `make bench-diff` | **Currently a tripwire, not a comparison.** No benchmarks exist yet, so it passes. The moment you add the first `func Benchmark`, it fails deliberately and asks you to implement the >10% regression gate ([docs/debt.md#3](docs/debt.md)) — that is the forcing function, not a bug |

**New dependencies need justification in the PR body:** what it does, why the standard library or
an existing dependency can't, its license, and its maintenance signal.

## Tests

- **Table-driven, `t.Parallel()` by default**, subtests named after the scenario *in vocabulary
  terms*.
- **Determinism first.** Anything LLM-dependent is tested against recorded fixtures in
  `testdata/fixtures/` (regenerate with `make record-fixtures`; secrets are scrubbed at record
  time). Judges are tested against the human-labeled calibration set with agreement thresholds.
- **Statistical code gets statistical tests** — property-based, with invariants like holdout
  isolation asserted directly.
- **Golden files** for report rendering and CLI output; `make update-golden` regenerates, and the
  diff is reviewed like code.
- **Any package that owns goroutines installs `goleak.VerifyTestMain`** in a `TestMain`. That means
  every adapter package and the shared transport: connection pools, rate-limiter timers, and
  request timeouts all outlive the call that started them, and a leak there shows up as a run that
  will not exit rather than as a failing test.

  `VerifyTestMain`, not a per-test helper. goleak takes a process-global census, so a parallel
  sibling's goroutines are indistinguishable from a leak — once per package is the only form that
  is not flaky, and a flaky gate gets deleted rather than fixed. See `docs/debt.md#18`.
- **Flaky tests are quarantined within 24 hours** with an issue, then fixed or deleted within a
  week. Retries are never the fix.

  "Fixed" means the code, not the test. A test of this project's own making went flaky on `main`
  within minutes of merging; the cause was two processes racing to create a database, and the fix
  was to serialize that — not to loosen the assertion.

## Style

- Idiomatic Go over clever Go. `gofumpt` formatting. Accept interfaces, return structs.
- `context.Context` is the first parameter, always propagated, never stored in a struct.
- Errors wrapped with `%w` and operation context. User-facing errors follow the CLI grammar —
  **what failed → why → how to fix** — via `errs.Actionable`, the same struct the API serializes.
- No `util`, `common`, `helpers`, or `misc` packages. If you can't name it, it doesn't belong
  together.
- Extract on the third occurrence, not the second. Duplication is cheaper than the wrong
  abstraction, especially in adapters.
- Soft caps: ~400 lines per file, ~60 per function. Exceeding either is a smell to justify, not a
  rule to game.
- Every exported symbol has a godoc comment. `make docs` will enforce this once `godoccheck` lands
  (M0c); until then it is reviewed by hand.

## Commits and PRs

- **[Conventional Commits](https://www.conventionalcommits.org/)** — `feat:`, `fix:`, `docs:`,
  `perf:`, `refactor:`, `test:`, `build:`. The changelog and version numbers are derived from them.
- **Sign off every commit** (`git commit -s`). We use the
  [DCO](https://developercertificate.org/), not a CLA — lower friction, sufficient for Apache-2.0.
- Branches: `feat/<slug>`, `fix/<slug>`, `docs/<slug>`, and short-lived (under ~3 days of work —
  split bigger efforts behind the plan).
- PRs are squash-merged; **the PR title becomes the commit message**, so write it as one.

**Definition of Done:** plan linked · both adversarial reviews recorded · `make check` green · docs
updated (godoc, CLI help, OpenAPI, and the mental-model or cookbook page if user-visible) ·
CHANGELOG entry under `Unreleased` · vhs tape re-recorded if CLI output changed.

## Security

Do not open a public issue for a vulnerability — see [SECURITY.md](SECURITY.md).

Two rules that catch people out: **API keys come from the environment or the OS keychain only**
(never `kno.yaml`, never fixtures, never logs), and **stored traces are customer data** — they may
contain end-user conversation content, so no trace content appears in log lines above DEBUG and
none ever goes into telemetry.

## Where to start

`good-first-issue` here is a curated pipeline, not a label of neglect — each one has context,
pointers, and a test to make pass. The natural on-ramps are **Ring-1 adapters** (a new
OpenAI-compatible endpoint, a pool format) and **judge prompts**. Several entries in
[the Debt Ledger](docs/debt.md) are also well-scoped starting points.

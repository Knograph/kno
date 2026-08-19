# CLAUDE.md — Kno Engineering Operating Manual

You are working on **Kno** (`github.com/knograph/kno`): a Go engine + CLI + API that measures the marginal value of data assets for LLM agents and curates portfolios for context, RAG, and fine-tuning. Read `DESIGN.md` first — it is the source of truth for architecture, vocabulary, and scope. This file is the source of truth for **how we work**. Where they conflict, stop and flag it; do not silently pick one.

## Prime directives

1. **Plan before code. Adversarial review before implementation. No exceptions.**
2. **The vocabulary is law.** Case, Evals, Asset, Pool, Goal, Valuation, Portfolio, Destination, Bridge, Holdout — used identically in code, CLI, API, proto, tests, and docs. A PR that introduces a synonym ("dataset", "item", "metric") is wrong even if it works.
3. **`core/` imports nothing above it.** cli/tui/api are shells over identical engine calls. Any PR that leaks upward dependencies into core is rejected regardless of quality.
4. **Never spend the user's money silently.** Any code path that can call an LLM or FT API must flow through the budget guard (`stats/budget`). New spend paths without estimate + confirm + checkpoint are P0 bugs.
5. **Statistical honesty is a feature.** No reported delta without its CI. No selection without dev/holdout separation. No holdout access before Validate. Tests enforce this; do not weaken them to make something pass.
6. **When in doubt about proto or plugin-protocol changes, they are breaking.** Treat schema compatibility as a security boundary.

## Workflow: every non-trivial change

**Why this process exists:** adversarial review is our tech-debt prevention strategy. We pay the review cost up front, at design time, when changing course is cheap — instead of paying compound interest on shortcuts as the codebase matures. Debt we *choose* to take on is fine; debt we drift into is not. Every piece of accepted debt is recorded, owned, and revisited on a schedule (see the Debt Ledger below) — a risk that is accepted and then forgotten is exactly the failure mode this process exists to prevent.

**Phase 0 — Plan (required for anything > ~50 LOC or any schema/interface touch).**
Write a plan in `docs/plans/YYYY-MM-DD-<slug>.md` containing: problem statement, proposed design, alternatives considered (min 2, with reasons rejected), affected packages, proto/schema impact (breaking? migration?), edge cases enumerated with mitigations, test plan (unit/integration/fixture needs), rollback story, and docs impact (which of: godoc, CLI help, OpenAPI, mental-model page, cookbook).

**Phase 1 — Adversarial review of the plan.**
Spawn a reviewer subagent with the instruction: *"Attack this plan. Find correctness holes, edge cases missed, cheaper designs, hidden costs, API-compat breaks, security exposures, and statistical validity problems. Order findings by damage."* The plan is amended until the reviewer's remaining objections are explicitly accepted as known tradeoffs (recorded in the plan under "Accepted risks" **and** mirrored to the Debt Ledger). Only then does implementation start.

**Phase 2 — Implement in parallel workstreams.**
Decompose along package boundaries and spawn subagents per workstream where dependencies allow:

- `core`/`stats` (pipeline + statistics) — sequential with itself, parallel to everything else
- `adapters/*` — each adapter is an independent workstream (openai-compatible, anthropic, tuners, pools)
- `judge` (BAML prompts + calibration) — independent
- `cli` + `tui` — parallel once the event schema for the change is fixed
- `api` — parallel once proto is merged
- `docs` + `examples` + vhs tapes — parallel, started from the plan, not after the code

Coordination rule: **proto first.** Any change touching wire types lands the proto diff (with `buf breaking` passing) before dependent workstreams begin. Subagents must not invent types; they consume generated code.

**Phase 3 — Adversarial review of the code.**
A second reviewer subagent reviews the diff with a different mandate than CI: *"Assume the tests pass and lint is green. Find what's still wrong: race conditions, unchecked errors, context leaks, budget bypasses, holdout leakage, vocabulary drift, missing capability checks, log lines that could contain secrets or trace content, docs that no longer match behavior."* Findings are fixed or explicitly accepted in the PR description — and any accepted finding that constitutes debt is mirrored to the Debt Ledger.

**Phase 4 — Gates, then merge.** See Quality Gates. Merges to `main` are squash-only, linear history, via PR. No direct pushes to `main`, including by maintainers, including "trivial" fixes.

## The Debt Ledger

`docs/debt.md` — one table, the only place accepted debt lives. Every entry: what was deferred, why (link to the plan or PR), the trigger for repayment (a date, a milestone like "before 1.0", or a condition like "when a second Tuner lands"), and an owner. Rules:

- Adding an entry requires a repayment trigger. "Someday" is not a trigger; an entry without one is a rejected review finding, not accepted debt.
- The ledger is reviewed at every minor release: each entry is repaid, re-dated with a written reason, or promoted to "won't fix" with the rationale moved into an ADR. Silent carryover is not an option — CI fails a release tag if any ledger entry's trigger has lapsed without a disposition.
- `TODO`/`FIXME`/`HACK` comments in code must reference a ledger entry (`// DEBT(debt.md#42): ...`) or an issue; the lint bundle rejects naked ones. If it's worth a comment, it's worth tracking; if it's not worth tracking, delete the comment and fix it or accept it silently — no zombie markers.
- The ledger is public. External contributors seeing an honest debt register is a trust feature, and it routes their energy — several entries will make excellent `good-first-issue`s.

## Quality gates (all must pass on every PR; CI enforces, but run locally first)

```bash
make check        # runs everything below; this is the only command you need to remember
```

- `make lint` — `golangci-lint` (govet, staticcheck, errcheck, gosec, revive, gocritic, misspell, godot). Zero tolerance; no `//nolint` without a justification comment and a linked issue.
- `make test` — unit tests, `-race` always, `-shuffle=on`. Coverage floor: **85% on `core/`, `stats/`, `bridge/`, `plugin/`; 70% repo-wide**. Coverage may never decrease on a PR (ratchet, enforced by CI diff).
- `make test-integration` — adapter tests against **recorded fixtures** (see Testing). Live-API tests are opt-in (`KNO_LIVE_TESTS=1`), never in PR CI, run nightly with a capped budget.
- `make typecheck-proto` — `buf lint` + `buf breaking --against main`. Breaking proto changes require a version bump plan in the PR description.
- `make fuzz-short` — 30s fuzz on parsers: plugin handshake, NDJSON frames, agent-ref parser, kno.yaml loader. (Nightly runs longer.)
- `make vuln` — `govulncheck` + dependency audit. New dependencies require justification in the PR body (what it does, why stdlib/existing deps can't, license, maintenance signal).
- `make docs` — regenerates OpenAPI from proto, godoc coverage check (every exported symbol documented), CLI help snapshot tests, link checker on `docs/`. **A PR that changes behavior without a docs diff fails this gate** (enforced via a behavior-change label check).
- `make bench-diff` — benchmarks on hot paths (scoring loop, routing, NDJSON framing) compared to `main`; >10% regression fails unless the plan declared and justified it.

**Definition of Done for any PR:** plan linked (Phase 0), both adversarial reviews recorded, `make check` green, docs updated (godoc + CLI help + OpenAPI + mental-model/cookbook if user-visible), CHANGELOG entry under `Unreleased`, vhs tape re-recorded if CLI output changed.

## Testing strategy

- **Determinism first.** LLM-dependent code is tested against recorded fixtures (`testdata/fixtures/`, recorded via `make record-fixtures`, secrets scrubbed at record time). Judges are tested against the human-labeled calibration set with agreement thresholds — a judge prompt change that drops agreement below threshold fails CI.
- **Statistical code gets statistical tests.** `stats/` is property-tested (rapid or gopter): CI coverage properties, holdout-isolation invariants ("no code path reads holdout before Validate" is an actual test using a canary case), winner's-curse regression test with synthetic data where ground truth is known.
- **Table-driven tests, `t.Parallel()` by default,** subtests named after the scenario in vocabulary terms. Golden files for report rendering and CLI output (`make update-golden` to regenerate, diffs reviewed like code).
- **Every bug fix ships with the test that would have caught it.** No test, no fix.
- **Integration tests own the seams:** adapter capability matrices, plugin handshake versioning (including a deliberately-misbehaving plugin in `testdata/`), resume-from-checkpoint (kill mid-run, resume, assert no double-spend and identical results), exit codes.
- **Flaky policy:** a flaky test is quarantined within 24h with an issue, fixed or deleted within a week. Retries are never the fix.

## Code style & organization

- Idiomatic Go over clever Go. `gofumpt` formatting. Accept interfaces, return structs. Contexts are the first parameter, always propagated, never stored.
- **Errors:** wrapped with `%w` and operation context; sentinel errors in `core/errs`; user-facing errors implement the CLI grammar (`what failed → why → fix`) via `errs.Actionable{Code, Message, Fix, DocsURL}` — the same struct the API serializes. Panics are for programmer error only; the CLI top-level recovers, prints a bug-report template, exits 1.
- **DRY with judgment:** extract on the third occurrence, not the second. Duplication is cheaper than the wrong abstraction, especially in adapters.
- Package names are the vocabulary. No `util`, `common`, `helpers`, `misc` packages — if you can't name it, it doesn't belong together.
- Exported surface is deliberate: every exported symbol has godoc; internal packages (`internal/`) for anything not part of the public Go API. The public Go API is `core`, `adapters` interfaces, and generated proto — nothing else is stability-promised pre-1.0.
- File size soft cap ~400 lines; function soft cap ~60. Exceeding either is a smell to justify, not a rule to game.

## Security

- **Secrets:** API keys only via env or OS keychain; never in kno.yaml, never in fixtures, never in logs/traces/error messages. A redaction layer sits in the logger and the trace store; `make test` includes a secrets-in-output scan (gitleaks on the repo, custom scan on golden files and fixtures).
- **Traces are customer data.** Stored traces (SQLite) may contain end-user conversation content: no trace content in log lines above DEBUG, a `kno purge` command exists, and the docs state retention behavior plainly. Never ship trace content in telemetry (see Observability).
- **Plugin boundary is hostile.** Ring-2 plugins are untrusted input: handshake and every frame validated against schema, timeouts and output-size caps on plugin I/O, plugin stderr is logged but never parsed, and plugins get no ambient credentials — they receive only what config explicitly grants them. Fuzz the frame parser (see gates).
- **Supply chain:** dependencies pinned via go.sum + Dependabot; releases built by goreleaser in CI with SLSA provenance, cosign-signed artifacts, SBOM (syft) attached, checksums in release notes. Nothing built on laptops ships.
- `SECURITY.md` with a private disclosure channel and a 90-day response commitment. gosec in the lint bundle; `govulncheck` gate above.

## Performance

- The engine's job is orchestration: the budget is **LLM latency and dollars, not CPU**. Optimize for (a) never re-spending (checkpointing correctness), (b) maximal safe concurrency (bounded worker pools, per-provider rate limiters honoring Retry-After), (c) streaming memory profile (iterators end-to-end; a 1M-case eval set must not load into RAM — `iter.Seq` is load-bearing, keep it).
- Benchmarks live next to hot code (`_bench_test.go`); `make bench-diff` gates regressions. Profiles (`pprof`) checked into `docs/perf/` for the scoring loop as a baseline; re-baseline deliberately, not accidentally.

## Observability

- **Structured logging** (charmbracelet/log): human-first at INFO, machine-parseable with `--json`. Every log line answers "what stage, what run ID, what case/asset". No naked `fmt.Println` outside `tui/`.
- **OTel tracing** built in: every stage, every adapter call, every plugin invocation is a span with run ID correlation. Local default is off; `--otel-endpoint` turns it on. Spans never contain conversation content or asset content — IDs and metrics only.
- **The event stream is the single spine:** engine emits typed events (proto-defined); tui renders them, api streams them (SSE), logs record them. New user-visible state = new event type, never a side channel.
- **Product telemetry is OPT-IN only** (`telemetry: true` in kno.yaml, off by default, prominently documented): anonymous command + version + duration + error class. Never content, never counts of user data, never endpoints. This audience will read the telemetry code — write it to be read.

## Version control & releases

- **Conventional Commits** (`feat:`, `fix:`, `docs:`, `perf:`, `refactor:`, `test:`, `build:`) — changelog and semver are derived from them (release-please). Squash-merge keeps one commit per PR; the PR title is the commit.
- Branch naming: `feat/<slug>`, `fix/<slug>`, `docs/<slug>`. Branches are short-lived (< ~3 days of work; bigger efforts split behind the plan).
- **SemVer with teeth:** pre-1.0, minor bumps may break with CHANGELOG notice + migration notes; post-1.0, proto, plugin protocol, exit codes, kno.yaml schema, and the public Go API are all covenants — breaking any requires a major. `buf breaking` and a plugin-protocol conformance suite enforce mechanically.
- Releases: tag → goreleaser → signed multi-platform binaries (darwin/linux/windows, amd64/arm64) + Homebrew tap + install script. Release notes generated from Conventional Commits, hand-edited for the top 3 highlights. Docs are versioned per minor release.

## Documentation (updated on every branch, gated at merge)

- **API reference:** OpenAPI generated from proto comments (`make docs`) — proto comments are the single source; the spec is served by `kno serve` at `/openapi.json` and rendered with **Scalar** at `/docs`. The published docs site embeds the same Scalar reference. Handwritten API prose is forbidden; fix the proto comment instead.
- **Repo docs:** README (with the vhs quickstart GIF), DESIGN.md (architecture truth), CONTRIBUTING.md (this workflow, distilled for humans), the mental-model page, cookbook entries, and *What the numbers mean* (the epistemics page). If a PR changes what a number means, that page changes in the same PR.
- **Internal docs:** `docs/plans/` (Phase-0 plans, kept forever as decision records), `docs/adr/` for architecture decisions that outlive a single plan, `docs/perf/` baselines.
- **CLI docs are code:** help text lives with commands, snapshot-tested; manpages and completions generated by fang at release.
- Docs PR checklist is enforced by the `make docs` gate + a PR template checkbox that CI verifies against the diff.

## Community & governance (external developers are the point)

- `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, issue templates (bug/feature/plugin), PR template with the DoD checklist, `CODEOWNERS` routing reviews (core/stats → maintainers; adapters → adapter owners).
- **License: Apache-2.0** (patent grant matters for enterprise adoption). **DCO sign-off** (`git commit -s`), not a CLA — lower contributor friction, sufficient for Apache-2.0.
- `good-first-issue` is a curated pipeline, not a label of neglect: each one has context, pointers, and a test to make pass. Ring-1 adapters and judge prompts are the designed on-ramp.
- Every external PR gets a first response within 48h, even if it's "reviewing this week."

## Agent parallelization quick-reference

| Workstream | Can start when | Blocks |
|---|---|---|
| proto/schema | plan approved | everything typed |
| core + stats | proto merged | api, cli behavior |
| each adapter | Ring-0 interfaces stable | its own fixtures only |
| judge prompts | calibration set exists | routing-dependent tests |
| cli + tui | event schema fixed | vhs tapes |
| api | proto merged | SDK generation |
| docs + examples + tapes | plan approved (yes, before code) | release |

Subagent hygiene: each workstream gets the plan, this file, and only its package scope; no subagent edits proto or another workstream's packages; integration happens through generated types and the event schema, reviewed at Phase 3 as one diff.
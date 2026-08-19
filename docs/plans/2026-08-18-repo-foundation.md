# Plan: Repository Foundation (Milestone 0)

- **Date:** 2026-08-18
- **Status:** M0a merged-ready (Phases 0–4 complete). M0b/M0c pending.
- **Author:** DeVaris Brown
- **Phase-1 reviewers:** two independent adversarial passes (architecture/schema/statistics; Go/proto
  mechanics). Both returned **BLOCK**. Findings and dispositions in "Phase-1 review record" below.
- **Branches:** `feat/governance-and-gates` (M0a) · `feat/proto-contract` (M0b) · `feat/ring0-surface` (M0c)

## Problem statement

The repository contains `DESIGN.md`, `CLAUDE.md`, `README.md`, an empty `docs/debt.md`, and two
reviewer agent definitions. There is no Go module, no proto, no `Makefile`, no CI, and no
community files.

This makes `CLAUDE.md` **unexecutable**. Every gate it mandates (`make check`, `make lint`,
`make test`, `make typecheck-proto`, `make fuzz-short`, `make vuln`, `make docs`,
`make bench-diff`) refers to machinery that does not exist. The Definition of Done cannot be met
by any PR, including this one, until the machinery lands. The coverage ratchet has no baseline to
ratchet from.

Additionally, `CLAUDE.md`'s Phase-2 coordination rule is **proto first**: "Any change touching wire
types lands the proto diff (with `buf breaking` passing) before dependent workstreams begin." Every
v0.1 workstream touches wire types, because `DESIGN.md` states all supporting types (`Case`,
`Score`, `Valuation`, `Portfolio`, `Report`) are defined once, in protobuf. Proto is on the critical
path for everything.

**Milestone 0 delivers two things: the contract (proto + Ring-0 Go interfaces) and the machinery
that enforces the process (gates, CI, community files).** It ships **no pipeline logic, no adapters,
and no LLM call paths.**

### Non-goals for M0 (explicitly deferred)

- Any pipeline stage implementation (Baseline, Value, Select, Validate, Export).
- Any adapter (`adapters/agent`, `adapters/evals`, `adapters/pool`, `adapters/tuner`).
- Any CLI verb beyond `kno version`.
- BAML judges, calibration set, `judge/`.
- Ring-2 plugin protocol and its handshake schema (`DESIGN.md` schedules Ring 2 for v0.3
  *deliberately*, so the protocol freezes only after Ring-0 survives contact with users).
- **`event.proto` and the `Run` resource** — cut from M0 in Phase-1 review (finding A5). They land
  with Baseline in M1, designed against a real producer.
- vhs tapes, goreleaser release pipeline, SDK generation.

## Milestone structure — three PRs, not one

Phase-1 finding A4: M0 as originally planned was weeks of review surface in one diff, violating
`CLAUDE.md`'s own "<~3 days of work" branch rule — and self-contradicting, since `CLAUDE.md`'s own
Phase-2 table lists docs/CI-shaped work as parallel to the proto critical path. Split:

| PR | Branch | Contents | Depends on |
|---|---|---|---|
| **M0a** | `feat/governance-and-gates` | Go module, license/governance/community files, `Makefile`, `.golangci.yml`, CI workflows, Dependabot, release-please, `docs/debt.md`, `CHANGELOG.md`, this plan, ADRs | nothing |
| **M0b** | `feat/proto-contract` | `proto/kno/v1/*.proto`, `buf.yaml`/`buf.gen.yaml`, checked-in `gen/`, proto tests | M0a (needs the gates to run against) |
| **M0c** | `feat/ring0-surface` | `core` interfaces + aliases, `core/errs`, `stats/budget`, `coretest` conformance harness, `covercheck`, `godoccheck`, import-boundary test | M0b (needs generated types) |

M0a and M0b are authored in parallel; M0b merges after M0a so its CI runs against real gates.

---

## PR M0a — Governance and gates

### Module and layout

- Module `github.com/knograph/kno`, Go 1.25.
- Directory skeleton per `DESIGN.md` "Repository layout". Directories M0 does not populate get a
  `doc.go` stating the package charter and its milestone — legible layout, meaningful
  `go build ./...`, and git can't commit empty directories anyway.
- **Deviation from `DESIGN.md`'s layout, flagged not silent:** generated proto code lands in
  `gen/kno/v1/`, a top-level directory `DESIGN.md` does not list. `DESIGN.md` lists `proto/` as "THE
  contract" (the `.proto` sources) and is silent on generated Go. Addition, not conflict. ADR-0002.
  **Confirmed by the user.**

### Tooling — native `tool` directive, no `tools.go`, no remote plugins

Phase-1 findings G7 and G8, both **verified empirically on this machine** rather than accepted on
assertion:

- Go 1.24 shipped the `tool` directive in `go.mod`, superseding the blank-import `tools.go` trick.
  Verified: `go mod edit -tool=…` writes a `tool` line under go 1.25.5. All tools
  (`protoc-gen-go`, `buf`, `golangci-lint`, `gofumpt`, `govulncheck`) are pinned this way.
- Consequently `protoc-gen-go` is a pinned local tool like everything else. The originally planned
  **buf remote plugins are dropped** — they added a network dependency and third-party binary
  execution at codegen time (a supply-chain surface `CLAUDE.md`'s Security section otherwise takes
  seriously), to solve a problem `go tool` already solves. Self-inconsistent; removed.

### `.golangci.yml` — v2 schema, verified before writing

Phase-1 finding G4 was **confirmed by direct probe**: golangci-lint v2.12.2 rejects a v1-shaped
config with `can't load config: unsupported version of the configuration: ""`. v2 requires a
top-level `version: "2"`, moves settings under `linters.settings`, exclusions under
`linters.exclusions`, and **splits formatters (`gofumpt`) into their own top-level `formatters:`
block** invoked by `golangci-lint fmt`. A config of the corrected shape was validated with
`golangci-lint config verify` before being committed.

Linters: govet, staticcheck, errcheck, gosec, revive, gocritic, misspell, godot. Formatters:
gofumpt. `gen/` and `*.pb.go` excluded — generated code is not ours to lint (finding G9 notes the
cost of this; see Accepted risks).

Three rules exist specifically to hold the alias design together (finding G1/G2, see M0c):

- **depguard**: `encoding/json` is banned outside `api/`. Proto3 JSON mapping requires int64 as
  *quoted strings* and enums as *names*; `encoding/json` emits bare numbers and raw ints, silently
  diverging from the OpenAPI spec `make docs` generates from the same proto. `protojson` only.
- **forbidigo**: `reflect.DeepEqual` is banned repo-wide. Proto messages carry `DoNotCompare`
  (`[0]func()`); comparing them through `any` is a **runtime panic**, not a compile error, and
  `go vet` cannot see it. Tests use `google.golang.org/protobuf/testing/protocmp`.
- **`make check` ordering**: fail-fast cheapest-first (fmt → lint → test → proto → vuln → docs), so
  the gate is actually run locally. Slow gates (fuzz, integration) stay separate targets, matching
  `CLAUDE.md`'s own listing.

### Gate machinery

`Makefile` targets: `check`, `lint`, `fmt`, `test`, `test-integration`, `typecheck-proto`,
`generate`, `fuzz-short`, `vuln`, `docs`, `bench-diff`, `record-fixtures`, `update-golden`,
`update-coverage-baseline`, `tools`.

**Coverage ratchet** (`internal/cmd/covercheck`, lands in M0c; target stubbed in M0a):

- Per-package floors — 85% `core`, `stats`, `bridge`, `plugin`; 70% repo-wide — plus a no-decrease
  rule against a committed `.coverage-baseline`.
- Finding G6 fix: a package is exempt **only if it contains no non-test `.go` file with a function
  body**. Keying exemption off "no entry in the coverage profile" would exempt the dangerous case —
  a package with real code and zero `_test.go` files — permanently and silently. Exempt packages
  are listed in output, never hidden.
- Baseline is one line per package, sorted, so conflicts are per-package and mechanically
  resolvable; `make update-coverage-baseline` regenerates. A PR that deletes covered code is
  handled by comparing per-package percentage against the floor, not against raw statement counts.

**Secrets scan**: gitleaks in CI. Locally, targets needing an absent tool print a loud named SKIP —
confirmed missing on this machine: `gitleaks`, `vhs`, `gum`, `baml-cli`. A gate that silently
no-ops is worse than no gate.

### Bootstrap gates — pinned, not open-ended

Finding A8 killed the original "skip when `proto/` is absent" heuristic: evaluated by directory
presence, it would silently re-disable breaking-change protection on any future merge that emptied
or renamed `proto/`.

| Gate | Bootstrap | Forcing function |
|---|---|---|
| `buf breaking --against main` | M0b is the first commit with proto, so the `main` baseline has no `.proto` files and buf reports no breaking changes — **no special case needed**. If buf errors on an empty baseline, the M0b CI run only sets `KNO_BUF_BOOTSTRAP=1`, and M0c's PR checklist requires its removal. | Removal is a checklist item on the very next PR |
| coverage ratchet | M0c writes the initial `.coverage-baseline`. **The 85%/70% floors bind on M0c's own new code** (`core/errs`, `stats/budget`) — only the *no-decrease* rule bootstraps. Finding A7: this was ambiguous; it is now explicit and is not a fourth hidden exemption. | Floors are enforcing immediately |
| `bench-diff` | No benchmarks exist and no hot path exists. Target prints "no benchmarks — placeholder" and exits 0. | Debt Ledger entry, trigger: *before the first PR touching the scoring loop, routing, or NDJSON framing merges* |
| `fuzz-short` | Fuzzes proto unmarshal of `Report` only. Finding G11 is correct that this largely fuzzes Google's parser, not ours; it is retained as a cheap panic-safety smoke test and **the target prints "PLACEHOLDER"** so a green run is never mistaken for "the security-relevant parsers are fuzzed." | Debt Ledger entry, trigger: *when the plugin handshake, NDJSON frame, agent-ref, or kno.yaml parsers land* |

### CI

`ci.yml` (`make check`, Linux + macOS, Go 1.25), `nightly.yml` (long fuzz, `KNO_LIVE_TESTS=1` under
a hard budget cap), `dco.yml` (sign-off enforcement). Dependabot for Go modules and Actions.
`release-please` in manifest mode. The goreleaser pipeline waits until there is a binary to release.

### Community and governance

`LICENSE` (Apache-2.0), `NOTICE`, `CONTRIBUTING.md` (the `CLAUDE.md` workflow distilled for humans),
`CODE_OF_CONDUCT.md` (Contributor Covenant 2.1), `SECURITY.md` (private disclosure, 90-day
commitment), `CODEOWNERS`, issue templates (bug/feature/plugin), `pull_request_template.md` carrying
the DoD checklist, `.gitattributes` marking `gen/**` `linguist-generated`.

`docs/debt.md` gets its table header and column contract, then this plan's Accepted risks.

---

## PR M0b — The proto contract

### Files

`event.proto` and a `Run` message are **cut from M0** (finding A5). Remaining:

| File | Contents |
|---|---|
| `common.proto` | `Provenance`, `CostVector`, `Kind`, `Destination`, `InjectionMode`, `AgentRef`, `Capabilities` |
| `case.proto` | `Case`, `Response`, `Score`, `Split` (dev/holdout) |
| `asset.proto` | `Asset` |
| `valuation.proto` | `Valuation`, `Interval`, `RejectionReason` |
| `portfolio.proto` | `Portfolio`, `PortfolioEntry`, `Rejection` |
| `report.proto` | `Report`, `Gap` |
| `tuner.proto` | `TuningJob`, `JobRef`, `JobStatus` |
| `error.proto` | `Actionable` — `code`, `message`, `fix`, `docs_url` |

**`tuner.proto` and `Capabilities` exist because of finding A1**, the sharpest hit of the review:
`DESIGN.md`'s `Tuner` interface references `TuningJob`, `JobRef`, and `JobStatus`, and `Capable`
returns `Capabilities` — none of the four is defined anywhere in `DESIGN.md`, and the original plan
promised to ship those interfaces "verbatim" while omitting all four from the schema. That is a
day-one compile error for the M0c workstream. `Tuner` and `Capable` are Ring-0 and frozen at v0.1
per `DESIGN.md`, so the types are defined now rather than scoped out; only *minimal* fields, since
`bridge/` (v0.2) owns the orchestration.

### Schema rules

- `buf lint` `STANDARD` + `enum_zero_value_suffix = _UNSPECIFIED`. Every enum gets an explicit
  `_UNSPECIFIED = 0`, so "unset" is never mistakable for a real value. This matters most for
  `Destination` and `Kind`, where a silent zero would read as "assigned to context" / "knowledge".
- Money is `int64` micro-USD (`cost_usd_micros`), never `double`. Float dollars accumulate error
  across thousands of calls, and spend is gated on this number.
- `Interval` is a message field, so presence already distinguishes "no CI computed" from a
  zero-width CI. Finding A10 correction: the original plan claimed the `optional` keyword provided
  this — under proto3, `optional` on a *message* field is a no-op, presence exists regardless. The
  plan text overstated the mechanism; the property holds, the explanation was wrong.
- **`Rejection` carries a reference field, not a bare enum** (finding A2). `DESIGN.md`'s Select
  stage requires `redundant_with:<id>` — a *parametrized* reason. A bare `RejectionReason` enum
  cannot express it, and restructuring the message after `buf breaking` locks it is expensive:

  ```protobuf
  message Rejection {
    string asset_id = 1;
    RejectionReason reason = 2;
    repeated string redundant_with_asset_ids = 3;  // set iff reason == REDUNDANT
    string detail = 4;
  }
  ```

- IDs are strings, ULID-formatted, documented once in `common.proto`. Adding a typed `Run` resource
  later is purely additive (finding A6).
- **`api_level = API_OPEN` pinned explicitly** in `buf.gen.yaml` (finding G3). `protoc-gen-go`'s
  Opaque API replaces struct literals with generated setters; if a future version defaults to it,
  every `&core.Asset{…}` literal and test fixture breaks at once and ADR-0001's central claim stops
  holding. Pinned, with a Debt Ledger trigger to revisit before any `protoc-gen-go` major bump.
- No field is ever renumbered; deletions become `reserved`.
- Every field and message carries a comment — proto comments are the single source for OpenAPI and
  the API reference (`CLAUDE.md`: "Handwritten API prose is forbidden").

### Proto / schema impact

**Breaking?** No — initial schema, nothing to break. `buf breaking` enforces from the commit after
M0b, which is exactly why the schema took the full adversarial pass now.

---

## PR M0c — The Ring-0 Go surface

### Interfaces

`core` declares `Agent`, `Evals`, `Pool`, `Goal`, `Tuner`, `Capable`, `ContextInjector`,
`KnowledgeInjector`. **Placement in `core` is an interpretation, not verbatim `DESIGN.md`**
(finding G15) — `DESIGN.md`'s layout diagram labels `core/` "pipeline stages … pure" and never
states where the Ring-0 interfaces live. `core` is chosen because `CLAUDE.md` names it the public
Go API and forbids upward imports. **Flagged to the user; confirmed.**

### Domain types — aliases, with the gates that make them safe

**User-confirmed decision.** Generated proto messages *are* the domain types, re-exported as type
**aliases** (not defined types), so `core.Case` and `knov1.Case` are the same type — one definition,
no conversion layer, no drift, and the vocabulary reads correctly at every call site. ADR-0001.

```go
type (
    Case      = knov1.Case
    Response  = knov1.Response
    Score     = knov1.Score
    Asset     = knov1.Asset
    Valuation = knov1.Valuation
    Portfolio = knov1.Portfolio
    Report    = knov1.Report
)
```

The Phase-1 review established that this design is only safe with enforcement attached. All four
gates are conditions of the decision, not follow-ups:

1. **`encoding/json` banned outside `api/`** (depguard) — finding G1. Serializing an alias with
   `encoding/json` emits `1500000` where protojson emits `"1500000"`, and raw enum ints where
   protojson emits names, silently breaking any consumer written against the generated OpenAPI
   spec. A test asserts `cost_usd_micros` round-trips as a quoted string.
2. **`reflect.DeepEqual` banned repo-wide** (forbidigo) — finding G2. `DoNotCompare` makes
   `any`-boxed comparison a runtime panic invisible to `go vet`. Tests use `protocmp.Transform()`.
3. **`api_level = API_OPEN` pinned** — finding G3, above.
4. **Pointers everywhere.** Interfaces take `*Case`, `*Asset`. `go vet`'s `copylocks` enforces it.
   Precision for ADR-0001 (finding G2): the sentinel is `DoNotCopy` (`[0]sync.Mutex`) — a
   zero-size marker that is never locked, not a real mutex, so there is no locking cost.

**Accepted, permanent cost:** generated fields are `Id` and `Url`, not `ID` and `URL`, because
`protoc-gen-go` does not apply Go initialism convention. `DESIGN.md`'s own Go sketch writes
`Asset{ID string}`; real code will read `a.Id`. Because `gen/` is lint-excluded, `ST1003` never
fires, so the divergence is permanent and invisible (finding G9). Recorded in ADR-0001 and the
Debt Ledger with a naming-policy trigger.

### Iterators — `iter.Seq2` with a written, tested contract

**User-confirmed decision.** `DESIGN.md` sketches `Cases(ctx) (iter.Seq[Case], error)`; a single
up-front error cannot express "case 4,001 of 1M failed to parse," and `CLAUDE.md` requires the
streaming profile hold (`iter.Seq` is load-bearing). Two deviations, confirmed separately per
finding A10: `iter.Seq` → `iter.Seq2`, and value → pointer element types.

```go
Cases(ctx context.Context)  (iter.Seq2[*Case, error], error)
Assets(ctx context.Context) (iter.Seq2[*Asset, error], error)
```

Finding G5 — the shape alone is not the contract, and an underspecified one is a *statistical*
bug, not an ergonomic one: if one adapter halts on a bad record and another skips it, the
denominator behind every confidence interval silently varies by adapter, invisibly. The contract
is written into the interface godoc and enforced by a shared harness:

- **Any yielded error is FATAL.** The consumer must stop ranging. Adapters that tolerate malformed
  records handle them internally and report counts via `Provenance` — never by yielding a skippable
  error. One rule, no ambiguity, denominator can't drift.
- **Producers defer cleanup inside the iterator closure.** Early `break` is legal, so cleanup that
  lives outside the closure leaks the file or connection.
- **Producers check `ctx.Err()` before each yield.** `iter.Seq2` carries no cancellation channel.
- `coretest.ConformIterator` — every adapter must pass it: break-after-N leaks no fd or goroutine,
  context cancel halts within one yield, a fatal error stops iteration.

Noted in the interface godoc, per finding G12: **iterator output is borrowed for the duration of
one loop iteration.** Consumers clone before retaining or mutating — otherwise concurrent valuation
workers mutating a shared `*Asset` is an ordinary data race that `-race` only catches if a test
happens to exercise it.

### `core/errs`

`errs.Actionable{Code, Message, Fix, DocsURL}` implementing `error`, wrapping with `%w`, converting
to and from `knov1.Actionable` — one struct, serialized identically by CLI and API. Sentinels:
`ErrBudgetExceeded`, `ErrHoldoutSealed`, `ErrCapabilityUnsupported`, `ErrCheckpointStale`. Exit-code
constants (`0` ok, `1` error, `2` budget-stopped, `3` validation-failed) live here so CLI and API
cannot disagree.

### `stats/budget` — the spend guard, before any spend path exists

`CLAUDE.md` prime directive 4 makes unguarded spend paths P0 bugs. Landing the guard before the
first adapter means every future spend path is written against an interface that already exists.

Ships: `budget.Guard` with `Estimate`, `Authorize`, `Record`, `Remaining`; caps for
`max_llm_calls`, `max_cost_usd`, `max_tokens`; a `Confirm` hook (huh in the CLI, `estimate_only` in
the API, auto-approved by `--yes`); mutex-guarded accounting; `ErrBudgetExceeded` → exit code 2. No
network, no LLM client — pure accounting, fully unit-testable, `-race` clean.

Deferred to the milestone that introduces spend: checkpoint persistence (needs `store/`), and the
double-spend-on-resume integration test.

---

## Alternatives considered

**A. Vertical slice first — a working `kno baseline` against a fake agent, gates later.**
Rejected. `CLAUDE.md`'s DoD makes gates a merge condition; every PR before they exist ships
non-compliant, and the coverage ratchet starts from whatever the slice happened to hit rather than
a deliberate floor. It also front-loads the least-reversible decision (proto shape) into a PR whose
review attention goes to pipeline logic.

**B. Hand-written Go types now; proto at v0.3 when the API lands.**
Rejected. Contradicts `DESIGN.md` ("defined once, in protobuf — schema source of truth from day
one, so the later API, SDKs, plugins, and platform are codegen, not rewrites") and `CLAUDE.md`'s
proto-first rule. The tradeoff — "ceremony now, no schema-drift rewrite later" — is already decided
in the design.

**C. Multi-module repo (one module per ring).**
Rejected. The deliverable is a single static binary. Multiple modules add intra-repo version skew
and complicate `go install`, while the `core` import rule is already enforced by a test. Revisit
only if the Python gradient sidecar or an SDK needs independent versioning.

**D. Include the Ring-2 handshake proto in M0 "since we're doing proto anyway."**
Rejected. `DESIGN.md` schedules Ring 2 for v0.3 explicitly so the protocol freezes only after
Ring-0 survives real users, warning that freezing early is "how projects end up maintaining two
protocols forever." Shipping the handshake now creates exactly the obligation that timing avoids.

**E. Generate proto code at build time instead of checking it in.**
Rejected. Checked-in generated code keeps `go get github.com/knograph/kno/core` working for
consumers with no `buf`, and keeps contributor setup at `go build`. Cost: generated files in diffs.
Mitigated by `.gitattributes` (`linguist-generated`) and a CI check that `make generate` produces
no diff, so checked-in output cannot drift from source.

**F. Generated idiomatic Go value types + `To/From` proto converters at I/O boundaries.**
*Added during Phase-1 review (finding G13); presented to the user alongside the alias design.*
Proto stays the single schema source, but domain code gets comparable, `encoding/json`-safe,
Opaque-API-insulated, correctly-initialism'd types — dissolving findings G1, G2, G3, and G9
outright. **Rejected by the user in favor of the alias design plus enforcement gates.** Reason: a
converter layer is a second representation that can drift, and DESIGN.md's "defined once" reads
most literally as aliases. The four findings it would have dissolved are instead handled by the
four gates above, and a Debt Ledger entry carries the trigger to revisit if those gates prove
insufficient in practice.

---

## Edge cases and mitigations

| # | Edge case | Mitigation |
|---|---|---|
| 1 | `buf breaking` has no baseline | Empty baseline yields no breaking changes; no special case. One-time env escape only if buf errors, removal is an M0c checklist item |
| 2 | Coverage ratchet has no baseline | M0c writes it; floors bind immediately, only no-decrease bootstraps |
| 3 | Package with code but no tests exempted forever | Exemption requires *no non-test `.go` file with a function body*, not "absent from profile" (finding G6) |
| 4 | Lint flooding on generated code | `gen/`, `*.pb.go` excluded by explicit path, not blanket wildcard |
| 5 | `protoc-gen-go` absent locally (confirmed) | Pinned via `go.mod` `tool` directive; no remote plugins, no network at codegen |
| 6 | `gitleaks`/`vhs`/`gum`/`baml-cli` absent locally (confirmed) | Loud named SKIP locally; CI installs them so the gate is real where it counts |
| 7 | Enum zero silently meaning a real destination/kind | Mandatory `_UNSPECIFIED = 0`, enforced by `buf lint` |
| 8 | Float dollar accumulation | `int64` micro-USD end to end; test sums 10,000 sub-cent charges exactly |
| 9 | Absent CI vs zero-width CI | `Interval` is a message field; presence semantics tested across a marshal round-trip |
| 10 | Proto message copied by value | Pointers in interfaces; `go vet` `copylocks`; PR-template checklist line |
| 11 | `any`-boxed proto comparison panics at runtime | `reflect.DeepEqual` banned by forbidigo; `protocmp` in tests (finding G2) |
| 12 | `encoding/json` diverging from protojson | depguard bans it outside `api/`; quoted-int64 round-trip test (finding G1) |
| 13 | Opaque API flip breaking every struct literal | `api_level = API_OPEN` pinned in `buf.gen.yaml` (finding G3) |
| 14 | Adapters disagreeing on skip-vs-halt, silently varying CI denominators | One rule: every yielded error is fatal; `coretest.ConformIterator` enforces (finding G5) |
| 15 | Early `break` leaking fds/connections | Cleanup deferred inside the closure; conformance test asserts no leak |
| 16 | Shared `*Asset` mutated by concurrent workers | Borrow-for-one-iteration contract in godoc; Debt entry with M1 trigger (finding G12) |
| 17 | `core` importing upward | Import-boundary test walks transitive imports **including test-only imports and all build tags** (finding G10) |
| 18 | Budget guard race | Mutex-guarded; `-race` test hammers from 128 goroutines, asserts cap never exceeded |
| 19 | Tool drift between contributor and CI | `go.mod` `tool` directive; CI runs `make tools` from the same pins |
| 20 | `make check` too slow to run locally | Fail-fast cheapest-first; fuzz and integration are separate targets |
| 21 | golangci-lint v2 config silently misconfigured | Config shape verified with `golangci-lint config verify` before commit (finding G4) |

## Test plan

**Unit**
- `core/errs`: what→why→fix grammar; `errors.Is`/`As` through `%w`; exit-code table test; round-trip
  to/from `knov1.Actionable`.
- `stats/budget`: per-dimension caps; exact micro-USD accumulation (#8); `Authorize` denies *at* the
  boundary, not one call past it; `-race` concurrency test (#18); `ErrBudgetExceeded` → exit 2.
- `gen`: round-trip for every message; `Interval` presence semantics (#9); **table-driven over file
  descriptors** asserting every enum has an `_UNSPECIFIED` zero, so new enums are covered without
  anyone remembering; `cost_usd_micros` marshals as a quoted string under protojson (#12).

**Structural / invariant**
- `core` import-boundary test (#17).
- `coretest.ConformIterator` — break-after-N leak check, context-cancel halt, fatal-error stop (#14, #15).
- `make generate` produces no diff (CI).
- `godoccheck` passes on all exported symbols.

**Fuzz (`make fuzz-short`)**
- Proto unmarshal of `Report`, labeled **PLACEHOLDER** in the target's own output. The parsers
  `CLAUDE.md` names — plugin handshake, NDJSON frames, agent-ref, `kno.yaml` — do not exist yet;
  the gate says so rather than implying coverage (finding G11).

**Deferred with reason (unstarted scope, not debt)**
- Holdout-isolation canary, winner's-curse regression, CI-coverage property tests: require the
  pipeline and `stats` internals M0 does not ship. Listed in the M1 plan.

## Rollback story

M0 is additive to a repository with one prior commit, no users, no published artifacts, no schema
consumers, and no persisted data. Rollback is `git revert` of each squash commit.

The one thing outliving a revert is the **proto compatibility baseline**: once M0b merges, `buf
breaking` treats `kno.v1` as the baseline. No published tag, no SDK, no plugin exists at M0, so the
blast radius is the repo itself — which is why the schema took the adversarial pass now.

## Docs impact

- **godoc:** every exported symbol in `core`, `core/errs`, `stats/budget` (gated by `godoccheck`).
- **CLI help:** `kno version` only; the snapshot-test harness is established before the surface grows.
- **OpenAPI:** none — M0 defines no services. `make docs` states this rather than emitting an empty spec.
- **Mental model / cookbook / *What the numbers mean*:** untouched. M0 changes no user-visible
  behavior and reports no numbers; the PRs carry no behavior-change label.
- **New:** `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `docs/adr/0001-proto-as-domain-types.md`,
  `docs/adr/0002-generated-code-layout.md`, `docs/adr/0003-platform-schema-boundary.md`,
  `docs/debt.md` header + entries, `CHANGELOG.md`.

---

## Phase-1 review record

Two independent adversarial passes, both **BLOCK**. Dispositions:

**Fixed in this amendment (blocking findings):**

| Finding | Fix |
|---|---|
| A1 — `Tuner`/`Capable` reference four types defined nowhere; day-one compile error | `tuner.proto` added (`TuningJob`, `JobRef`, `JobStatus`); `Capabilities` added to `common.proto` |
| A2 — `RejectionReason` as bare enum cannot express `redundant_with:<id>` | `Rejection` carries `redundant_with_asset_ids` + `detail` |
| A4 — M0 violates the <3-day branch rule; CI/community work has no proto coupling | Split into M0a / M0b / M0c |
| A5 — `event.proto` frozen with zero producers, justified by citing v0.3 features | Cut from M0; lands with Baseline in M1 |
| A7 — Ambiguous whether coverage *floors* bind on the bootstrap PR | Explicit: floors bind on M0c; only no-decrease bootstraps |
| A8 — `buf breaking` skip was a re-evaluable heuristic, not a one-time bootstrap | Heuristic removed; empty baseline needs no special case |
| A10 — `optional` on a message field claimed to provide presence; it is a proto3 no-op | Plan text corrected; property holds, mechanism was misstated |
| G1 — `encoding/json` silently diverges from protojson (int64, enums) | depguard ban outside `api/` + quoted-int64 test |
| G2 — `DoNotCompare` panics at runtime through `any`; `go vet` can't see it | forbidigo ban on `reflect.DeepEqual`; `protocmp` in tests |
| G4 — golangci-lint v2 rejects the planned config shape | **Verified by probe**; corrected v2 config validated before commit |
| G5 — Iterator error contract underspecified → CI denominators vary by adapter | Fatal-error rule + `coretest.ConformIterator` |
| G6 — Coverage exemption would hide a code-bearing package with zero tests | Exemption requires no non-test function bodies |
| G7 — buf remote plugins add a network + supply-chain surface for no benefit | Dropped; `protoc-gen-go` pinned via `go.mod` `tool` |
| G8 — `tools.go` obsolete on Go 1.25 | **Verified**; native `tool` directive used |
| G9 — Generated `Id`/`Url` contradict Go initialism convention, invisibly | Accepted with an explicit written policy; ADR-0001 + Debt entry |
| G15 — Ring-0 interfaces placed in `core` is interpretation, not verbatim DESIGN.md | Flagged to the user; confirmed |

**Accepted risks → `docs/debt.md`** (every entry carries a repayment trigger, per `CLAUDE.md`):

| # | Accepted risk | Repayment trigger |
|---|---|---|
| 1 | Prime directive 5 is *representable but not enforced* by the M0 schema — a `Valuation` can carry a delta with no `Interval`. "No holdout access before Validate" is a runtime property no schema can express. (A3) | Construction-time invariant when the Value stage lands (M1); holdout canary test with the Baseline stage |
| 2 | No typed `Run` resource; run IDs are bare ULID strings (A6) | Before `serve` / the event stream lands (v0.3) |
| 3 | `bench-diff` is a no-op placeholder with no benchmarks to gate (A9) | Before the first PR touching the scoring loop, routing, or NDJSON framing |
| 4 | `fuzz-short` fuzzes Google's parser, not ours (G11) | When the plugin handshake, NDJSON frame, agent-ref, or kno.yaml parsers land |
| 5 | Opaque-API migration would break every struct literal at once (G3) | Before any `protoc-gen-go` major version bump |
| 6 | Generated `Id`/`Url` spellings diverge from Go convention and `DESIGN.md`'s sketch, invisibly (G9) | Set a `kno.v1` field-naming policy before 1.0 |
| 7 | Import-boundary test cannot catch *schema* contamination — only the Go import graph (G10) | When Ring-2 plugin work begins (v0.3) |
| 8 | Borrow-for-one-iteration is a godoc convention, not a compiler-enforced rule (G12) | Before the first stage that fans out workers over one iterator (M1) |
| 9 | No mechanism decided for platform-only fields; "just add a field to `Case`" is the path of least resistance (G14) | Before platform proto work begins — ADR-0003 records the intended split (separate platform package referencing OSS IDs; `kno.v1` never carries platform-only fields) |
| 10 | Alias design chosen over generated-idiomatic-types (Alternative F); four findings are handled by gates rather than dissolved (G13) | Revisit if the depguard/forbidigo gates are bypassed or prove insufficient in review |

---

## Phase-3 review record (M0a)

One adversarial pass over commit `1d47faf`. Verdict **BLOCK**, with every finding verified
empirically against a scratch copy rather than argued from reading. Because this PR *is* the
enforcement machinery, the reviewer's framing was right: the highest-damage failure is a gate that
appears to enforce something and does not. Five of those shipped.

**Fixed:**

| Finding | What was actually wrong | Fix |
|---|---|---|
| F1 | `forbidigo` was excluded from `_test.go`, so the `reflect.DeepEqual` ban was off in the only place that construct appears. ADR-0001 names it a precondition of the alias design; the design shipped unprotected. | Exclusion scoped by `text:` to the print rules only. Verified: `reflect.DeepEqual` now caught in tests, `fmt.Println` still allowed there. |
| F2 | `typecheck-proto` and `generate` shipped the exact `[ -d proto ]` heuristic Phase-1 finding A8 rejected — a `proto/` rename would silently disable breaking-change protection, and CI greps for `SKIP`, not `PEND`. | Replaced with a tracked `proto/PENDING` token that M0b deletes. Hard-fails if any `.proto` exists while the token remains. Verified both directions. |
| F3 | The nightly live-API job carried real credentials and a `KNO_MAX_COST_USD` comment claiming budget-guard protection. No code reads that variable; `stats/budget` is a stub. A trap set ahead of the guard. | New `make test-live`, unreachable from `check`, refusing to run unless the cap is set *and* some Go file reads it. Ledger entry 11. |
| F4 | `gitleaks detect` scans history only — a live key sitting uncommitted in the working tree passed. The gate only turned red after remediation required a history rewrite. | Runs `gitleaks dir` (tree) *and* `gitleaks git` (history). |
| F5 | `git diff --quiet -- gen/` cannot see *new* untracked generated files, which is the common case when a message is added. ADR-0002's "can never drift" was false for additions. | `git status --porcelain -- gen/`. ADR-0002 corrected. |
| F6 | DCO checked merge commits (blocking legitimate PRs), had no Dependabot exemption (every dependency PR would be red), and failed **open** when `git rev-list` errored. | `--no-merges`, bot exemption, materialize-then-check so an unreadable range fails closed, and `grep -xF` matching author *or* committer. A regex-escaping bug in the first fix was caught by dry-running it against real history. |
| F7 | `make check` omitted `test-integration`, `fuzz-short`, `bench-diff`, and `generate-check`, so green locally did not predict CI — and the fixture integration path never gated a PR at all, while the only path that ran it was the money-spending one. | All four folded into `check`. CI now runs exactly `make check`. |
| F8 | Six documented mechanisms did not exist: godox/nolintlint enforcement, CI verification of the PR template, release-please invocation, commit-message linting, SLSA/cosign/SBOM, and the ledger-lapse check. | `godox` and `nolintlint` enabled; release-please and commit-lint workflows added; PR template now says a human checks it; SECURITY.md states plainly that no release pipeline exists. Ledger entries 13 and 15. |
| F9 | `tools` was `.PHONY`, so every gate reinstalled all five tools and the CI cache was inert. | Stamp file keyed on `tools/go.{mod,sum}`; cache key now includes the Go version. |
| F10 | The `fmt.Print` rule had no `tui/` carve-out despite the comment claiming one, and overriding `forbid` silently dropped forbidigo's builtin `print`/`println` defaults. | `tui/` exclusion added, builtins re-added, `os.Std{out,err}` covered. All verified firing. |
| F11 | depguard exempted the entire `api/` tree — precisely where the protojson divergence is fatal — via an unanchored `**/api/**` glob that would widen as the tree grew. | Narrowed to explicitly-named envelope files; test exemption dropped entirely. |
| F12 | gitleaks was installed unpinned and unverified, differing between matrix legs, so a leak could be caught on one OS and missed on the other. | Pinned to one version on both legs, checksum-verified before extraction. Action SHA-pinning recorded as ledger entry 14. |
| F13 | `GOTESTFLAGS ?=` let `GOTESTFLAGS= make test` silently drop `-race`, directly under a comment saying it must not be removable. | `:=`, with a separate `GOTESTEXTRA ?=` for real tuning. |
| F14 | `KNO_CI` was dead configuration; the actual mechanism was a `grep 'SKIP'` over merged stdout, which would also fire on any tool banner containing that word. | `KNO_CI` wired into `skip_missing` so a missing tool exits 1. Grep removed. Verified both states. |
| LOW | `.NOTPARALLEL` absent, `revive` custom rules silently replacing its defaults (so `package-comments` was unenforced), `make help` fragile under `pipefail`, no job timeouts, `docs/debt.md#N` anchors not resolving. | All fixed; ledger rows now carry explicit HTML anchors. |

**Not fixed — recorded as debt with triggers:** entries 11–15 (`docs/debt.md`).

**Verification:** `make check` green with zero `SKIP`; `KNO_CI=1 make check` green, confirming the
CI path. The proto sentinel, the `KNO_CI` interlock, the DCO matcher, and every lint rule were each
exercised in both the passing and the failing direction — a gate only counts once it has been seen
to fail.

# CaseExecution: a writer, a reader, and the recorded half of a resume gate

Repays [`docs/debt.md#26`](../debt.md#26). Second of three PRs replacing the blocked
[M2-10e plan](2026-08-25-m2-10e-observations-and-orphan-spend.md). Lands after
[the orphan-spend PR](2026-08-25-orphan-spend.md), which owns the only migration.

**It needs no schema change.** Every column it aggregates — `refused`, `truncated`,
`usage_estimated`, `provider_build_id`, `resolved_model`, `score_value` — landed in migration 1.
The superseded plan claimed otherwise and used it to argue against splitting.

## Problem

`CaseExecution` landed in M2-0 with message presence so a stage that executes no Cases can be
told from one that attempted nothing. Nothing writes it. `closeRun` still writes the five flat
counters and `cli` still reads them, so [#26](../debt.md#26) is half repaid and the proto comment
says so in caps.

## Presence is a stage property, not a query result

This is the decision the superseded plan got backwards, and it matters most in the case this
milestone just created.

ADR-0004: presence means *"this Run executed Cases"* — absent for Value and Select, which work
over Assets. That is decided by **which stage is running**, before any Case runs.

Derive it from the query instead and a run where the guard refused every Case *after billing*
returns no rows, writes `case_execution` absent, and tells a consumer "this stage executes no
Cases" — about a run that executed Cases and spent money. That is the inverted ambiguity
`run.proto` explicitly forbids.

**So `closeRun` writes `CaseExecution` unconditionally for Baseline**, with zeros where the run
genuinely did nothing. A stage that invokes no agent leaves it absent by not writing it.

## The resume gate this fills half of

> **Retracted 2026-08-25.** This section originally claimed this PR arms the gate. It does not,
> and the claim survived into the PR body before being caught. `checkResumable` compares
> `CaseExecution.resolved_models` against `BaselineOptions.ResolvedModel` — and **nothing sets
> that option outside tests**, verified by grepping every assignment in the tree. Writing
> `resolved_models` fills the *recorded* operand; the *current* operand stays empty, and
> `resolvedModelChanged` short-circuits on an empty side exactly as before. No user-facing
> `ErrCheckpointStale` becomes reachable in this PR.
>
> The option cannot be filled either: it is read at `openRun`, before any request, so the only
> value it could hold is one a previous run recorded — the check comparing a value to itself. The
> gate has to move to **first-response time**, which is a `core` change with an event and an exit
> code. Re-triggered to M2-11 in [debt #42](../debt.md#42); the end-to-end test lands there.

`checkResumable` refuses a resume when `resolvedModelChanged`, which reads
`firstResolvedModel(run)` → `CaseExecution.resolved_models[0]`. Nothing writes that field.

Two defects to fix in the recorded half regardless, because a gate that goes live over bad data is
worse than one that is inert:

- **`resolved_model` is `TEXT NOT NULL DEFAULT ''`.** Every errored Case has `''`. A naive
  `SELECT DISTINCT` returns `''` as a set member, so `resolved_models` would carry an empty
  string and `models[0]` could *be* it. Filter `WHERE resolved_model != ''`.
- **`DISTINCT` has no guaranteed order.** `firstResolvedModel` reads `models[0]`, so a run that
  saw two models would refuse or accept a resume nondeterministically across two invocations of
  the same command. A flaky money gate is worse than no gate. `ORDER BY resolved_model`.

`models[0]` remains a lie about a set even when ordered, so `checkResumable` compares **set
membership**: a resume is refused when the model now resolving is absent from the recorded set.
[#42](../debt.md#42) is *not* repaid here — its detection half needs the check moved to
first-response time, which is its own PR — and this PR must not mark it so.

## Scope

- `Store.CaseObservations`.
- `closeRun` composes and writes `CaseExecution`.
- `cli/jsonreport.go` **and** `cli/render.go` read it.
- The five flat counters keep being written.
- `resolved_models` filtered and ordered; `checkResumable` compares set membership.
- The `run.proto` "NOT POPULATED YET" comment and the `DEBT()` marker in `baseline_close.go` both
  deleted in this diff.

## `CaseObservations`' contract

```go
// CaseObservations aggregates per-outcome facts for one Run.
//
// Aggregable facts ONLY. dev_case_count and holdout_case_count are NOT here:
// they describe what was loaded, not what executed, and ADR-0004 records that
// aggregating them from outcomes reports a zero holdout count — the number
// that sets every interval's width. closeRun composes CaseExecution from this
// plus BaselineOptions.
//
// Zeros for a Run with no outcomes, with no presence signal. Presence is a
// property of the stage and belongs to the caller.
//
// Purge-transparent: every field is a column, never a blob.
CaseObservations(ctx context.Context, runID string) (Observations, error)
```

It returns `Observations`, not `*knov1.CaseExecution` — a store that returned the proto message
would invite the caller to forget the two ingested counts, which is the exact trap ADR-0004 warns
about.

**It is two queries, not one**, and the plan says so rather than claiming atomicity it does not
have: scalar aggregates in one, the two `DISTINCT` string sets in another, because
`group_concat(DISTINCT x)` cannot take a separator safe against arbitrary model names. `closeRun`
runs after the executor drains, so nothing races it within a process; nothing prevents two
`kno baseline --resume` processes on one run ID, which is true of every store read today.

## Two numbers for one fact

After this, `RunFinished.Attempted/Scored/Errored` come from `agg.counts()` and
`CaseExecution.*_case_count` from SQL. They agree today — `emit` counts after the sink persists,
which is why that ordering exists — but nothing enforces it.

**Decision: SQL is authoritative for the record, the aggregator for the stream.** The stream
reports what this process observed as it happened; the record reports what is durable. A test
asserts they agree at close for an ordinary run, so a future divergence is caught rather than
discovered.

The five flat counters keep being written. Removing them is a wire change with no reader ready,
and [#26](../debt.md#26)'s own text keeps them "until their writer and reader migrate" — the
reader migrating is this PR; retiring the fields is a later one with a deprecation note.

## `--json` is deliberately unchanged

The superseded plan made the counts `*int32` rendering `null`. Cut, for two reasons the review
established:

- **The null is unreachable.** `validate` refuses `DevCases <= 0` and `HoldoutCases <= 0`, and
  Baseline is the only front end. There is no run `kno baseline` can produce where the counts are
  absent, so the break costs an external user something and buys nothing until a non-Case-executing
  stage exists.
- **`jsonreport.go` is deliberately decoupled from the proto** — its own comment says it is *"a
  CLI contract aimed at a person's jq pipeline"* that *"should not shift underneath them when the
  schema gains a field"*. Mirroring the proto's presence rules is the coupling that comment exists
  to prevent.

Deferred to a third PR, when a stage that executes no Cases actually exists. A sibling
`counts_present` bool is the non-breaking option if it is ever needed sooner.

## Edge cases

| Case | Behaviour |
|---|---|
| Baseline run, any shape | `CaseExecution` present, zeros if nothing ran |
| A stage that invokes no agent | Absent, because that stage does not write it |
| Every Case refused after billing | Present with zeros — it executed Cases, and the orphan spend says so |
| All Cases errored | `resolved_models` empty after the `!= ''` filter, so the resume gate stays inert rather than comparing against an empty string (it is inert for a second reason too — see the retraction above) |
| Two models observed | Both in the set, ordered; `checkResumable` compares membership |
| Purged run | Unaffected — every aggregated field is a column |

## Test plan

- `closeRun` writes `CaseExecution` matching the flat counters and `RunFinished`.
- A run with zero scored Cases still gets the message, with zeros.
- `dev`/`holdout` counts come from options: a fixture whose outcomes disagree with the split
  proves they are not aggregated.
- `resolved_models` excludes `''` and is ordered; a two-model run is deterministic across
  repeated reads.
- `checkResumable` refuses a resume whose model is absent from the set, and accepts one whose
  model is present but not first — the case `models[0]` gets wrong.
- Human and `--json` output both render from `CaseExecution`; golden files and the vhs tape
  re-recorded.
- Every one verified failing first.

## Rollback

Revert. No schema change, no wire change, no migration.

## Docs impact

- `run.proto`'s "NOT POPULATED YET" paragraph — it is the published API reference and becomes
  false the moment this merges.
- `docs/mental-model.md:112` — *"Resuming is refused if the evals, the goal, or the agent
  changed"* becomes true of the model too, for the first time.
- The event-stream section M2-10 has owed since 10a.
- CLI help snapshots, golden files, vhs tape.
- CHANGELOG.

## Accepted risks

- **Arming `checkResumable`'s model check is a behaviour change users will see.** A resume that
  worked yesterday can be refused today, correctly — the alternative is a run whose halves were
  measured against different models. Called out in the CHANGELOG rather than left to be
  discovered.
- **`RunFinished` and `CaseExecution` derive one fact two ways.** Reconciled by a test rather than
  by construction; making the stream read SQL would put a query on the hot path.

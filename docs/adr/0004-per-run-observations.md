# ADR-0004: Per-run observations live in a submessage, derived from persisted rows

- **Status:** accepted
- **Date:** 2026-08-21
- **Context:** [M2 plan](../plans/2026-08-21-m2-first-provider-adapter.md) §10a, opened by
  Phase-1 pass three, finding M-7.

## Problem

M2 produces four facts about a Run that nothing today can carry:

- the distinct backend identities a provider reported during the run,
- how many Cases the provider **refused** on policy grounds,
- how many were **truncated** at the output ceiling,
- how many settled against an **estimated** rather than reported usage block.

None has anywhere to live. `Run` is written twice — at `openRun`, before any `Response` exists, and
again at `closeRun`. The only cross-worker state in the stage is `core/baseline_events.go`'s
`aggregator`, carrying `sum`, `scored`, `errored`, `seq`, and two resume-seeded priors.

`docs/debt.md#26` describes the same shape from the other side: `Run`'s five Case counters do not
track presence, so a stage that executes no Cases writes a hard zero indistinguishable from a real
one. Its prescribed fix is an additive submessage with message presence.

## Decision

**One submessage, `CaseExecution`, on `Run`. Its numbers are aggregated from persisted outcome rows
at `closeRun`, not accumulated in memory.**

It carries the five Case counts (repaying debt 26) *and* the four observations above, because both
are facts about a run that executed Cases, both need presence for the same reason, and a stage that
executed none has neither.

Three consequences worth stating:

**It is not named `BaselineDetail`,** which is what debt 26 prescribed. Value also executes Cases,
so a stage-named message would be either wrong for Value or duplicated for it. `CaseExecution`
describes the condition under which the fields are meaningful, which is what the presence bit
actually encodes. Deviating from the ledger's prescribed name is recorded here rather than done
quietly.

**The numbers come from SQL, not from counters.** §2.9a's migration already gives `outcomes` a
column per fact (`score_value`, `refused`, `truncated`, `usage_estimated`), so `closeRun` aggregates
what is already durable. This is the same move that fixed `docs/debt.md#27`: deriving a run-level
number from persisted rows rather than from in-memory state is what makes it survive a crash and be
correct across a resume. It also leaves `aggregator` — the one piece of shared mutable state in the
stage — untouched, which matters given `core/baseline.go` is already 657 lines against a ~400 soft cap.

**The field is `observed_backends`, not `system_fingerprint`.** `Run.input_fingerprint`
(`run.proto:105-108`) already means "the hash whose mismatch refuses a resume." A second
"fingerprint" on the same message, meaning "which provider build answered," is the vocabulary drift
prime directive 2 forbids. It is `repeated` because with concurrency N there is no first response,
and during a provider rollout two workers in one run legitimately see different backends — a set
makes the mixture visible where a first-writer-wins scalar hides it.

## Alternatives rejected

**A — extend `aggregator` with four channels.** Grows the stage's only shared mutable state, and the
numbers would be lost on a crash and wrong across a resume in exactly the way debt 27 documents.

**C — emit them only as events, never on `Run`.** Cheapest, and a consumer that did not replay full
history could not see them; `--json` could not report them at all. The counts qualify the score, and
a qualifier that travels separately from the number it qualifies is a qualifier nobody reads.

## Consequences

- `Run`'s five flat counters stay until the writer and reader migrate (M2-10). Until then both
  representations exist, and `docs/debt.md#26` stays labelled *partly* repaid — a `DEBT()` comment
  pointing at a row marked repaid is worse than a live marker.
- `closeRun` gains one query. It runs once per Run, against a primary-key-indexed table.
- Value inherits the message when it lands, rather than defining its own.

# ADR-0003: The platform never adds fields to `kno.v1`

- **Status:** Accepted (intent recorded; not yet enforced)
- **Date:** 2026-08-18
- **Context:** [Plan: repo-foundation](../plans/2026-08-18-repo-foundation.md), Phase-1 finding G14

## Context

Kno is the OSS engine beneath the closed uknoAI platform. `DESIGN.md` draws the open-core seam as
"a directory boundary, not a fork," and `CLAUDE.md` makes `core` importing upward a rejectable
offense.

The Go import graph is enforced by a test. **The schema is not.** Because ADR-0001 makes generated
`kno.v1` messages the shared domain types for both OSS and platform code, "just add a field to
`Case`" becomes the path of least resistance the first time platform work needs state that OSS does
not have. Nothing in the Go import check can see that happen.

## Decision

**`kno.v1` never carries platform-only fields.** Platform state lives in a separate platform proto
package whose messages reference OSS entities by ID.

Rejected alternatives:

- *Extend `kno.v1` directly* — contaminates the open-core boundary and forces OSS consumers to
  carry fields that are meaningless without the platform.
- *Proto extensions on `kno.v1` messages* — proto2-flavored, poorly supported across the generated
  SDKs Kno intends to ship, and it still puts platform semantics in the OSS schema's namespace.

## Consequences

- OSS `kno.v1` messages stay meaningful standalone, which is the promise `DESIGN.md` makes when it
  says "the OSS engine is complete and genuinely useful standalone."
- Platform code pays a join (by ID) rather than reading a field.
- **This ADR is currently intent, not enforcement.** No check detects a violation. `docs/debt.md#9`
  carries the trigger: decide the enforcement mechanism before platform proto work begins.

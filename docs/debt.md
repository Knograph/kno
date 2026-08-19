# Debt Ledger

The only place accepted technical debt lives. It is public on purpose: an honest debt register is
a trust feature, and several entries here make excellent `good-first-issue`s.

**Rules** (from `CLAUDE.md`):

- Adding an entry **requires a repayment trigger**. "Someday" is not a trigger — an entry without
  one is a rejected review finding, not accepted debt.
- Reviewed at every minor release. Each entry is repaid, re-dated with a written reason, or
  promoted to "won't fix" with the rationale moved into an ADR. Silent carryover is not an option;
  CI fails a release tag if a trigger has lapsed without a disposition.
- `TODO`/`FIXME`/`HACK` comments in code must reference an entry here (`// DEBT(docs/debt.md#42): …`)
  or an issue. The lint bundle rejects naked ones.

Reference an entry by its number: `docs/debt.md#3`.

| # | What was deferred | Why | Repayment trigger | Owner |
|---|---|---|---|---|
| 1 | Prime directive 5 ("no reported delta without its CI") is **representable but not enforced**. The schema permits a `Valuation` carrying a delta with no `Interval`. Separately, "no holdout access before Validate" is a runtime property no schema can express. | [Plan: repo-foundation](plans/2026-08-18-repo-foundation.md), Phase-1 finding A3. M0 ships no Value stage, so nothing can violate it yet — but the guarantee must not be assumed to come from the schema. | Construction-time invariant when the Value stage lands (M1); holdout-canary test lands with the Baseline stage (M1). | @devarispbrown |
| 2 | No typed `Run` resource. Run IDs are bare ULID strings; `Event`, `Report`, and the future API all reference them untyped. | [Plan](plans/2026-08-18-repo-foundation.md), finding A6. Adding a typed `Run` message later is purely additive, so deciding now bought nothing. | Before `serve` / the event stream lands (v0.3). | @devarispbrown |
| 3 | `make bench-diff` is an inert placeholder — no benchmarks exist and no hot path exists to gate. | [Plan](plans/2026-08-18-repo-foundation.md), finding A9. Nothing to measure at M0; the target exists so the gate name is real. | Before the first PR touching the scoring loop, routing, or NDJSON framing merges. | @devarispbrown |
| 4 | `make fuzz-short` fuzzes the protobuf runtime's wire parser, not kno's own parsers. | [Plan](plans/2026-08-18-repo-foundation.md), finding G11. The parsers `CLAUDE.md` names (plugin handshake, NDJSON frames, agent-ref, `kno.yaml`) do not exist yet. Retained as a cheap panic-safety smoke test, labeled PLACEHOLDER in its own output so green is never misread as coverage. | When the plugin handshake, NDJSON frame, agent-ref, or `kno.yaml` parsers land (M1+). Fuzz targets must be added in the same PR. | @devarispbrown |
| 5 | `protoc-gen-go`'s Opaque API is not adopted; `api_level = API_OPEN` is pinned. A flip would break every struct literal and test fixture at once. | [ADR-0001](adr/0001-proto-as-domain-types.md), finding G3. The Opaque API would insulate call sites from generated-struct changes, but migrating now costs more than pinning. | Before any `protoc-gen-go` **major** version bump. | @devarispbrown |
| 6 | Generated field spellings diverge from Go convention and from `DESIGN.md`'s own sketch: `Asset.Id`, not `Asset.ID`; `Url`, not `URL`. Because `gen/` is lint-excluded, `ST1003` never fires, so the divergence is permanent **and invisible**. | [ADR-0001](adr/0001-proto-as-domain-types.md), finding G9. A direct consequence of proto messages being the domain types; the alternative was a converter layer (rejected, see the plan's Alternative F). | Set an explicit `kno.v1` field-naming policy before 1.0. | @devarispbrown |
| 7 | The `core` import-boundary test enforces only the **Go import graph**. It cannot detect schema contamination — the platform adding fields to shared `kno.v1` messages. | [Plan](plans/2026-08-18-repo-foundation.md), finding G10. No Go-level check can see this; it is a proto-review problem. [ADR-0003](adr/0003-platform-schema-boundary.md) records the intended split. | When Ring-2 plugin work begins (v0.3), which adds new import surfaces to reason about. | @devarispbrown |
| 8 | "Iterator output is borrowed for one loop iteration; clone before retaining or mutating" is a **godoc convention**, not a compiler-enforced rule. | [Plan](plans/2026-08-18-repo-foundation.md), finding G12. Concurrent workers mutating a shared `*Asset` is an ordinary data race that `-race` catches only if a test happens to exercise it. | Before the first pipeline stage that fans out workers over one iterator (M1). That PR must add the race test. | @devarispbrown |
| 9 | No mechanism decided for platform-only proto fields. With shared `kno.v1` types, "just add a field to `Case`" is the path of least resistance for platform work. | [ADR-0003](adr/0003-platform-schema-boundary.md), finding G14. Recorded intent: a separate platform proto package referencing OSS IDs; `kno.v1` never carries platform-only fields. Not yet enforced by anything. | Before platform proto work begins. | @devarispbrown |
| 10 | The proto-alias design was chosen over generated idiomatic types + converters. Four review findings (G1, G2, G3, G9) are **handled by lint gates rather than dissolved by design**. | [Plan](plans/2026-08-18-repo-foundation.md), Alternative F, finding G13. Converters are a second representation that can drift; `DESIGN.md`'s "defined once, in protobuf" reads most literally as aliases. | Revisit if the depguard/forbidigo gates are bypassed, disabled, or prove insufficient in review — or at 1.0, whichever comes first. | @devarispbrown |

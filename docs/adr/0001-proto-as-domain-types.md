# ADR-0001: Generated proto messages are the domain types

- **Status:** Accepted
- **Date:** 2026-08-18
- **Context:** [Plan: repo-foundation](../plans/2026-08-18-repo-foundation.md) (M0c)
- **Supersedes:** nothing

## Context

`DESIGN.md` states that all supporting types (`Case`, `Score`, `Valuation`, `Portfolio`, `Report`)
are "defined **once**, in protobuf — schema source of truth from day one, so the later API, SDKs,
plugins, and platform are codegen, not rewrites."

That leaves one decision open: does Go code manipulate the *generated* messages, or does it define
its own domain types and convert at the boundaries?

## Decision

**Generated proto messages are the domain types.** `core` re-exports them as **type aliases**, not
defined types:

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

Because these are aliases, `core.Case` and `knov1.Case` are the *same type*. There is no conversion
layer, no second representation to drift, and the vocabulary (`CLAUDE.md` prime directive 2) reads
correctly at every call site.

All such types are **always passed by pointer** (`*Case`, `*Asset`). `go vet`'s `copylocks`
enforces this mechanically.

## Consequences

This design inherits four hazards from generated protobuf code. Adopting it is conditional on all
four mitigations, which are part of the decision rather than follow-up work.

### 1. `encoding/json` silently diverges from protojson

Proto3 JSON mapping requires 64-bit integers to be encoded as **quoted strings** and enums as
**names**. `encoding/json` — which has no idea it is looking at a proto message — emits bare
numbers and raw integers.

Concretely: `cost_usd_micros` marshals as `"1500000"` under protojson and `1500000` under
`encoding/json`. Any consumer written against the OpenAPI spec (generated from the same proto)
breaks on the second form. `Destination` and `Kind` — the enums that decide where an asset ships —
diverge the same way.

**Mitigation:** `depguard` bans `encoding/json` outside `api/`. `protojson` is the only sanctioned
marshaler for `kno.v1` types. A test asserts the quoted-int64 round-trip.

### 2. `DoNotCompare` is a runtime panic, not a compile error

Generated messages carry `DoNotCompare` (`[0]func()`). Writing `c1 == c2` directly is a compile
error — loud and safe. But boxing either side into `any` (a generic helper, a `map[string]any`, a
naive dedup set) makes it legal Go that **panics at runtime**: `comparing uncomparable type`.
`go vet` cannot see it, because the static type at the comparison site is `any`.

**Mitigation:** `forbidigo` bans `reflect.DeepEqual` repo-wide. Tests compare with
`google.golang.org/protobuf/testing/protocmp` via `go-cmp`.

*Precision:* the copy sentinel is `DoNotCopy` (`[0]sync.Mutex`) — a zero-size marker that is never
locked. It exists so `go vet` can flag value copies; it imposes no locking cost.

### 3. The Opaque API would break every struct literal at once

`protoc-gen-go`'s Opaque API replaces exported struct fields with generated getters, setters, and
builders. It exists precisely to eliminate the hazards above, and Google is pushing generated code
in that direction. If a future `protoc-gen-go` defaults to it for `kno.v1`, every
`&core.Asset{ID: …}` literal and every test fixture breaks simultaneously, and this ADR's central
claim — "no conversion layer, vocabulary reads correctly at every call site" — stops being true.

**Mitigation:** `api_level = API_OPEN` is pinned explicitly in `buf.gen.yaml`, not left to the
tool's default. `docs/debt.md#5` carries the trigger to revisit before any major bump.

### 4. Generated field names are not idiomatic Go, permanently and invisibly

`protoc-gen-go` capitalizes proto field names without applying Go's initialism convention: proto
`id` becomes `Id`, not `ID`; `url` becomes `Url`, not `URL`. `DESIGN.md`'s own illustrative Go
sketch writes `Asset{ID string}`; real code reads `a.Id`.

Because `gen/` and `*.pb.go` are excluded from linting (correctly — generated code is not ours to
lint), `staticcheck`'s `ST1003` initialism check never fires. So the Go vocabulary and the proto
vocabulary diverge on spelling **by construction**, invisibly, in tension with `CLAUDE.md`'s
"idiomatic Go over clever Go" rule.

**Mitigation:** none available without abandoning the decision. Accepted and recorded in
`docs/debt.md#6` with a trigger to set an explicit field-naming policy before 1.0.

## Alternatives considered

**Hand-written Go domain types, proto added later.** Rejected — directly contradicts `DESIGN.md`
and `CLAUDE.md`'s proto-first coordination rule, and guarantees the schema-drift rewrite the
design chose ceremony to avoid.

**Generated idiomatic Go value types + `To`/`From` proto converters at I/O boundaries.** Proto
stays the single schema source, but domain code gets comparable, `encoding/json`-safe,
Opaque-API-insulated, correctly-initialism'd types — dissolving all four consequences above rather
than mitigating them. Rejected: a converter layer is a second representation that can drift, and
"defined once, in protobuf" reads most literally as aliases. This was a close call, raised by
Phase-1 adversarial review and decided by the maintainer; `docs/debt.md#10` carries the trigger to
revisit if the lint gates prove insufficient.

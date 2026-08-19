# ADR-0002: Generated proto code is checked in under `gen/`

- **Status:** Accepted
- **Date:** 2026-08-18
- **Context:** [Plan: repo-foundation](../plans/2026-08-18-repo-foundation.md) (M0b)

## Context

`DESIGN.md`'s repository layout lists `proto/` as "THE contract" but is silent on where generated
Go code lands, and on whether it is checked in or generated at build time.

## Decision

Generated Go lands in a top-level **`gen/kno/v1/`** directory and is **checked into git**.

`gen/` is an addition to `DESIGN.md`'s layout, not a conflict with it — the design names `proto/`
as the source of truth and does not speak to codegen output. Confirmed with the maintainer rather
than taken silently, per `CLAUDE.md`'s rule on `DESIGN.md`/`CLAUDE.md` divergence.

## Consequences

**Why checked in:** `go get github.com/knograph/kno/core` works for consumers who have no `buf`
installed, and contributor setup stays at `go build`. A repository whose Go code does not compile
without a codegen step is hostile to the drive-by contributor that Ring-1 adapters are designed to
attract.

**Cost:** generated files appear in diffs and inflate review surface.

**Mitigations:**

- `.gitattributes` marks `gen/**` as `linguist-generated`, so GitHub collapses it in diffs and
  excludes it from language statistics.
- `make generate-check` fails CI if `make generate` produces a diff, so checked-in output can never
  drift from its `.proto` source.
- `gen/` is excluded from linting and formatting (`.golangci.yml`) — generated code is not ours to
  lint. The known cost of that exclusion is recorded in `docs/debt.md#6`.

**Codegen is local, not remote.** `protoc-gen-go` is pinned in `tools/go.mod` via Go's native
`tool` directive and installed into `./bin`. buf **remote plugins** were considered and rejected:
they add a network dependency and third-party binary execution at codegen time — a supply-chain
surface `CLAUDE.md`'s Security section otherwise takes seriously — to solve a problem `go tool`
already solves locally and reproducibly.

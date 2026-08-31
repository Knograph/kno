// Package pool supplies candidate Assets — the study material. Shipping
// adapters: jsonl, csv, markdown-dir, and Hugging Face.
//
// Two more are named in DESIGN.md and deliberately absent here. Parquet is
// deferred with a recorded trigger (docs/debt.md#83): nothing in the v0.1
// feature set consumes it, and a reader is a real dependency. MCP arrives in
// v0.3 — it makes kno an MCP client, so any of the hundreds of existing MCP
// servers becomes a pool with no Kno-specific connector code, and inheriting
// an ecosystem beats building one. Neither is importable today; this comment
// says so rather than letting a reader grep for a package that is not there.
package pool

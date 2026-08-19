// Package pool supplies candidate Assets — the study material. Adapters:
// jsonl, csv, parquet, markdown-dir, and MCP.
//
// The MCP adapter makes kno an MCP client, so any of the hundreds of existing
// MCP servers becomes a pool with no Kno-specific connector code. Inheriting
// an ecosystem beats building one.
package pool

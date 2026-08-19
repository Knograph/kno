// Package store persists runs, traces, and valuations in SQLite — zero-config
// for CLI and CI use.
//
// Stored traces are customer data: they may contain end-user conversation
// content. No trace content appears in log lines above DEBUG or in any span,
// a purge command exists, and retention behavior is documented plainly.
//
// The platform swaps Postgres in behind this interface.
package store

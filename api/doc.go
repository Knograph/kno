// Package api serves the connect-rpc API — gRPC and REST from one proto
// definition on one port, with SSE for the event stream.
//
// Resource-shaped rather than RPC-soup: runs, valuations, portfolios, and
// reports are resources. Long operations are async by default, mutations take
// idempotency keys so a retried CI job cannot double-spend, every expensive
// endpoint accepts estimate_only, and errors mirror the CLI grammar.
//
// Lands in v0.3.
package api

// Package tui renders the live run dashboard with bubbletea and lipgloss:
// per-stage progress, spend against budget, cases per second, ETA, and the
// asset currently under valuation.
//
// It renders the same typed events the API streams and the logs record — one
// event schema, several renderers, never a side channel. Piped or in CI it
// degrades to plain deterministic lines.
//
// Deltas are colored by whether the confidence interval crosses zero, not by
// sign. Honesty in the palette.
package tui

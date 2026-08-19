// Package bridge connects in-context measurements to fine-tuning outcomes.
//
// In-context gains do not reliably predict fine-tuning gains: ICL favors
// knowledge injection, FT favors behavior and format, and they diverge exactly
// where a naive tool would mislead. bridge closes the gap with mechanism
// routing (knowledge assets never reach the tuning set), a cheap ICL screen,
// proxy fine-tuning on a small open model for group-level ablation, and
// post-tune validation against the same untouched holdout.
//
// Lands in v0.2.
package bridge

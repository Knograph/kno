// Package tuner submits and tracks fine-tuning jobs on hosted APIs: openai,
// together, fireworks.
//
// Orchestration is HTTP calls — there is no torch in the Go binary. This is
// what makes proxy fine-tuning affordable enough to be a measurement rather
// than a commitment.
package tuner

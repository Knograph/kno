// Package budget is the spend guard.
//
// Every code path that can call an LLM or a fine-tuning API flows through it:
// estimate, confirm, record, checkpoint. A spend path that bypasses the guard
// is a P0 bug, because it spends someone else's money without consent.
//
// The guard ships before the first adapter deliberately — so every future
// spend path is written against an interface that already exists rather than
// retrofitted onto one. Lands in M0c.
package budget

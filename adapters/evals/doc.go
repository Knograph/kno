// Package evals supplies scoreable Cases — the exam. Adapters: jsonl, csv, and
// transcripts (via mine).
//
// Kept separate from pool because the exam and the study material are
// different things with different sources, and conflating them made --pool
// ambiguous.
package evals

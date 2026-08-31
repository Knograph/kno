// Package evals supplies scoreable Cases — the exam. Shipping adapters:
// jsonl, LangSmith, Langfuse, Braintrust, Hugging Face, and transcripts
// (via mine). The dev/holdout split they share lives in evals/split, so the
// denominator math cannot vary by source.
//
// Kept separate from pool because the exam and the study material are
// different things with different sources, and conflating them made --pool
// ambiguous.
package evals

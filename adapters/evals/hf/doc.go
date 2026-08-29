// Package hf reads eval Cases from a Hugging Face dataset split, served by
// the datasets-server API.
//
// # Grammar
//
// A split is addressed by four names, and only four: the dataset (org/name),
// the config, the split, and nothing else. That is why the CLI form is
// hf:<org>/<name>/<config>/<split> — four slash-separated segments. A
// revision is never one of them: the datasets-server ignores a revision
// query parameter, so the x-revision RESPONSE header is the fingerprint
// (finding F1 of the plan at
// docs/plans/2026-08-29-huggingface-adapters.md). Every page must answer the
// same x-revision as the first, or the read is dead: the split changed
// mid-stream and is no longer one object.
//
// # Mapping (F3, F5)
//
// The mapping is deliberately Ring-0-shaped. The input column is the first
// present of input, prompt, question, decided once at open from the first
// row — a dataset-level fact, not a per-row guess. A split with rows and no
// input column is refused at open, naming the columns the dataset actually
// has. Expected is ONE string, the first present of expected, completion,
// answer; other candidate columns are dropped, because a golden with two
// winners is a dataset that has not decided what it is testing. Structured
// values are canonical JSON, so a nested value maps identically wherever it
// appears; null input is fatal, naming the row.
//
// # What HF rows do not carry
//
// The weak-label signal of the jsonl adapter has no HF equivalent: rows
// carry no per-record derivation note, so nothing here is ever marked
// derived and CountSplits reports WeakLabelCases as zero. The absence is
// documented because it is a behavior difference, and silent differences
// are how reports mislead.
//
// # Cost and gating
//
// Reading a dataset costs nothing but the pages themselves; the credential,
// when a dataset is gated or private, is the HF_TOKEN environment variable,
// never a flag (a key on a command line lands in shell history and CI
// logs). A 401 — which datasets-server sends both for a missing name and
// for gating — is refused with both remedies. Self-hosted mirrors of
// datasets-server are a real use, so Options.Host and the Allow* fields
// exist; the default is the public server.
package hf

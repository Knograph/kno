// Package langsmith reads eval Cases from a LangSmith dataset.
//
// A dataset is the LangSmith unit the kno CLI takes as an eval set: the user
// names a dataset, this package resolves the name to a dataset id and streams
// its example rows, mapping each row to a core.Case with the same shape the
// jsonl adapter produces for a JSON Lines file.
//
// This adapter is read-only. It never writes to LangSmith, so there is
// nothing here that can spend money and no budget guard is consulted — the
// budget posture of this source is the posture of a file read.
//
// The dev/holdout division is the one shared package every Evals adapter
// uses (adapters/evals/split), so the denominator math cannot vary by
// source.
//
// Two error surfaces matter to callers:
//
//   - The error from Cases() is an OPENING error — the dataset could not be
//     resolved, or its first page could not be fetched. Nothing was yielded.
//   - An error YIELDED by the iterator is a fatal record error: a malformed
//     row, a duplicate example id, an oversized row, or an HTTP failure in
//     mid-pagination — a page after the first answered with a status other
//     than 200 (which names the dataset), a request timed out or was
//     refused, or the 429 retry budget ran out. The consumer must stop at
//     the first one, or the denominator behind every later delta shrinks
//     silently. The opened page's body is closed by the iterator's own
//     deferred cleanup the moment the error stops the stream, so the
//     consumer needs no cleanup of its own.
//
// The API key is environment-only: it is read from LANGSMITH_API_KEY, and
// there is deliberately no Options field or CLI flag that accepts a key
// value, because a key on a command line lands in shell history, ps output,
// and CI logs.
package langsmith

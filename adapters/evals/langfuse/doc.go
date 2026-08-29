// Package langfuse reads eval Cases from a Langfuse dataset.
//
// A dataset is the Langfuse unit the kno CLI takes as an eval set: the user
// names a dataset, this package resolves the name and streams its dataset
// items, mapping each item to a core.Case with the same shape the jsonl
// adapter produces for a JSON Lines file.
//
// This adapter is read-only. It never writes to Langfuse, so there is
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
//     resolved (a 404 names the dataset and the host), or its first page
//     could not be fetched. Nothing was yielded.
//   - An error YIELDED by the iterator is a fatal record error: a malformed
//     item, a duplicate item id (a dataset edited under the pagination —
//     the same item seen twice across a page seam), an item whose input is
//     null, an oversized row, or an HTTP failure in mid-pagination — a page
//     after the first answered with a status other than 200 (which names
//     the dataset), a request timed out or was refused, or the 429 retry
//     budget ran out. The consumer must stop at the first one, or the
//     denominator behind every later delta shrinks silently. The opened
//     page's body is closed by the iterator's own deferred cleanup the
//     moment the error stops the stream, so the consumer needs no cleanup
//     of its own.
//
// Weak-label honesty: an item harvested from a trace (sourceObservationId
// or sourceTraceId set) is marked Provenance.Derived, and CountSplits
// reports it as a weak label; a hand-authored item is not. This is the
// per-item counterpart of langsmith's uniform marking — the semantic
// difference is recorded in docs/what-the-numbers-mean.md.
//
// Credentials are environment-only: the host comes from LANGFUSE_HOST
// (default https://cloud.langfuse.com) and the keys from
// LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY. There is deliberately no
// Options field or CLI flag that accepts a key value, because a key on a
// command line lands in shell history, ps output, and CI logs. The keys
// travel as HTTP basic auth (public key as user, secret key as password),
// which is base64, not encryption — which is why a plain-HTTP endpoint is
// refused by default.
package langfuse

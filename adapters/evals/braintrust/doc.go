// Package braintrust reads eval Cases from a Braintrust dataset.
//
// A dataset is the Braintrust unit the kno CLI takes as an eval set: the
// user names a dataset, this package resolves the name and streams its
// events, mapping each event to a core.Case with the same shape the jsonl
// adapter produces for a JSON Lines file.
//
// This adapter is read-only. It never writes to Braintrust, so there is
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
//     resolved (an unknown name is a miss on the filter endpoint and is
//     refused naming the dataset and the host), or its first page could not
//     be fetched. Nothing was yielded.
//   - An error YIELDED by the iterator is a fatal record error: a malformed
//     event, an event with a null input, an event missing its id or its
//     _xact_id (the version counter the dedupe rule and the resume
//     fingerprint are keyed on), an oversized row, or an HTTP failure in
//     mid-pagination — a page after the first answered with a status other
//     than 200 (which names the dataset), a request timed out or was
//     refused, or the 429 retry budget ran out. The consumer must stop at
//     the first one, or the denominator behind every later delta shrinks
//     silently. The opened page's body is closed by the iterator's own
//     deferred cleanup the moment the error stops the stream, so the
//     consumer needs no cleanup of its own.
//
// Duplicate event ids are NOT fatal. Braintrust's pagination walks the
// dataset's version history — a later page may re-serve a row that already
// appeared, with an earlier _xact_id — so the adapter merges by id, keeping
// the first (newest) occurrence and dropping the duplicate. An edit
// mid-pagination surfaces as exactly these duplicates, and the merge rule
// is the response rather than a refusal (plan P0-2, fixture-pinned).
//
// Weak-label honesty: an event carrying an origin object — Braintrust's
// record of "copied from another object" — is marked Provenance.Derived,
// and CountSplits reports it as a weak label; a hand-authored event is not.
// This is the per-item counterpart of langsmith's uniform marking — the
// semantic difference is recorded in docs/what-the-numbers-mean.md.
//
// Credentials are environment-only: the host comes from
// BRAINTRUST_API_BASE_URL (default https://api.braintrust.dev, the same
// base URL the vendor's own SDKs default to), the key from
// BRAINTRUST_API_KEY, and the optional org selection from
// BRAINTRUST_ORG_NAME (a query parameter, for keys that span orgs). There
// is deliberately no Options field or CLI flag that accepts a key value,
// because a key on a command line lands in shell history, ps output, and CI
// logs. The key travels as a Bearer token on every request — which is why a
// plain-HTTP endpoint is refused by default.
package braintrust

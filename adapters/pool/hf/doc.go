// Package hf reads candidate Assets from a Hugging Face dataset split,
// served by the datasets-server API — the study material half of the
// Hugging Face story, sibling to the Evals adapter at
// adapters/evals/hf. The two share the transport and the open-time
// discipline through adapters/internal/datasetserver; this package owns
// what a row becomes.
//
// # Kind is declared, never guessed
//
// An Asset's Kind is a routing decision — knowledge assets go to one
// destination, behavior assets to another — so the kind is part of the
// source address: hf:<org>/<name>/<config>/<split>:<kind>. An hf pool
// without a declared kind is refused at construction. The CLI cannot pass a
// kind it did not name, and the dataset cannot say what a user intends to
// do with its rows.
//
// # Content
//
// A row's text-bearing columns — the columns whose values are JSON strings —
// become the Asset content, one "name: value" line per column, sorted by
// column name so the bytes are deterministic whatever order the server
// emits keys in. Nulls, numbers, and structured values are not text-bearing;
// a row with no text-bearing column at all is fatal, naming the row, when
// the split has text to begin with. The cost estimate is the shared one:
// context tokens from bytes over the fixed divisor, exactly as the markdown
// and CSV pools estimate (docs/debt.md#68).
//
// # Identity
//
// An Asset's id is <dataset>/<config>/<split>@<row_idx>: the server's own
// addressing, stable across re-reads at the same revision. The x-revision
// discipline is the Evals adapter's, verbatim — a page whose header drifts
// from the first is a split that changed mid-read, and that is fatal.
package hf

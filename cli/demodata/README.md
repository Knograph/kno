# `kno demo`'s scenario — and why you must not shrink it

These two files are embedded into the binary (`//go:embed` in `cli/demo.go`) and written
verbatim into `kno-demo/` by `kno demo`. They are the same twelve Cases and three Assets
`tapes/quickstart.tape` records, so there is exactly one demo scenario in the tree.

The comment block belongs beside the data, and JSONL has no comment syntax — the reader
(`adapters/evals/jsonl`) treats a malformed line as fatal rather than skipping it, which is
the right call for a user's eval file and the reason this prose lives in a sibling file
instead of on a `#` line.

## The count is load-bearing

**Twelve Cases. Not nine, not eleven.** The dev/holdout split is keyed on the Case id, so
twelve Cases put eight in dev and hold four back. `value` then reserves
`value.DefaultControlReserve` (0.3) of dev for the control arm — `int(8 * 0.3)` = **two**
control Cases, which is the minimum `interval.compute` will form an interval from (it
returns nil below n=2).

At nine Cases the arithmetic collapses: six dev, `int(6 * 0.3)` = **one** control Case,
every interval comes back nil, the value table renders "sample too small or ragged to form
an interval" for every row, and `select` rejects on `underpowered` rather than `no-effect`.
The demo's epilogue — which claims deltas of `+0.0000` *with their intervals*, and an empty
Portfolio because every corrected interval crosses zero — would then be a lie the screen
never showed. The tape spent its first life making exactly that claim.

So: the epilogue's truth is what this fixture size protects. `TestDemoTranscriptGolden` and
`TestDemoWritesTheEmbeddedFixturesVerbatim` fail if a Case is trimmed. Do not shrink these
files without re-reading what the output actually says.

Twelve also keeps the warning the demo wants on its first screen: at
`split.DefaultHoldoutFrac` (0.2) the holdout is four Cases, below `split.MinHoldout` (20),
so the baseline renderer prints the too-small-holdout caveat.

## Do not "improve" the numbers

`fake:` answers every Case with what the Case expects, so the baseline score is 1.000 by
construction, and `contextAgent.Invoke` delegates to it unchanged — so injecting an Asset
cannot move a deterministic answer and every delta is exactly `+0.0000`. No choice of Assets
can change that. Picking flattering content would buy a prettier screen and zero additional
truth, and it would turn a demo into a promise the product cannot keep on the user's own
data. The empty Portfolio is the product refusing to recommend something, which is the one
screen here that an eval harness does not have.

`tapes/quickstart.tape` carries the same prohibition. Both are enforced by acceptance
criterion 6 of the plan, `docs/plans/2026-08-30-kno-demo.md`.

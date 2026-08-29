// Package mine turns production transcripts into weak-label eval Cases.
//
// Wherever a human corrected the agent in a conversation — "No, it should be
// X" — that exchange becomes a Case whose expected outcome is what the human
// said it should be. The result is a weak-label import for `kno baseline`,
// not a synthetic-data generator: every mined record is an exchange that
// actually happened, and every record carries the provenance fields the
// jsonl adapter round-trips into Case.Provenance, so a weak-label eval set
// cannot pass for a hand-authored one.
//
// # Goal-mode declaration
//
// Mined Cases carry no rubric. A mined Case's expected text is what a human
// typed, in the human's own words — shaped only by removing the chit-chat
// around it (see classify.go). It is therefore honest under judge goals,
// where a judge reads input and expected together and decides whether the
// agent's answer matches the expectation's substance. Under exact-match it
// is honest only when --mode resolution shaped the expected to a short
// answer: a resolution is what the human settled on at the end of a thread,
// which the agent had to converge to, whereas an immediate reply was never
// told to the agent as a requirement. Consumers of a mined set keep that
// caveat by construction: the Run records how many Cases in the eval set
// are derived, and the report prints the count when it is nonzero.
//
// # Modes
//
// ModeResolution (default) uses a thread's final human message as the
// expected — the answer that closed the thread. ModeImmediate uses the
// human reply after each agent answer, with the modal reply classes
// filtered and counted (gratitude, acknowledgment, escalation, quote-back,
// retraction, counter-question).
//
// # Formats
//
// The three pinned input schemas, selected by --format or sniffed from the
// first non-empty line:
//
//   - jsonl-chat: one JSON message per line:
//
//     {"id": "m1", "role": "user", "content": "…", "timestamp": "RFC3339", "thread_id": "t1"}
//
//     id is required; role is from the closed vocabulary assistant, agent,
//     user, human; timestamp and thread_id are optional. Unknown fields are
//     fatal.
//
//   - markdown: **Speaker:** message lines (--agent-name marks the agent;
//     every other speaker is human), optional H1 titles and --- rules as
//     thread boundaries, optional ISO-8601 timestamp line after a speaker
//     line, continuation lines until the next speaker line. The run prints a
//     speaker inventory and a per-file pairing summary.
//
//   - csv: a header row naming the question and answer columns
//     (case-insensitive). A row whose question or answer is empty is fatal,
//     like a blank id in jsonl-chat. A question,answer row IS the pair —
//     input is the question, expected is the answer — so csv is independent
//     of --mode.
//
// The mined output is the jsonl adapter's record format plus the additive
// provenance fields (derived, derivation_note, source_ref), one Case per
// line, ready for `kno baseline --cases` and for re-reading by the same
// adapter that ingests it.
//
// # Identity
//
// A mined Case's id is a pure function of its content — thread identity,
// question, expected, the token cap, and the parser version — shaped like
// a ULID with a timestamp prefix from the exchange's own transcript time
// (see id.go). Re-mining the same transcripts yields the same ids, and the
// dev/holdout split, which is keyed on Case ids, never moves a mined Case
// when a transcript grows.
//
// # Review
//
// --review presents each mined Case for keep / edit / drop. Decisions are
// written to a manifest beside the output (format.go), and re-mining reads
// the manifest back: a curated drop can never resurrect and an edited
// expectation is re-applied on the next run. Review is refused without a
// terminal.
package mine

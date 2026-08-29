# Ring-1 adapter batch: exec agent, csv/parquet/markdown pools

Four small Ring-1 adapters from DESIGN's v0.1 list, batched into one plan because they share
the adapter template (fixtures, conformance harness, capability matrix) and none touches core.

## Problem

DESIGN:397's v0.1 bar names shell agents and csv/parquet/markdown pools. Today `exec:` is
advertised-but-missing (`cli/agentwiring.go` prints "exec: and tuned: land in a later
milestone" — the milestone is this one), and the only Pool source is JSONL. Every non-JSONL
data source in the cookbook requires a manual export step first.

## Design

### `exec:` shell agent (`adapters/agent/exec`) — its own PR

**Phase-1 re-review split this batch.** The exec adapter is a larger security surface than
the HTTP transport ever was (arbitrary subprocess, environment inheritance, kill
semantics, output handling), and the repo's own precedent keeps security boundaries out
of batches (debts #37/#38: "a diff that is the milestone's security boundary"). **exec
ships alone; the pool adapters ship together.** *(P2-4)*

- **Contract**: `exec:<command>` runs a subprocess per Case — the agent is "any
  executable". Input: the Case's input on stdin; output: the agent's answer on stdout,
  judged by the Goal. A nonzero exit = errored Case (provider-failure classification, not
  a score of zero). The ref grammar already accepts the scheme
  (`adapters/agent/agentref` defines `SchemeExec`); the DECISION this plan pins is how
  the target string becomes a process: **no `sh -c`** — the ref string is split into
  argv directly (shell interpretation of a ref is a command-injection door, and the
  split rule is pinned by tests). *(P3-3)*
- **The environment, redesigned** *(P1-1)*: the first draft's "no ambient credentials"
  was false — a nil `Env` inherits the parent's full environment, every key included,
  and output-redaction cannot un-exfiltrate. The child is spawned with an explicit env
  **allowlist**: `PATH`, `HOME`, `TMPDIR`, plus `--exec-env KEY=VALUE` grants — the
  plugin posture CLAUDE.md already mandates ("plugins get no ambient credentials; they
  receive only what config explicitly grants them"), and this adapter is the plugin
  protocol's ancestor. The test pins that a key exported in the parent is NOT visible to
  the child.
- **Cost and consent, redesigned** *(P1-2)*: `Spends()` returns `costPerCall > 0` —
  free exactly when the user DECLARED it, not by structural fiat. Consequences pinned:
  with the override set, each Response is priced with the scalar so the report and the
  settled spend are honest (the first draft would have shown $0.00 per Case even with
  the override); with no override, the consent prompt DOES fire on a capped run (the
  zero-scalar early-return the first draft triggered is exactly the silent-overspend
  shape the guard exists against); and the Value-stage ranking denominator — the plan
  states it — treats zero-cost arms as unranked-by-cost, so a free arm cannot silently
  outrank paid arms by dividing by zero (the bias class debts #65/#68 exist to make
  visible).
- **Kill, timeout, retry** *(P2-2)*: process-group spawn (`Setpgid` + group kill,
  TERM-then-KILL) so a script's children do not outlive the timeout or the Ctrl-C; the
  hang fixture becomes a parent-with-child fixture. A hung or deterministically-failing
  script is **NOT retryable** — the transport's retry premise is transient provider
  state, and a local script that hung once hangs again; retrying multiplies wall-clock
  by `MaxAttempts` for nothing. Both streams are capped (stderr becomes error context —
  uncapped, it defeats the memory bound), and the error context is truncated so a script
  debug-printing its stdin does not turn Case content into stored error text.
- **ContextInject: NOT declared in v0.1** *(P3-1)*: stdin already carries the Case, env
  blows up at ~128KB on real Assets, and the framing decision will be inherited by the
  Ring-2 protocol — designing it now without the plugin protocol's shape is the trap.
  The capability matrix says exec declares no `context_inject`; Value's V-4c refusal
  therefore correctly refuses exec arms for injected measurement.
- **Wiring refusals** *(P3-4)*: `--base-url` with an `exec:` ref is refused at wiring
  (composeRef would silently compose `exec:cmd@https://…`, and the silent-ignore shape
  is what the wiring file exists to refuse).

### Pool adapters (`adapters/pool/csv`, `adapters/pool/markdown`; `parquet` below)

- **csv**: `Pool` from a CSV — `id` column **required, fatal when absent** *(P2-1)*: the
  first draft's row-number fallback re-numbers every id on any insertion, orphaning every
  paid measurement and silently re-splitting the holdout between Baseline and Value — the
  exact failure jsonl's own comments reject. A content-hash fallback exists behind an
  explicit opt-in flag whose help text states the stability difference. `content` column,
  optional `kind`/`tags`; malformed rows fatal, named, never skipped.
- **markdown**: `Pool` from a directory of `.md` files (or one file with `## `-level
  sections as Assets when `--split-sections` is set — one doc is not one Asset). `id` =
  file path (sections: path + a defined separator rule — a heading containing the
  separator is escaped, and duplicate section headings in one file are FATAL, matching
  jsonl's duplicate-id refusal). Front matter (`kind`, `tags`) read when present.
- **parquet**: DESIGN names it, and it is the one adapter with a real cost: a parquet
  reader is a dependency. **Deferred with a disposition that can actually lapse** *(P2-3)*:
  the first draft's trigger "when a user files it" was the exact report-conditioned shape
  debt #46 records as REJECTED (it can never lapse, so CI's release check can never fire
  on it). New trigger: **"the first Tuner PR, or v0.1.0, whichever is first"** — and the
  entry's why records that this is a DESIGN-scope disposition (parquet is on the v0.1
  bar's adapter list) with the cheaper-than-claimed cost named: a pure-Go parquet reader
  is the same dependency class the repo already carries (`modernc.org/sqlite`).
- All three: `coretest.ConformIterator` + `CleanupProbe` + the iterator-contract rules
  (fatal errors, ctx checks, borrowed values, cleanup inside the closure) — the same
  proofs `jsonl` carries. The `Pool` interface's contract is identical to `Evals.Cases`;
  the conformance harness is interface-agnostic.

## Alternatives considered

**One generic "table" adapter for csv/parquet.** Rejected: DRY-with-judgment — the
adapters share the iterator skeleton but their error grammars and id rules differ enough
that a table-of-formats switch is the wrong abstraction at n=3 (extract on the third
occurrence, and this IS the third — the shared part is the conformance harness, which
already exists).

**Parquet via a CLI extractor instead of a reader.** Rejected as worse than deferral: a
half-integration is the two-step export the LangSmith plan rejected for the same reason.

**exec as a plugin instead of a first-party adapter.** Rejected: the exec protocol IS
Ring-2's design target, and exec-first-party is the cheapest way to learn what the plugin
protocol must carry before v0.3 freezes it (the same argument DESIGN uses for Ring 2's
timing).

## Affected packages

`adapters/agent/exec` (new), `adapters/pool/csv`, `adapters/pool/markdown` (new),
`cli/agentwiring.go` (the exec branch and the removed "later milestone" line),
`cli/poolwiring.go` or equivalent (pool source dispatch), `docs/debt.md` (parquet re-date),
CHANGELOG.

## Proto / schema impact

None. `exec:` rides the existing Agent interface; pools ride the existing Pool interface.

## Edge cases

| Case | Behavior |
|---|---|
| exec command not found | Construction-time refusal with the fix line (before any Case runs) |
| exec hangs | Per-call timeout, errored Case (not run-fatal — a hung script is like a hung provider call, retryable per the transport rules) |
| exec emits > cap | Truncated and counted, Case errored with the cap in the fix line |
| exec exits nonzero with useful stderr | stderr becomes the error context, capped and truncated — a script debug-printing its stdin must not turn Case content into stored error text. No output-scrubbing exists, because the env the child receives is already the allowlist — there are no ambient credentials to echo |
| CSV without an id column | Fatal, named (matching jsonl); content-hash fallback only behind the explicit opt-in flag |
| A key exported in the parent env | Not visible to the exec child — the allowlist test pins it |
| Markdown with no front matter | File-path id, content = whole file, `kind` unset (routing judges) |
| Empty directory / empty CSV | Zero Assets, exit 0 with a count, not an error |

## Test plan

- exec: fixture scripts (good, failing, hanging, huge-output, echoing-env); the caps; the
  no-credential-inheritance statement tested; `Spends() == false` path through the consent
  skip.
- csv/markdown: golden files, malformed-row fatal tests, conformance harness, the
  section-splitting rule, front-matter parsing.
- Capability matrix entries for exec (which capabilities it declares — the CLI's
  construction-time check must accept it).
- Snapshot tests for the new agent grammar (`exec:`) and pool grammars (`--pool
  csv:path`, `--pool md:dir` — exact grammar decided at implementation, pinned by
  snapshot).

## Rollback

Delete the packages and the wiring branches.

## Docs impact

CLI help snapshots, cookbook ("Bring your own agent via exec", "Pool from CSV/Markdown"),
CONTRIBUTING's adapter on-ramp examples (these are the designed on-ramp), debt ledger
(parquet re-date), CHANGELOG.

## Accepted risks

- **exec is a footgun for the unwary** (arbitrary subprocess per Case). Documented; the
  env allowlist, process-group kill, caps, and non-retry semantics are the mitigations;
  the plugin boundary (v0.3) inherits the lessons.
- **A zero-cost exec arm is unranked-by-cost in Select.** Stated in the docs — the
  alternative (ranking by division-by-zero) silently outranks paid arms and is the bias
  class debts #65/#68 exist to make visible.
- **CSV is schema-less by nature.** The column-name contract is documented and validated
  loudly; wrong columns are a load error, not a guess.
- **Parquet deferred.** Disposition recorded in the ledger with a trigger that can lapse
  (first Tuner PR, or v0.1.0), per the rules.

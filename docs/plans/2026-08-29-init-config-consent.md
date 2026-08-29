# init, the config layer, and the interactive consent prompt

Fires [`docs/debt.md#62`](../debt.md#62) (no env/kno.yaml configuration layer — DESIGN:294
specifies three layers, M2-11 shipped flags only) and [`#59`](../debt.md#59) (the interactive
spend-confirmation prompt does not exist; `confirmFunc` always declines and prints "Re-run with
--yes"). Both are named as the triggers' owning PR.

## Problem

A user's first five minutes are: install, write Cases, run. The fifth command onward needs
configuration — provider, key binding, cap, goal, concurrency — and today that is either flags
retyped every run or nothing (defaults silently). `kno init` is DESIGN's answer: a wizard that
writes one commented `kno.yaml`. Separately, a real-provider run without `--yes` cannot be
confirmed at all: the CLI declines and tells the user to re-run, which is not a consent flow,
it is a refusal flow. Prime directive 4's consent surface is the gap.

## Design

### `kno init` (huh wizard)

- A `huh`-based wizard (`charmbracelet/huh` — new dependency, justified below) asking, in
  DESIGN's order: agent/provider + model, key binding (variable name; a value the user
  pastes is refused — the credential grammar the CLI already enforces), default Goal,
  holdout fraction, cost caps (money + calls), concurrency, output directory. Every answer
  is mirrored by a flag and a `KNO_*` env var, in the precedence order DESIGN:294 fixes:
  **flag beats env beats file beats default**.
- Writes `kno.yaml` — heavily commented, one line per answer, the wizard's questions as the
  comments. `kno init` re-running MERGES: the wizard PRE-FILLS from the existing file (never
  from flags or defaults), enter-through means "unchanged", keys the wizard does not cover
  are round-tripped (a hand-edited `temperature:` survives re-init), and the write is atomic
  (temp + rename — a crashed write must not corrupt the one file every command reads).
  `--force` overwrites, and it REFUSES a file whose `version` is newer than this binary
  understands (overwriting the future is how data dies). The wizard's "output directory"
  answer maps to the existing `--db` flag's path — the plan invents no flag without a
  mirror, per its own bijection rule.
- **Schema**: YAML (no new parser — a `kno.yaml` schema is a post-1.0 covenant per the
  ledger, so v0.1 ships a documented, validated, versioned file with a `version: 1` field
  and a `kno doctor`-style validator; the schema itself is deliberately simple: flat keys,
  no nesting beyond what the flags already express). Validation fails loud on unknown keys
  — and the load error's fix line says "upgrade kno" when the file's `version` is newer,
  versus "fix the key" for a typo, because an older binary cannot tell the two apart and
  hard-refusing with the wrong fix line is the silent-breakage failure this layer exists to
  prevent.
- **The schema key set, pinned** (Phase-1 review): snake_case mirrors of the run-shaping
  flags only. **Excluded from the file**: `yes` (a committed `yes: true` is a silent
  consent waiver in a shared repo), `json`, `resume`, `trace-spans` (per-invocation),
  `allow-insecure-base-url` / `allow-private-address` / `unsafe-baseline` /
  `accept-unknown-cost` (security booleans exist to be deliberate per-run choices — a
  committed `allow_insecure_base_url: true` is an ambient TLS downgrade for every
  teammate). **Validated at load, not just at the flag layer**: negative caps refused,
  the `price-input`/`price-output` pair rule re-applied (half a pair refused, same as
  `validateCaps`), and `agent:` / `base-url:` values run through `agentref.Parse` — the
  validator that already exists, because a credential in a base-URL path is accepted at
  runtime today (debt #60) and the file layer must not become a second door for it.
- **The env mirror and the existing collision, resolved**: `KNO_MAX_COST_USD` already has
  a meaning — debt #11's live-test spend cap, with missing-variable-is-fatal semantics in
  the live tests. The env mirror for `--max-cost-usd` is `KNO_MAX_COST_USD` per DESIGN's
  mirror rule, and the collision is resolved by precedence, not by renaming: the file and
  env layers feed the CLI flag default; the live-test guard reads the same variable only
  in `KNO_LIVE_TESTS=1` runs, which are the one context where the cap's meaning is
  identical. Stated here rather than left to surprise the nightly job.
- **The loader discriminates by `Changed()`, not by value.** The classic config-layer bug
  is live in this flag surface: `--holdout-frac` defaults to `split.DefaultHoldoutFrac`
  (0.2), so a file saying 0.2 is indistinguishable from the default by comparison. The
  repo already has the mechanism (`cmd.Flags().Changed(...)` at `cli/baseline.go:104-105`),
  and the loader uses it: a flag the user did not set is unset, and only then do env, file,
  and default apply. Equality traps enumerated in the test plan: `concurrency: 0` (a
  sentinel meaning "conservative default"), `cost-per-call-usd: 0` (both the default and
  an explicit claim — "0 asserts the calls are free" — which the file layer must
  preserve), and the repeated `--key-env host=VAR` list (file entries APPEND to flag
  entries, never replace — pinned by test).
- **Fires #62's other half**: the two stale fix lines — `ErrBudgetExceeded.Fix` ("raise
  max_cost_usd in kno.yaml") and `ErrRateLimited.Fix` ("lower concurrency in kno.yaml") —
  become TRUE in the same PR, per the entry's "corrected in the same PR or earlier".

### The interactive consent prompt (#59)

- `cli.confirmFunc` stops declining. On a spend-estimated run, print the estimate, then
  prompt: **yes / no / yes-with-adjusted-cap**. The third option is the consent flow prime
  directive 4 wants: the user edits the cap, the guard is REBUILT pre-run with the new cap,
  and the flow re-quotes the SAME bounded figure — one number in one flow.
- **The prompt is a CLI pre-run dialog, not an engine-side mid-run surface.** The
  engine-side PreConfirm quote (`core/baseline_budget.go`) stays as the fail-closed
  backstop for callers that construct a guard without the CLI in front of them; the CLI's
  ConfirmFunc returns the recorded decision, so nothing prompts twice. The quote the
  prompt shows is the BOUNDED figure — on a fresh run, the core-estimated total; on a
  resume, the remainder after `SettledSpend` (the CLI already reads the store for its
  reports, so it computes the same number PreConfirm would, and the two cannot disagree
  because both read the same persisted spend).
- **The #44 prompt-half channel, pinned**: an additive plain struct field on
  `budget.Estimate` — `Width *WidthDecision` (`WidthDecision{Requested, Effective int,
  ReducedReason string}`; a plain struct, because `stats/budget` imports no proto). The
  three CLI closures at `cli/render.go:63,70,77` read it; the future API caller ignores it
  (additive). `checkFeasible` populates it on the main run-open path, where it already
  runs before `confirmRun` (`core/baseline.go:384-387`) — so the prompt quotes "width 32
  → 5 (headroom)" when the engine narrowed it, closing #44's prompt half in the PR that
  owns the prompt.
- **Prompt mechanics**: stdlib `bufio` line-read behind `isatty(stdin) && isatty(stdout)`
  — both directions, because `kno baseline < /dev/null` (stdin not a TTY) must not hang
  and `kno baseline | tee` (stdout not a TTY) must not pollute the pipe. Non-TTY keeps
  today's behavior exactly: printed message + refusal (exit 2). A prompt failure fails
  closed as the same exit-2 refusal; SIGINT at the prompt is exit 4 (interrupted,
  resumable). GHA is not the whole CI story — some runners allocate a pty, so TTY
  detection is explicit, not "CI means no TTY".

### CLI grammar

`kno init [--force]`; config loading happens in every command after flag parsing: file
(default `./kno.yaml`, `--config` to move) → env (`KNO_*`, one per flag) → flags. A loaded
file's keys are reported at `-v` and in `kno doctor`. Secrets: kno.yaml holds VARIABLE
NAMES only — a key VALUE in the file is refused at load with the fix line ("bind
host=VAR, values come from the environment").

## Alternatives considered

**Flags-only forever (status quo).** Rejected: DESIGN:294 is the decision; the two stale
fix lines are live defects that name a file that does not exist.

**Full TUI dashboard in this plan.** Rejected: the dashboard needs the event stream's
final shape and every stage landed; the consent prompt is the only TUI surface with a
prime-directive deadline. Deferred below.

**Viper/koanf config library.** Rejected: a config dependency for a flat, versioned,
validated file the repo's own grammar rules can enforce in ~150 lines; the validator is
the part that must be testable, and a library does not write the validator.

## Affected packages

`cli/` (init command, config loading, confirmFunc), `cmd/kno`, `docs/` (cookbook "configure
kno with kno.yaml", mental-model precedence note), `docs/debt.md` (#62 repaid, #59 repaid
+ #44's prompt half).

## Proto / schema impact

None. `kno.yaml` is a file schema, not a wire schema; it is versioned from day one because
post-1.0 it becomes a covenant.

## Edge cases

| Case | Behavior |
|---|---|
| kno.yaml missing | Every command works exactly as today (file is optional) |
| Unknown key in kno.yaml | Load fails loud, naming the key and the fix (a typo must not silently no-op) |
| Key VALUE in kno.yaml | Refused at load with the credential grammar's fix line |
| A flag beats env, env beats the file, the file beats default | Pinned by a precedence test, not a comment — the order is DESIGN:294's, and this table states it in that order |
| `--holdout-frac` unset vs a file saying 0.2 | Indistinguishable by value; the loader discriminates by `Changed()` — the equality rows are in the test plan |
| Re-running init over an existing file | Merge: keep existing answers, ask only what changed |
| Non-TTY stdout during consent | No prompt; `--yes` required; `--json` unaffected |
| Non-TTY stdin with TTY stdout (cron `< /dev/null`) | Must not hang — detection is `isatty(stdin) && isatty(stdout)`, both directions tested |
| No kno.yaml and `ErrBudgetExceeded` fires | The fix line is dynamic: file-aware when a file exists, the old static line otherwise — the defect #62 names is a fix line pointing at a file the user does not have |
| Consent prompt during `--yes` | Skipped; the printed estimate remains (informed blanket flag, per #59) |
| CI/n8n (no TTY) | Unchanged behavior — the exit-code contract is untouched |

## Test plan

- Precedence tests: flag > env > file > default, one table, every flag with a mirror —
  plus the EQUALITY rows the value-comparison loader would get wrong: file `holdout_frac:
  0.2` vs an unset flag (file wins), `concurrency: 0` sentinel preserved, `cost_per_call:
  0` preserved as the explicit "free" claim, and `key_env` list entries appending rather
  than replacing.
- Schema validation tests: unknown key, wrong type, missing version, key-value-in-file.
- init merge behavior: second run keeps existing answers (golden-file the kno.yaml output).
- Consent prompt: driven through huh's test harness (huh ships one); yes/no/adjust-cap
  paths; non-TTY path; `--yes` path; the #44 width line in the quote.
- The two fix lines from #62: a test pins that `ErrBudgetExceeded.Fix` and
  `ErrRateLimited.Fix` name kno.yaml AND that a file with the matching keys actually
  changes the behavior (a fix line that points at a dead file is the defect the entry
  exists for).

## Rollback

Delete init and the loading branch; kno.yaml stops being read; behavior returns to
flags-only. No store or proto change.

## Docs impact

Cookbook ("Configure Kno with kno.yaml" — replaces flag-recitation in every recipe's
preamble), mental model (precedence), CLI help snapshots, CHANGELOG, ledger rows.

## New dependencies

`charmbracelet/huh` for the **wizard only**. Corrected dependency facts: charmbracelet
appears ZERO times in `go.mod` today (CLAUDE.md names the intended logging stack; the code
does not use it), so huh pulls the bubbletea family fresh — ~20 modules — and that weight
is stated here rather than inherited silently. The wizard earns it: multi-field forms with
inline editing and pre-fill are the huh use case. The **consent prompt does not need huh**:
a three-choice menu is ~40 stdlib `bufio` lines behind `isatty` (isatty is already in the
module graph via mattn/go-isatty), testable with a pipe, and it avoids running a raw-mode
terminal takeover from inside a spend path. License: MIT. Maintenance: charmbracelet org,
active.

## Accepted risks

- **`kno.yaml`'s shape will churn pre-1.0.** The churn is NEW KEYS at `version: 1`, not
  renames — the version field mitigates renames, not key addition, and an older binary
  cannot distinguish a new-key file from a typo. Accepted: the older binary hard-refuses,
  and its fix line says "upgrade kno", not "fix the typo". Mixed-binary households (one
  `brew upgrade` and one `--resume` apart, per debt #31) get a loud refusal, which is the
  honest failure.
- **Consent flow cannot cover the SIGKILL window** (#20) — the prompt is about the
  ESTIMATE, and the ledger already owns the unobservable window.
- **The dashboard stays minimal.** The verdict-table dashboard is deferred to the TUI
  milestone proper; this plan delivers the two TUI surfaces with deadlines.

## Deferred

The full TUI dashboard (live event rendering, verdict table, run control). Trigger: after
`select`/`export` land and the event schema stops moving — the dashboard renders state
that does not exist yet, and building it first is the trap #42 records.

# Pricing drift detector

Repays [`docs/debt.md#40`](../debt.md#40). Supersedes the first draft of this plan, which
proposed a code-generating worker that opened pull requests. Phase 1 blocked it; this is the
rewritten scope. The generator is deferred, not abandoned — see "What is deferred".

## Problem

`adapters/agent/pricing.Version` is `"2026-08-21"` and the table is hand-entered. Providers
reprice and ship models continuously. Two failure modes, not symmetric:

- **A stale price** produces a wrong estimate. Bounded — settlement reconciles against the
  provider's reported usage, so the spend is right even when the estimate was not.
- **A missing or wrongly-matched model** produces a wrong *ceiling*. `docs/debt.md#46` is
  exactly this, found by fetching the provider's live model list during review: nothing in
  the repo was watching, and a human reading a page found it in minutes.

Nothing today tells us either has happened.

## What this builds

A **detector**. It fetches, compares against the committed table, and reports. It generates
no code, opens no pull request, and holds no write token.

```
make pricing-check     (also a scheduled CI job)
  ├── fetch     three sources, read-only
  ├── compare   committed table vs each source
  └── report    non-zero exit + an issue, or silence
```

The output is a failing job and an issue. A human then edits `table.go` by hand — a
thirteen-row file — and that edit arrives as an ordinary reviewed diff with every rationale
comment intact. That is what debt 40's trigger asks for: *"a price change is reviewed like
code."* A generator is one way to get there; it is not the only way, and it is by far the
more dangerous one.

## Why not the generator (the superseded draft)

Phase 1 established four things I had wrong:

1. **My gate threshold was set at the amplitude of the most likely bug.** I proposed failing
   on price moves >50%. Anthropic's batch-pricing table is *exactly* −50% on every row, and
   the modal scraper error is picking the wrong table for the right model name. The gate
   passes the error it exists to catch.
2. **`table.go` is not a data file.** It carries the only in-code pointer to
   [`docs/debt.md#41`](../debt.md#41), the `nil`-versus-zero rationale that
   `EstimateWithPrice` depends on, and `Lookup`'s measured-incident comment. A generator has
   no source for any of it. It also has no memory of deliberate omissions — `claude-mythos-5`
   is absent on purpose and would be re-proposed every week forever.
3. **My round-trip byte-identity test does not prove what I claimed.** It constrains the
   renderer only until the first legitimate formatting change, which regenerates the golden
   and reformats every line *in the same diff* — precisely where one changed digit hides.
4. **My OpenRouter margin claim was unmeasured and wrong.** `anthropic/claude-opus-5`
   reproduces Anthropic's published rates to the cent across all five dimensions. The
   conclusion (not a price of record) survives for a different and better reason: it *can*
   disagree, and does — `openai/gpt-5.6-sol` is reported at half what OpenAI's own page says.

That last one is the whole design. **Disagreement between two independent sources is a
signal with no free parameters,** where "moved more than 50%" is a number I made up.

## Design

### Sources

| Source | Mechanism | Role |
|---|---|---|
| OpenRouter | `GET /api/v1/models`, JSON | Cross-check + model discovery |
| Anthropic | HTML, real `<table>` markup | Price of record for `anthropic` |
| OpenAI | HTML, **zero `<table>` elements** — nested divs, hashed CSS class names | Price of record for `openai` |

The two HTML pages are different problems, not one. Anthropic's is a table-selection problem
(13 tables, "Claude Opus 5" appears in four). OpenAI's has no tables at all, and the only
stable anchor is label text (`Input`, `Cached Input`, `Output`) with a sibling lookup —
any selector keyed on the build-hashed class names breaks on an unrelated CSS build.

### The checks, in order of how much they are trusted

1. **Cross-source agreement.** Where two sources price the same model, they must agree. No
   threshold, no tuning. Verified by hand today: it reproduces Anthropic exactly and flags
   the live `gpt-5.6-sol` disagreement.
2. **Header-literal assertion** for Anthropic: refuse unless the parsed table's header row
   equals a committed literal. Catches wrong-table, column-shift, and layout change
   deterministically, where a model-count heuristic cannot — the standard and batch tables
   have identical row counts.
3. **Ratio invariants**, per provider, which held on 100% of live rows at review time:
   `cache read = 0.10 × input` for both providers; for Anthropic additionally
   `5m cache write = 1.25 × input` and `1h cache write = 2.00 × input`. A row violating its
   provider's invariants is a parse error regardless of magnitude. Paired with (2), never
   used alone — a batch row violates none of them.
4. **Exact-key discovery.** A model present at a source and absent from the table as an
   **exact key**. Never `Lookup`, which prefix-matches and would have hidden `-fast`.
5. **Prefix-collision hard stop.** A discovered identifier that prefix-resolves to an
   existing row is reported as a defect, not a note — `docs/debt.md#46`'s failure, watched
   for automatically now that we know it happens.

A magnitude comparison is still *reported*. It is not a gate.

### Containment

The detector reads third-party bytes and writes none of them to disk as code. Still:

- Fetch timeouts, response-size caps, and a host allowlist — `CLAUDE.md` mandates these at
  the plugin boundary, and the open internet deserves no less scrutiny.
- A stable identifying `User-Agent`.
- The scheduled workflow runs with `contents: read` and no write token. It opens an issue
  through a separate step that receives only the rendered text.
- Any action used is pinned by SHA, not tag — [`docs/debt.md#14`](../debt.md#14) is open,
  and adding a workflow while it is open without pinning is the ledger's own
  "accepted then forgotten" failure.

## Alternatives considered

**Fetch prices at runtime.** Rejected, standing decision (M2 plan, Alternative D). An
endpoint that is down forces a choice between refusing to run and running with no ceiling;
one that is wrong is a spend path with no ceiling at all.

**The code-generating worker.** Rejected for now — see above. Its cost is a supply-chain
path from third-party bytes into compiled Go, and its benefit is saving a hand-edit of
thirteen rows.

**Vendor a third-party pricing library.** Rejected: a dependency deciding how much of the
user's money we may authorize is the last place to take on someone else's maintenance risk.

**Do nothing until the first tagged release.** Genuinely tenable — that is the entry's
trigger, and [`docs/debt.md#13`](../debt.md#13) says the release pipeline does not exist yet.
Rejected because debt 46 proved detection has value *now*: a live mispricing sat in `main`
and only a manual fetch found it.

## Affected packages

`internal/cmd/pricingcheck` (new, `package main`), `adapters/agent/pricing` (test-only —
the detector reads the exported `Lookup`/`Models` surface), `.github/workflows/`, `Makefile`,
`CODEOWNERS`, `docs/debt.md`.

## Proto / schema impact

**None.** The detector writes nothing and changes no wire type.

Phase 1 raised a related gap that is **not** this plan's to fix and is ledgered separately:
nothing compares `pricing_table_version` on resume, so a run interrupted under one table and
resumed under another is capped against a blend of two price regimes. That needs the resolved
`Price` recorded on the `Run` and a `checkResumable` check — additive proto, and it belongs
with M2-10/M2-11 which already write `CaseExecution`.

## Edge cases

| Case | Behavior |
|---|---|
| A source is unreachable | Reported as unreachable; other sources still check. One provider being down must not blind the others |
| A source returns HTML where JSON was expected | Treated as unreachable, **not** as an empty model list — empty would read as "every model was removed" |
| Two sources disagree | Report both values and both URLs. Do not pick a winner |
| A legitimate >50% repricing | Reported, does not fail the job by magnitude alone. Generational cuts of this size are ordinary — Opus 4.1 at $15 to Opus 4.5 at $5 is −67% |
| A model is retired at the source | Reported. Never acted on — a removed row turns priced Cases into refused ones |
| A deliberate omission (`claude-mythos-5`) | Suppressed via a committed exclusion list carrying a reason per entry, so it is not re-reported weekly into an ignored list |
| Discovery that prefix-collides | Hard failure, not a note. This is debt 46 |
| Unit normalization | OpenRouter is USD/token as a decimal *string*; pages are USD/MTok; the schema is micro-USD/MTok. Parsed with `math/big.Rat`, not float — a named test asserts all three representations produce identical micro-USD |

## Test plan

- Fetchers and parsers against **recorded fixtures** in `internal/cmd/pricingcheck/testdata/`
  (not a top-level path — Go tooling expects it under the package). Live fetches are
  `KNO_LIVE_TESTS=1` only, never in PR CI.
- Every check gets a fixture that trips it, each verified to fail before the check is written.
- A deliberately-broken HTML fixture (columns shifted, layout changed) asserts the parser
  **errors** rather than returning a plausible wrong number. This is the case that matters
  most and the one the superseded design got wrong.
- A fixture reproducing the batch table asserts it is refused by the header check — the
  specific bug that defeated the old gate.
- Coverage: the package is new and lands under the 70% repo-wide floor with the no-decrease
  ratchet; `make check` must be green before merge, and a floor entry is added if the package
  proves to warrant one.
- Fixture size: payloads are ~1.9 MB per full capture. Only the parsed-relevant subset is
  committed, not whole pages, and there is no per-run accumulation because nothing is
  captured on a schedule into the repo.

## Rollback

Delete the workflow and the Makefile target. The detector writes nothing, so there is no
state to unwind — which is most of why this scope was chosen.

## Docs impact

- `CONTRIBUTING.md` — how to respond to a drift issue, and that a price edit is a spend-path
  change reviewed accordingly
- `docs/what-the-numbers-mean.md` — cost figures mean "reported usage at these rates", and
  the rates carry a date

## What is deferred, and on what trigger

The generator, the renderer, the policy file, and the PR-opening workflow. Revisit **after
M2-11**, when the adapters have shown which identifiers actually reach `Lookup` in a real
run. Phase 1's argument holds: the discovery design was already wrong in the superseded
draft precisely because no adapter had ever called `Lookup` in production.

## Accepted risks

- **Scrapers break.** Mitigated by failing loudly, and bounded because a broken scraper leaves
  the table exactly where it is today. The header-literal check turns "broke silently" into
  "broke visibly."
- **A compromised source publishes a plausible wrong price.** Cross-source agreement catches
  it unless both are compromised. The detector proposes nothing, so the blast radius is a
  false issue rather than a merged diff.
- **A `--price` override still does not exist.** `EstimateWithPrice`'s godoc calls itself the
  seam for one. Until it exists, a user hitting a wrong or missing price has no local remedy
  and must wait for a release. That is a real gap this plan does not close, and it is the
  strongest argument for building the override before the generator.

# v0.1 release checklist

Not a plan — the tag-day gate. Everything here is either a ledger obligation with a
"before 0.1.0" trigger or a release-mechanics decision. Owned by the release PR, not by
any feature PR.

## Ledger obligations due at the v0.1 tag

| # | Obligation | Disposition before tagging |
|---|---|---|
| [19](../debt.md#19) | release-please `feat:` bump decision | Revisit bump-minor-pre-major at v0.1: decide whether `feat:` bumps minor from here, and write the decision into the release-please config comment + CHANGELOG policy paragraph |
| [76](../debt.md#76) | CHANGELOG fold | REPAID 2026-08-27 (fold-changelog.sh) — verify the fold runs on the v0.1 release PR |
| [75](../debt.md#75) | `kno value` seed promise | Select's plan dispositions this — confirm before tagging that either the shuffle is inlined or the wording is scoped |
| [73](../debt.md#73) | Homebrew tap | v0.1 is at-or-after the third tag; verify the tap updated automatically |
| [12](../debt.md#12) / [14](../debt.md#14) / [15](../debt.md#15) / [16](../debt.md#16) | tools vuln scan, SHA pins, DoD checks, gate selftest | These are "before 1.0" — NOT v0.1; leave open, note in the release review |

## The release review itself

- Every ledger entry's trigger is checked at the minor release: repaid, re-dated with a
  written reason, or promoted to won't-fix with an ADR. CI's lapse check is not built
  (entry 15), so this is a hand review — run it and record it in the release PR.
- #40/#46's clocks: the pricing table is fresh as of 2026-08-28; the detector enforces
  the 90-day leg from here.

## Release mechanics

- CHANGELOG: hand-written `## [Unreleased]` folds to `## 0.1.0 — in detail` via the
  existing script; release notes hand-edited for the top 3 highlights (top 3 for v0.1:
  `select`, `export`, `mine` — the story is "the loop is complete", not "five stages").
- vhs tape: re-record the README quickstart after `init`/`select`/`export` change the CLI
  output (the DoD clause, now with content).
- `make release-check`, `make release-stamp`, and the full `make check` on the tag commit.
- Post-tag: the pricing-check push trigger runs on the release merge — expect the clean
  run to close any open pricing-drift issues; confirm none are left dangling.
- README Status table flips Select/Export to **Shipped** in the same PR that ships them —
  the status-contradiction lesson from the README rewrite is a standing rule now.

## What deliberately does NOT block v0.1

- Ring-2 plugins, `serve`, SDKs, OTel export (v0.3, per DESIGN).
- The TUI dashboard beyond the consent prompt (deferred in the init plan).
- Parquet pools (re-dated in the adapters plan).
- Knowledge-injection mode, Tuner/Bridge, `validate` (v0.2).

## The v0.1 ledger review (recorded 2026-08-29)

Ledger reviewed in full for the v0.1 tag. Four lapses were disposed in this review, each with
the lapse admitted and a re-dated trigger: #3 (benchmarks — re-dated to the first
performance-claiming PR or 1.0), #43 (transport sentinel — re-dated to the next
request/response-handling transport PR), #68 (tokenizer bias — re-dated to the second
content-type pool or 1.0), #70 (jsonl probe — re-dated to the next jsonl PR, the port is the
whole repayment). #33 gained a date leg (inbound-report trigger, per #46's precedent), #37's
export half re-dated to v0.3 per DESIGN, #44's prompt half and #59 repaid with the init/consent
PR (#102), #58 discharged as bookkeeping (already done by #69), #83 re-dated to the first Tuner
PR. Before-1.0 entries (#12/#14/#15/#16, #6, #30, #32, #71, #72) stay open with written
triggers, noted here as the release review requires. Known gap, recorded honestly: the CI lapse
check (#15) is not built — this review is the hand-run substitute.

## Disposition (2026-08-29)

Released as **0.1.0**. The bump is a deliberate choice, not a default: the breaking marker on
the release commit is what moves the version off the patch track under the #19 policy. The
break it names is real — checkpointed Value runs recorded before this release cannot be
resumed under it (the seed stream changed, CHANGELOG migration notes) — and the version bump
is the covenant semver asks for in return. All checklist obligations disposed: ledger reviewed
(#114), #19 recorded (#113), #73 verified (tap at 0.0.4; brews block re-enabled #112), tape
re-recorded (#116), cookbook swept against the final CLI (#117). Post-tag watches: the
release workflow's formula push to Knograph/homebrew-tap and the pricingcheck clean run.

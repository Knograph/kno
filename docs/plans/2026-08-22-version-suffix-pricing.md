# Which model identifiers inherit a base row's price

Interface change to `adapters/agent/pricing.Lookup`, on a spend path. Written because
`CLAUDE.md` Phase 0 requires a plan for any interface touch, and the Phase-3 review of PR #35
correctly found this one shipping without it.

## Problem

`Lookup` does exact match, then longest-prefix fallback. The fallback exists so a pinned
identifier resolves: `claude-sonnet-4-5-20250929` is the canonical API ID and
`claude-sonnet-4-5` is the alias, and pricing only the alias refuses every user who pins.

It could not distinguish that from a suffix that **changes** the price.
`anthropic/claude-opus-5-fast` is on the provider's live model list; it resolved to
`claude-opus-5` and priced at base. Measured on `main`:

```
claude-opus-5-fast    -> $5/MTok in, $25/MTok out
claude-sonnet-5-fast  -> $2/MTok in, $10/MTok out
gpt-5.6-sol-pro       -> $4/MTok in, $20/MTok out
```

Anthropic prices fast mode well above base, so a capped run reserved at a fraction of the
true rate. Prime directive 4: the cap the user set was not the cap they got.

## The asymmetry that decides the design

| | Visible to the user? | Under `--max-cost-usd` |
|---|---|---|
| Model is unpriced | Yes — the run refuses | Run does not start |
| Model priced at another model's rate | **No** | Run proceeds, reserves too little |

`Lookup`'s own godoc already said which of these is correct: *"the caller must not substitute
a zero: with a cost cap set, an unpriceable Case is refused rather than authorized against a
guess."* The prefix fallback was doing exactly what that sentence forbids. So the rule must
**fail closed** — refuse what it cannot confirm.

## Design

**Versions are numbers. Variants are words.**

That is the distinction providers actually draw, and it is orthographic rather than semantic,
which is why it can be checked. Every published pin is digits and separators; every suffix
that carries its own price is a word. `latest` is the single word that is a version, because
providers document it as an alias to the current snapshot at the same rate.

A suffix inherits the base row when it starts with `-` or `@`, contains at least one digit,
and every segment (split on `-`, `.`, `:`, `@`) is either all digits or a `v`-prefixed
revision tag.

| Accepted | Refused |
|---|---|
| `-20250805` (Anthropic) | `-fast` |
| `-2026-03-01` (OpenAI) | `-pro` |
| `-0613` (OpenAI legacy MMDD) | `-preview` |
| `-1-20250805` (point release) | `-20260514-fast` |
| `@20250929` (Vertex) | `-v` (no digit) |
| `-20250929-v1:0` (Bedrock) | `-.-.-` (no digit) |
| `-latest` | `520260514` (no separator) |

## Alternatives considered

**Count the digits (the first implementation, rejected on review).** Required exactly eight.
It refused `claude-opus-4-1-20250805` — a shipped identifier whose point-release segment makes
nine — while accepting `-2026-13-45`, which is not a date. The table already carries
`claude-opus-4-5` and `claude-opus-4-8`, so point releases are not hypothetical here. The rule
described neither versions nor dates; it was a coincidence of two providers' current
conventions, on a spend path.

**Parse and validate a real date.** Stricter, and still wrong: it refuses the point-release
form, `-latest`, and the Vertex/Bedrock shapes. Validating month and day ranges buys
protection against `-2026-13-45`, which is not a model anyone can call — the provider 404s it.
The cost is refusing identifiers that exist.

**Per-scheme rules.** Suggested on review. Rejected as more surface for the same answer: all
six accepted shapes are numeric, and a per-scheme table would need updating for every new
provider while the numeric rule already covers them.

**Denylist the known variant words (`-fast`, `-pro`, …).** Rejected: fails **open**. The next
variant nobody enumerated prices at base, which is the bug being fixed.

**Drop the fallback entirely; require exact rows.** The likely end state once the pricing
detector ([`docs/debt.md#40`](../debt.md#40)) can enumerate identifiers. Rejected now because
it refuses every pinned version until the table is regenerated, which is a larger regression
than the one being fixed.

## Affected packages

`adapters/agent/pricing` only. No proto change, no schema change, no migration.

## Edge cases

| Case | Behavior |
|---|---|
| No cost cap set | Unchanged. `estimate()` falls back to `--est-cost-per-call`; refusing an uncapped run because a price is unknown would be worse |
| Suffix with no digits | Refused — a bare `-`, `-v`, or a run of separators |
| Suffix not separated from the base | Refused: `claude-opus-520260514` does not start with `-` or `@` |
| A variant under a cost cap | Every Case errors and the run ends as "too many cases errored", naming nothing about pricing. Known and ledgered — see Accepted risks |
| Tokenizer selection | `usesNewTokenizer` keeps an unrestricted prefix match on purpose: a denser tokenizer over-counts, which over-reserves. Over-reserving is recoverable; a wrong rate has no safe direction |

## Test plan

`TestAVariantSuffixIsUnpricedRatherThanGuessed` — every row above, each verified failing
against three mutations:

| Mutation | Rows that fail |
|---|---|
| Accept any suffix (the shipped behavior) | 8 |
| Require exactly 8 digits (the rejected first design) | 4, including the point-release and Bedrock forms |
| Drop the at-least-one-digit requirement | 1 (`-.-.-`) |

Each accepted row additionally asserts the resolved price equals its base row on **all four**
rates, against a hand-written `baseOf` map rather than a rule derived from the model string —
so the test cannot agree with the code by reusing the code's own logic. An earlier version
split on a hardcoded `"-2026"`, which for `claude-sonnet-4-5-20250929` returned the whole
string and compared a value to itself.

## Rollback

Revert. No persisted state, no schema.

## Docs impact

- `docs/debt.md#46` (new): variants are unpriced until someone adds rows
- `docs/debt.md#40`: its claim that "a dated identifier resolves by longest prefix" needed
  correcting in the same PR that falsified it
- CHANGELOG, including the explicit statement that uncapped runs are unaffected

## Accepted risks

- **A user on a variant with a cost cap cannot run at all** until rows are added. Rows need
  the published rates read from the provider's page by a human — a rate reported in a review
  summary is not a source to price against. [`docs/debt.md#46`](../debt.md#46).
- **The refusal is per-Case, not per-run.** A capped run on an unpriced model quotes a figure
  from the run-scoped scalar, takes the user's consent, then errors every Case and reports
  "too many cases errored" — which names nothing about pricing. A pre-flight refusal was
  written and reverted: it aborts the whole run for a **single** unpriceable Case, overturning
  `TestEstimatorFailureRefusesWhenACostCapIsSet`'s deliberate decision that the priced Cases
  should still run. Separating "this model has no price" from "this Case cannot be priced"
  needs a model-level sentinel an adapter sets and `core` reads, and adding it before an
  adapter merges is dead code. Lands with **M2-11**; ledgered on #46.
- **`-2026-13-45` and `-99999999` still price as base.** Not dates, but not callable models
  either — the provider rejects them. Validating dates would cost the point-release form,
  which is real.

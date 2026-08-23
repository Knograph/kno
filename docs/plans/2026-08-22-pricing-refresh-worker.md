# Pricing refresh worker

Repays [`docs/debt.md#40`](../debt.md#40). Requested directly: *"We should probably store
that in a worker to update pricing as models get updated."*

## Problem

`adapters/agent/pricing.Version` is `"2026-08-21"` and the table is hand-entered. Providers
change prices and ship models continuously. A stale table has two failure modes, and they
are not symmetric:

- **A stale price** produces a wrong estimate. Bounded and self-correcting — settlement
  reconciles against the provider's reported usage, so the *spend* is right even when the
  *estimate* was not.
- **A missing model** produces an unpriceable Case. Under a cost cap that is a refusal:
  the run does not start. A user on a model we have never heard of cannot use a cap at all.

The second is the one that actually breaks people, and it gets worse every week we do not
ship this.

## Non-goal, stated first because it is the whole design constraint

**Nothing fetches a price at runtime. Ever.** The M2 plan rejected it (Alternative D) and
that stands: an endpoint that is down leaves the engine choosing between refusing to run
and running with no ceiling, and an endpoint that is *wrong* is a spend path with no
ceiling at all. Prime directive 4 is not negotiable against convenience.

The worker's output is **a pull request**. A price change is reviewed like code.

## Design

`internal/cmd/pricingsync` — internal, so it is not public Go API and cannot be reached
from the shipped binary. A GitHub Actions workflow runs it weekly and on demand.

```
pricingsync
  ├── fetch    per-source, each behind an interface
  ├── diff     against the current table, classified
  ├── gate     refuse to open a PR when the diff looks wrong
  └── render   regenerate table.go + bump Version
```

### Sources

| Source | Mechanism | Confidence |
|---|---|---|
| OpenRouter | `GET /api/v1/models`, `.data[].pricing.{prompt,completion}`, USD per token | Machine-readable, versioned, stable |
| Anthropic | HTML page | Scraped, brittle |
| OpenAI | HTML page | Scraped, brittle |

OpenRouter is the only structured feed. It is also **not authoritative for the other two**:
it resells, and its prices carry a margin. So OpenRouter is used for *discovery* — "a model
exists that we have no row for" — and never as a price for the `anthropic` or `openai`
schemes.

For the two HTML sources, the worker **proposes** and a human confirms. A scraper that
silently mis-parses a table cell and writes `$0.02` where the page says `$20` must not be
able to land that.

### The gate

The job fails loudly rather than opening a PR when:

- any existing price moves by more than 50% in either direction
- a model **disappears** from a source (removal is never automatic — losing a row turns
  priced Cases into refused ones)
- a parsed price is zero, negative, or absent where one existed before
- the number of models parsed from an HTML source drops by more than a third (the signature
  of a layout change that broke the selector)

A failing gate is a notification that a human needs to look, not a silent no-op.

### What lands in the PR

- The regenerated `table.go`, formatted identically to the hand-written one, so the diff is
  readable line by line
- `Version` bumped to the run date
- A PR body table: model, old price, new price, percent change, source URL
- The raw fetched payloads under `testdata/pricing/<date>/`, so the diff is auditable
  against what the source actually said

## Alternatives considered

**Fetch at runtime with a cache.** Rejected — the standing decision above. Also makes every
run depend on a third party's uptime for a number it only needs for an estimate.

**Vendor a third-party pricing library.** Rejected: a dependency that decides how much of
the user's money we are allowed to authorize is the last place to accept someone else's
maintenance risk. `CLAUDE.md` requires justification for new dependencies; "it saves us a
scraper" does not clear that bar for a spend path.

**Auto-merge when the gate passes.** Rejected. The gate catches malformed data, not wrong
data — a source that publishes a genuinely incorrect price passes every check. A human
reading "Opus 5 input: $5.00 → $0.50" catches it; a bot does not. The review is the control.

**Scrape only, no OpenRouter.** Rejected: OpenRouter is the only source that tells us a
model *exists*, which is the failure mode that actually breaks users.

**Ship prices as a data file instead of generated Go.** Deferred, not rejected — see
Accepted risks. Generating Go keeps `Lookup` allocation-free and the table compile-time
checked, and avoids shipping a file users can edit into a spend path.

## Edge cases

| Case | Behavior |
|---|---|
| A source is unreachable | That source is skipped, the job reports it, other sources still produce a PR. One provider being down must not block the others |
| A source returns HTML where JSON was expected | Treated as unreachable, not as an empty model list — an empty list would read as "every model was removed" |
| Model exists in OpenRouter, unknown to us | Reported as a **discovery**, not a price. The PR notes it needs a row and a human supplies the rate from the provider's own page |
| A price gains a dimension (e.g. OpenAI starts billing cache writes) | New field set, flagged in the PR body as a shape change rather than a rate change |
| Two runs the same day | `Version` is the date; the second PR supersedes the first. Branch name is fixed per date so it force-updates rather than opening duplicates |
| Provider renames a model | Looks like one removal and one discovery. Removal is gated, so it stops for a human |

## Test plan

- Fetchers tested against **recorded fixtures** in `testdata/pricing/`, per `CLAUDE.md`'s
  determinism rule. Live fetches are `KNO_LIVE_TESTS=1` only and never in PR CI.
- Every gate condition gets a fixture that trips it, and each is verified to **fail** before
  the gate is written.
- A deliberately-malformed HTML fixture (layout changed, cells shifted) asserts the parser
  reports an error rather than a plausible wrong number. This is the case that matters most.
- Golden test on the rendered `table.go`, so a formatting change is reviewed.
- Round-trip: render the current table from its own parsed form and assert byte-identity
  with the committed file. That is what proves the generator cannot quietly reformat
  everything and bury a real change in the noise.

## Rollback

Delete the workflow. The table is committed Go and keeps working untouched — the worker only
ever proposes.

## Docs impact

- `docs/cookbook/` entry on overriding a price and on what `pricing_table_version` means
- `docs/what-the-numbers-mean.md` — cost figures are "reported usage at these rates", and
  the rates have a date
- `CONTRIBUTING.md` — how to review a pricing PR, since it is a spend path and the review is
  the only control

## Accepted risks

- **Scrapers break.** Mitigated by the count-drop gate and by failing loudly, not by
  pretending otherwise. A broken scraper is a stale table, which is where we already are.
- **A source could be compromised and publish a plausible wrong price.** The gate does not
  catch this; human review is the control, which is why auto-merge is rejected. Scope is
  bounded: a wrong price affects estimates, and settlement still reconciles against reported
  usage.
- **Generated Go rather than a data file.** Revisit if a user needs to supply prices for a
  self-hosted model without rebuilding — which is a real request waiting to happen.

# Read the whole story with `kno report`

What you're asking: **what did the stages conclude, in one place?** `kno report` composes the recorded Baseline, Value, Select, and Export runs into a single page. It reads only recorded aggregates — no LLM calls, no evals re-read, no trace content — so it works on any machine holding the same `--db`, and it never spends anything.

## Before you start

You need the run IDs from the report lines of the stages you ran:

- **`kno baseline`** prints `Baseline <id>`. This one is required — the report composes around the Value run, which pairs against a Baseline.
- **`kno value`** prints `Value run <id> (...)`. This one is required.
- **`kno select`** prints `Select run <id> (...)`. Optional.
- **`kno export`** prints `Export run <id> (...)`. Optional.

```sh
kno report --value-run-id <id> \
  --select-run-id <id> \
  --export-run-id <id>
```

## Reading the page

| Section | What it tells you |
|---|---|
| **Baseline** | The reference the deltas mean anything against: its score and how many Cases were scored (and errored) |
| **Asset verdicts** | Each Asset's delta with its 95% CI. Deltas are in the Goal's own units — positive is toward the Goal. The Corrected column shows the Select run's routing-fraction correction when it recorded one |
| **Portfolio** | What Select chose and rejected, and why (the rejection log, folded by reason). The dev-estimated gain is a selection-time estimate and says so |
| **Gaps** | The failure clusters nothing in the pool improved, with a verdict per cluster: **improved** (a covering Asset's interval excludes zero), **gap** (well-covered, nothing significant), or **unknown** (nothing routed to enough Cases, or the covering measurement is underpowered) |

## The caveat you will see

The Portfolio section carries this line whenever it shows a dev estimate:

> **not yet validated on holdout**

It is mandatory, not decoration. The holdout is the only clean test of a
selection, and `kno validate` does not exist in this release — so the
estimate is a selection-time number, winner's-curse inflation included. When
the page shows it, treat it as a headline with its disclaimer attached.

## What "no cluster data for this run" means

The gaps section says exactly this when the Export run recorded no gaps
record. It is the honest absent-answer, never a guess: the source Value run
predates the cluster snapshot, or the run had no failure clusters to route
on. Either way, there is nothing to render, and the page says so instead of
inventing a table.

## Refusals are also honest

A Baseline that Value's own rules would refuse — it ended on its error rate,
or blended two models into one reference — is refused here too, with the
fix, before a page can compose around it. If you see that, it is the same
refusal `kno value` would have given you, read back later.

## Watching a live run

`kno report --value-run-id <id> --watch` re-renders the page every 2
seconds while the Value run is not terminal, and exits 0 the moment it is —
the exit code is the "is it done" answer for a wrapper script. It needs a
terminal, and it cannot combine with `--json`; for machine consumers, the
one-shot page has `--json`:

```sh
kno report --value-run-id <id> --json | jq '.portfolio.dev_estimated_gain'
```

The `--json` contract is the same snapshot the human page renders — two
renderers, one composed document.

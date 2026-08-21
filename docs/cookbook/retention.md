# Delete stored conversation content

Goal: keep the numbers, drop the trace content.

## What Kno stores, plainly

Every `kno baseline` run writes a SQLite database (`kno.db` by default) containing, per Case:

| Stored | Is it conversation content? |
|---|---|
| The agent's **output** | **Yes.** Whatever your agent said, verbatim |
| The judge's **rationale**, when a Goal uses one | **Yes.** It quotes and reasons about the output |
| The Case ID, whether it scored, the score, what it cost | No |
| Which Cases completed | No |
| One event per Case on the run's event stream | No — the schema forbids it, and a test enforces that structurally |

If your evals are built from production logs, the first two rows are end-user data sitting on whatever machine ran the command. **Kno itself** sends nothing anywhere: there is no telemetry of content, ever. Your Cases do go to whatever provider you point Kno at — that is the product — and that provider's retention is between you and them. Locally, Kno keeps the results, because a measurement you cannot audit is a measurement you have to take on faith.

**Nothing expires on its own.** There is no retention timer. Deleting is something you do.

## Delete it

```bash
kno purge --run-id 20260821T091515-ffc3097d49da
```

It tells you how many outcomes it would touch and stops, **exiting non-zero** so a scheduled job that forgot `--yes` fails loudly instead of reporting success over data it never removed. Add `--yes` to go through with it:

```bash
kno purge --run-id 20260821T091515-ffc3097d49da --yes
```

```
Purged 44 outcome(s) for run 20260821T091515-ffc3097d49da.
The run is still resumable: completion records, costs, and scores were kept.
```

This cannot be undone. Purge zeroes the freed pages and rewrites the database file, so the content is gone from the bytes on disk and not merely unlinked from a column — `strings kno.db` finds nothing. That rewrite is why purging a large database takes a moment.

## What survives, and why that is not a loophole

Purge removes the agent's output and the judge's rationale. It keeps the Case ID, the score, the cost, and the fact that the Case completed.

That is not Kno hedging about deletion — it is the difference between "what was said" and "what it measured." A score of `0.82` is not conversation content, and neither is `$0.003`.

**One thing that will look wrong.** Resume a purged run and the report says `score none`:

```
Baseline demo
  cases      44 scored, 0 errored (of 44 dev; 6 held back)
  score      none
  status     completed
```

The scores are still there — verified in the database, all 44 of them. What you are seeing is a separate, known limitation: a resumed run's reported aggregate covers only the Cases *that process* scored, and a run that was already finished scores none. It has nothing to do with purge and happens on any completed run you resume. It is [`docs/debt.md#27`](../debt.md#27), and it is fixed when the aggregate starts reading the stored scores instead of in-memory counters.

We would rather write that down than let you conclude purge ate your numbers.

Keeping them is also load-bearing. **Kno has no separate "this Case is done" marker: the recorded outcome _is_ the marker.** Delete those rows and a resumed run has no way to know the work happened, so it runs every Case again and pays for every Case again. A purge that reopened the double-spend hole would be a privacy feature that costs you money.

So `kno purge` nulls the content columns and never deletes a row. If you want the rows gone too, delete the database file — that is a real and supported answer, and it makes the run unresumable, which is the honest trade.

## In a scheduled job

```bash
# Yesterday's nightly baseline: keep the score, drop the trace content
kno purge --run-id "nightly-$(date -I -d yesterday)" --yes
```

Purging a run that does not exist is an error rather than a silent success, so a typo in the run ID fails the job instead of quietly keeping data you meant to remove. Omitting `--yes` fails the same way, for the same reason.

**A run that is still executing is refused.** Cases finishing after the purge would write fresh output, and the command would have reported success over content that reappeared seconds later. Wait for it to end, or pass `--force` if you know it is not running.

Purge is per-run today. Bulk retention across every run older than *N* days is not built yet.

## What purge does not cover

- **Your eval file.** Kno reads `--evals` and never modifies it. If it contains user data, that file is yours to manage.
- **Exported artifacts.** Anything you wrote to disk from a report.
- **Provider-side logs.** Your LLM provider's retention is between you and them; check their policy.
- **Backups of `kno.db`.** Purge touches the database you point it at, nothing else.

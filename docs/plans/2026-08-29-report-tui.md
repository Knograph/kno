# report: the one-page verdict, and the minimal TUI dashboard

The v0.1 bar lists `report` (glow) and a TUI dashboard. The consent prompt already landed the
TUI's prime-directive half in the init plan; this plan scopes the REST deliberately small.

**Phase-1 re-reviewed 2026-08-29 — verdict: reject as written; amended.** The first draft's
centerpiece — the gaps artifact — was unbuildable from durable data: Case tags are never
persisted, "no Asset moved the needle" was statistically undefined, and DESIGN assigns gaps to
**Export**, not to a report reader. The amendments below pin a persistence path (cluster data
written into the existing value-plan blob at planning time — no schema change, no evals
re-read, no holdout exposure), define the statistic, move the gaps RECORD to Export with
report as its renderer, and name the dependency. Findings 1–8 folded and tagged.

## Problem

`kno baseline` and `kno value` print their own reports; `kno select`/`kno export` (landing
with them) will too. What does not exist: (a) a cross-stage **report** — the one-page answer
over the recorded runs, (b) the **gaps** artifact DESIGN's Export names as "the tool's most
forward-looking output", and (c) any TUI beyond the consent prompt.

## Design

### Step 0 — make the gaps artifact computable (lands in the Select/Export PR, not this one)

- **Cluster data persists at value-plan time** *(finding 1)*: `value.Plan` already exists and
  is gob-serialized into `run.value_plan` on every close path; it already carries routed and
  control Case IDs. It gains `Clusters[]{Tag string; CaseIDs []string; NDropped int}` —
  the failure clusters `route.go`'s `cluster()` already computes, snapshotted at planning
  time, dev-only by construction (routing never sees the holdout; the seal guards the path).
  Gob is append-tolerant, so OLD blobs decode with an empty Clusters and the report says
  "no cluster data for this run" rather than guessing. No store schema change, no evals-file
  re-read, no raw holdout Cases in any process outside the seal *(finding 7)*.
- **The gaps statistic, defined** *(finding 2)*: a cluster is **improved** when at least one
  Asset routed to ≥ `min_cluster_cases` (default 5) of its Cases has a delta whose 95% CI
  excludes zero. **Unknown** when nothing routed reaches the minimum or the covering
  measurement is underpowered — non-significance is not absence, and the output is a spend
  recommendation. **Gap** = well-covered and no significant improvement. The reported number
  per cluster is the BEST covering Asset's CI, never a cluster-level threshold game; multiple
  testing is labeled ("this list is a discovery aid, not a test — as many as k of these can
  be noise under screening"), the same honesty the #66 correction exists for, in reverse.
- **Ownership resolved per DESIGN** *(finding 3)*: Export computes and PERSISTS the gaps
  record (a `portfolios`-table-adjacent row or a proto blob beside the portfolio — the
  Select/Export plan owns the exact shape, and this plan defers to it); `kno report` RENDERS
  it. The Select/Export plan's store contract (WritePortfolio/Portfolio, rejection log,
  correction metadata, gaps record, and a `ListRuns`-class reader for the fix line) is the
  dependency this plan names rather than denies.

### `kno report` (glow-rendered, `--json` behind it)

```
kno report --value-run-id <id> [--select-run-id <id>] [--export-run-id <id>] [--watch]
```

- Composes the recorded stages into one page: baseline score + status, per-Asset verdict
  table (delta, interval, corrected-for-screening flag **when the Select run exists and
  recorded its correction metadata** — the store contract above), the portfolio's dev
  estimate with its interval AND the mandatory caveat line "not yet validated on holdout"
  (validate is not in the v0.1 bar, and a headline number must not pretend otherwise —
  *finding 8*), the rejection log summarized by reason, and the gaps section rendered from
  Export's persisted record.
- **A dirty reference is refused, not rendered through**: a baseline whose
  `error_rate_exceeded` or model-gate state fails Value's own fingerprint rules is reported
  as an unusable reference with the fix line, not composed into a page.
- **Dependency, named** *(finding 6)*: `charmbracelet/glow` — DESIGN:397 commits the report
  to glow, and dropping it is a DESIGN amendment, not a shortcut. It renders markdown
  **non-interactively** (no raw mode — the 2s redraw loop is a plain ticker, consistent with
  the init plan's reasoning). Module weight: the bubbletea-family tree the init plan already
  priced for huh (~20 modules, MIT, charmbracelet org). `--json` emits the same content as a
  hand-written struct in `cli/jsonreport.go` (ADR-0001's convention), pinned by golden
  equivalence tests — two renderers, one pinned content.
- **Unknown run id** *(finding 4)*: the fix line names where run ids come from
  (`kno baseline`/`kno value` print them) until the Select/Export PR's ListRuns lands; the
  plan does not claim a run-list command that does not exist.
- **Trace content never renders**: the page reads measurement/outcome aggregates and
  headers, never the response blobs (one line, per the log-content rules).
- **`--watch`, pinned** *(finding 5)*: re-renders every 2s while the watched run is not
  terminal; **exits 0 when the run reaches a terminal status** (the primary case the first
  draft missed); each render is a best-effort per-query snapshot and the final render is
  authoritative (WAL: rows are never torn, only compositions are); `--watch` + `--json` is
  REFUSED (an unframed stream of JSON documents is worse than none); non-TTY `--watch`
  refuses with exit 2, the consent prompt's precedent.

## Alternatives considered

**A full live-event TUI now.** Rejected: the event schema is still moving (Select/Export
events are landing), and building a dashboard over a moving schema is the trap debt #42
records. Store-polling is schema-stable.

**Computing gaps in report from the rejection log.** Rejected in review: rejection reasons
are PORTFOLIO reasons — a `cost_dominated` Asset did improve its cluster, and reading
rejections as non-improvement marks improved clusters as gaps.

**Re-reading the evals file at report time for tags.** Rejected: pulls raw holdout Cases
into a process outside the seal, and the path is not recorded. The plan-time snapshot
closes both.

## Affected packages

`cli/` (`kno report`, `--watch`), `docs/` (cookbook entry, README status line), CHANGELOG.
The store/schema halves (clusters in the value-plan gob, Select/Export persistence
contract) land in the Select/Export plan's PRs; this PR is the reader.

## Proto / schema impact

None in THIS PR. The gob `value.Plan` addition and the Select/Export store contract are the
Select/Export plan's, named here as the dependency.

## Edge cases

| Case | Behavior |
|---|---|
| --value-run-id missing or unknown | Refused; the fix line names where run ids come from (ListRuns arrives with Select/Export) |
| A stage in the chain is BUDGET_STOPPED | Rendered with status and incomplete_reason, like Select's source-run rule |
| No Select run yet | Raw (uncorrected) intervals with a one-line note |
| A dirty-reference baseline | Refused as unusable with the fix line, not composed |
| Gaps record absent (run predates the clusters field) | "no cluster data for this run" — never guessed |
| Gaps list empty because every cluster is unknown/underpowered | Says "unknown", not "nothing to collect" — the two are different claims |
| --watch on a finished run / finishing while watched | Renders once, exits 0 |
| --watch + --json, or --watch non-TTY | Refused (exit 2, fix line) |
| The holdout | Cluster data is dev-only at plan time; a canary test pins that no holdout Case id appears in the report's inputs |

## Test plan

- Golden files for the composed page across stage combinations (baseline+value, +select,
  +export, budget-stopped source, dirty-reference refusal).
- Gaps rendering: synthetic clusters with known coverage → improved/unknown/gap each pinned;
  min-cluster-size boundary; multi-tag Case double-count ruled out (each Case belongs to its
  clusters exactly once per the snapshot).
- The holdout canary: a holdout Case id planted in the source never reaches the report's
  cluster data.
- `--json`/human equivalence goldens; `--watch` exit-on-terminal, snapshot semantics,
  non-TTY and `--json` refusals; exit codes.

## Rollback

Delete the command. Nothing persists.

## Docs impact

Cookbook entry ("Read the whole story with kno report"), README status line, CHANGELOG.

## Accepted risks

- **The gaps list is a discovery aid, not a test.** Multiple-cluster labeling carries the
  screening noise in reverse; the page says so, and the best-covering-CI form makes the
  evidence visible per row.
- **--watch polls the store.** Fine at v0.1 scale; the event-stream dashboard is the v0.2
  shape.
- **Old runs carry no cluster data.** The snapshot is append-only knowledge; pre-field runs
  report its absence honestly.

# Run Kno inside n8n: scheduled valuation with alerts

What you're asking: **make Kno's verdict part of an automation pipeline** — re-value the knowledge base on a schedule, alert when an asset turns harmful, and keep humans out of the loop for everything except the decision.

n8n doesn't hold eval data; it runs workflows. Kno is a CLI, so the integration is an Execute Command node plus the exit codes — the same contract the [CI recipe](ci-gate.md) uses.

## The workflow shape

```
Schedule trigger (weekly)
      │
      ▼
Execute Command ── kno value --evals cases.jsonl --pool pool.jsonl
                    --baseline-run-id <id> --json
      │
      ▼
IF node ── exit code
   ├── 0 ──► Code node (parse --json, build the verdict table)
   │            │
   │            ▼
   │         Slack node ── post the table to #agent-ops
   │
   ├── 2 ──► no-op (budget stop; resumable, not a failure)
   └── 1 ──► Slack node ── page: the run FAILED
```

## 1. The Execute Command node

One command, environment credentials, nothing on the command line:

```
kno value --evals cases.jsonl --pool pool.jsonl \
  --baseline-run-id 2026-08-21-baseline \
  --agent openai:gpt-4.1 --max-cost-usd 10.00 --json
```

n8n's Execute Command node runs a shell; give it `OPENAI_API_KEY` from an n8n credential field, mapped into the node's environment — keys come from the environment, never from a flag, and never into the n8n workflow JSON, which is what gets exported and shared.

The `--json` flag makes the report machine-readable; the Code node parses it instead of scraping the human output.

## 2. Branch on the exit code

The same table as [CI](ci-gate.md), but n8n's `IF` node reads it as the command's exit status:

| Exit | Meaning | Workflow branch |
|---|---|---|
| `0` | Completed | Parse the report, post the table |
| `2` | Stopped at a budget cap | Not a failure — nothing to post, or a quiet note |
| `1` | Failed | Something is broken — page the channel |
| `4` | Interrupted | Resumable — leave a note, don't page |

`2` and `4` both leave a resumable run. Reporting either as `1` would train the channel to ignore real pages.

## 3. The Code node: verdicts, not raw JSON

Parse the report and select what deserves a human's attention:

```js
const rep = $input.item.json;           // the --json report from the previous node
const table = rep.valuations
  .filter(v => v.low !== undefined)      // skip not_measured rows ("routed to nothing" etc.)
  .map(v => {
    const d = v.delta_goal, lo = v.low, hi = v.high;
    if (hi < 0) return `🔴 ${v.asset_id}  Δ${d.toFixed(3)} [${lo.toFixed(3)}, ${hi.toFixed(3)}] — HARMFUL, retire`;
    if (lo > 0) return `🟢 ${v.asset_id}  Δ${d.toFixed(3)} [${lo.toFixed(3)}, ${hi.toFixed(3)}] — earns its place`;
    return null;                         // interval crosses zero: no evidence either way
  })
  .filter(Boolean);
return [{ json: { text: table.join("\n") || "No asset moved the needle this week." } }];
```

The report carries one row per Asset: `asset_id`, flat `delta_goal`/`low`/`high` (present only when an interval exists), and `not_measured` with the reason otherwise.

Field names are the `kno value --json` report's own — pin them against one real run before wiring the node, and re-check after a Kno upgrade: the JSON shape is documented per release in the CHANGELOG.

## 4. The Slack node: only what a human should act on

The Code node's output is already filtered — harmful assets and clear winners only. The "interval crosses zero" rows are deliberately dropped: a weekly post with twenty "no evidence" lines trains the channel to scroll past, and the one harmful asset in week nine gets missed because of it.

## Why this is the right integration boundary

n8n orchestrates; Kno measures; the exit codes are the contract. No plugin, no SDK, nothing to maintain — the workflow is a shell command plus a parse, and every Kno upgrade that matters announces itself in the CHANGELOG. When `select` and `validate` land, the same workflow grows two more nodes: run `kno select` before the alert, and gate the deploy on `kno validate`'s exit code.

*The measurement side — building the Cases and the Pool in the first place: [Value your Zendesk knowledge](zendesk.md), or any other vendor recipe.*

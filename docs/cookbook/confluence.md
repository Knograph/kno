# Value your Confluence knowledge: pages for an engineering or support agent

What you're asking: **which of our written pages actually make the agent answer better?** Confluence is the pool in the classic pairing — pages are the candidate content, and the evals come from where the questions happened (Jira issues, support tickets, or Slack).

## What maps where

| Confluence thing | Kno thing | How |
|---|---|---|
| Page (spec, ADR, runbook, how-to) | Asset | `content` = page body rendered to text, `kind: "knowledge"` |
| Page tree / space | Pool | One Asset per page; spaces with thousands of pages want curation first |
| Jira issue or support ticket | Case | From the [Jira recipe](jira.md) or your support export |

## 1. Export the pages

```bash
export CONFLUENCE_HOST=acme.atlassian.net
export CONFLUENCE_EMAIL=you@acme.com
export CONFLUENCE_API_TOKEN=...
export SPACE_KEY=ENG

curl -s "https://$CONFLUENCE_HOST/wiki/rest/api/content?spaceKey=$SPACE_KEY&type=page&limit=100&expand=body.storage" \
  -u "$CONFLUENCE_EMAIL:$CONFLUENCE_API_TOKEN" \
  | jq -c '.results[] | {id: ("page-" + (.id|tostring)), content: .body.storage.value, title: .title, kind: "knowledge"}' \
  > pool.jsonl
```

`body.storage` is storage-format markup — render it to plain text before committing. Pages in a wiki reference each other; the agent reads text, so strip macros and links that only make sense in a browser.

## 2. Build the Cases

From Jira (the natural pairing):

```jsonl
{"id":"PROJ-4821","input":"Deploy pipeline fails on the canary stage after the Tuesday infra change","expected":"The canary stage needs the new service account role; add it to the pipeline's IAM binding and re-run."}
```

From support tickets or Slack questions: same shape, curated `expected`. The deltas claim only what `expected` says good looks like.

## 3. Baseline, then value

```bash
kno baseline --evals cases.jsonl --agent openai:gpt-4.1 --max-cost-usd 2.00
kno value --evals cases.jsonl --pool pool.jsonl \
  --baseline-run-id <the run id from the baseline> --max-cost-usd 10.00
```

## 4. Read it back into Confluence decisions

| The line says | The action |
|---|---|
| Interval above zero | The page earns its place in the agent's context — pin it in grounding or the space's landing page |
| Interval crosses zero | No evidence yet — more questions touching that area |
| Tight interval near zero | Dead weight — archive the page (a wiki keeps history; grounding doesn't need the backlog) |
| Delta below zero | **Harmful.** A stale spec or runbook teaching the agent a wrong answer. Update it — and check which pages link to it, because they carry the stale claim too |
| CONTROL bound below zero | Helps its own area, hurts others — the page over-generalizes; scope it to the team it was written for |

Confluence pairs with Jira the way Zendesk articles pair with Zendesk tickets: one vendor holds the candidate knowledge, the other holds the evidence. Export both halves, curate the `expected`, and the rest of the recipe is identical.

*Vendor table and the general recipe: [Value your Zendesk knowledge](zendesk.md#same-recipe-different-source).*

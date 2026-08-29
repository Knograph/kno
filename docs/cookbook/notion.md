# Value your Notion knowledge: pages and databases for an internal-knowledge agent

What you're asking: **which of the pages and snippets in our workspace actually make the agent answer better?** Notion holds the candidate knowledge; the questions come from wherever your team actually asks them.

## What maps where

| Notion thing | Kno thing | How |
|---|---|---|
| Page (runbook, decision, onboarding doc) | Asset | `content` = page text, `kind: "knowledge"` |
| Database row (FAQ, policy, glossary) | Asset | One Asset per row — databases export as clean JSON |
| Slack question / ticket / doc-comment | Case | `input` = the question; `expected` = the vetted answer |

## 1. Export pages and databases

```bash
export NOTION_TOKEN=...
export DATABASE_ID=...

# A database — the cleanest shape Notion has:
curl -s -X POST "https://api.notion.com/v1/databases/$DATABASE_ID/query" \
  -H "Authorization: Bearer $NOTION_TOKEN" -H "Notion-Version: 2022-06-28" \
  | jq -c '.results[] | {id: .id, content: ([.properties[]? | tostring] | join(" ")), kind: "knowledge"}' \
  > pool.jsonl

# Pages: search for the space you care about, then fetch each page's blocks.
curl -s -X POST "https://api.notion.com/v1/search" \
  -H "Authorization: Bearer $NOTION_TOKEN" -H "Notion-Version: 2022-06-28" \
  -d '{"filter":{"property":"object","value":"page"}}' \
  | jq -c '.results[] | {id, title: ([.properties.title.title[]?.plain_text] | join(""))}' > pages.jsonl
```

Blocks export needs a walker — Notion pages are trees, not strings. Your team's export tooling likely already flattens them; feed that output to Kno instead of re-solving it here.

## 2. Build Cases from the questions your team actually asks

Curated `expected` answers — the step that decides what the numbers claim:

```jsonl
{"id":"q-01","input":"How do I rotate the staging database credentials?","expected":"Run the rotate script from the ops runbook; it updates staging, then the secrets manager, then restarts the staging pods."}
{"id":"q-02","input":"What is our policy on customer data retention?","expected":"Delete request data 90 days after account closure, per the retention policy."}
```

## 3. Baseline, then value

```bash
kno baseline --evals cases.jsonl --agent openai:gpt-4.1 --max-cost-usd 2.00 --yes
kno value --evals cases.jsonl --pool pool.jsonl \
  --baseline-run-id <the run id from the baseline> \
  --agent openai:gpt-4.1 --max-cost-usd 10.00 --yes
```

`--yes` answers the spend confirmation (above $1.00 Kno asks; a non-TTY run exits 2 without it), and value must name the same `--agent` as the baseline — it defaults to `fake:`. Once `select` and `export` have run, [read the whole story](read-the-whole-story.md).

## 4. Read it back into workspace decisions

| The line says | The action |
|---|---|
| Interval above zero | The page earns its place in the agent's grounding — pin it, or promote it to the team's canonical home |
| Interval crosses zero | No evidence yet — more questions touching that topic |
| Tight interval near zero | Dead weight — archive it; Notion search improves when the noise leaves |
| Delta below zero | **Harmful.** A stale decision or runbook teaching the agent a wrong answer. Update it, then re-value |
| CONTROL bound below zero | Helps its own topic, hurts others — the page over-generalizes; scope it |

Notion workspaces accumulate the way data does: everything stays, evidence accumulates. Kno is the audit that says which of it still earns its place.

*Vendor table and the general recipe: [Value your Zendesk knowledge](zendesk.md#same-recipe-different-source).*

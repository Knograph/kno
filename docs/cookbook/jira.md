# Value your Jira knowledge: issues as evals

What you're asking: **which project content actually makes my agent resolve tickets better?** Jira supplies the evals — resolved issues with the resolution written down — and the candidate content usually lives next door in Confluence (see the [Confluence recipe](confluence.md)).

## What maps where

| Jira thing | Kno thing | How |
|---|---|---|
| Resolved issue | Case | `input` = summary + description; `expected` = the resolution notes |
| Epic / spec / ADR text | Asset | `content` = the body, `kind: "knowledge"` — or export the same docs from Confluence |
| Runbook linked in the resolution | Asset | Same shape |

## 1. Export resolved issues

```bash
export JIRA_HOST=acme.atlassian.net
export JIRA_EMAIL=you@acme.com
export JIRA_API_TOKEN=...

curl -s "https://$JIRA_HOST/rest/api/3/search?jql=resolution%20is%20not%20EMPTY%20order%20by%20updated%20desc&maxResults=200" \
  -u "$JIRA_EMAIL:$JIRA_API_TOKEN" \
  | jq -c '.issues[] | select(.fields.description != null) | {id: .key, input: (.fields.summary + "\n\n" + (.fields.description.content // [] | tostring)), expected: ""}' \
  > cases-raw.jsonl
```

The Jira v3 description is a structured doc object, not a string — the `tostring` above is a placeholder. Render it to plain text with whatever your team's exporter does, or pull the plain summary plus the comments that carried the resolution.

## 2. Curate the expected answers

Fill each `expected` with what the resolution *should* have been — one line of curation per Case:

```jsonl
{"id":"PROJ-4821","input":"Deploy pipeline fails on the canary stage after the Tuesday infra change","expected":"The canary stage needs the new service account role; add it to the pipeline's IAM binding and re-run."}
```

## 3. Baseline, then value

```bash
kno baseline --evals cases.jsonl --agent openai:gpt-4.1 --max-cost-usd 2.00
kno value --evals cases.jsonl --pool pool.jsonl \
  --baseline-run-id <the run id from the baseline> --max-cost-usd 10.00
```

## 4. Read it back into Jira decisions

| The line says | The action |
|---|---|
| Interval above zero | This doc earns its place in the agent's context — link it from the issue type's template or pin it in grounding |
| Interval crosses zero | No evidence yet — more resolved issues in that component |
| Tight interval near zero | Dead weight — retire or merge the doc |
| Delta below zero | **Harmful.** A stale spec teaching the agent a wrong fix. Update it, then re-value |
| CONTROL bound below zero | Helps its own component, hurts others — the doc over-generalizes; scope it |

Jira is the eval source in this pairing; Confluence is the pool. Together they cover the whole "does our written knowledge actually help?" question for an engineering agent.

*Vendor table and the general recipe: [Value your Zendesk knowledge](zendesk.md#same-recipe-different-source).*

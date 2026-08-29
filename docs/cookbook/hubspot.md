# Value your HubSpot knowledge: articles and tickets

What you're asking: **which of the things my support or sales agent reads actually make it answer better?** HubSpot holds both halves — knowledge base articles and support tickets.

## What maps where

| HubSpot thing | Kno thing | How |
|---|---|---|
| Knowledge base article | Asset | `content` = article body HTML stripped, `kind: "knowledge"` |
| Support ticket | Case | `input` = subject + description; `expected` = the resolution |
| Sales playbook / pricing page | Asset | Same shape |

## 1. Export the articles

```bash
export HUBSPOT_TOKEN=...

curl -s "https://api.hubapi.com/cms/v3/knowledge/articles?limit=100" \
  -H "Authorization: Bearer $HUBSPOT_TOKEN" \
  | jq -c '.results[] | {id: ("article-" + (.id|tostring)), content: .content, title: .title, kind: "knowledge"}' \
  > pool.jsonl
```

The `content` field carries markup — strip it before committing; the agent reads text.

## 2. Build Cases from tickets

```bash
curl -s "https://api.hubapi.com/crm/v3/objects/tickets?limit=100&properties=subject,content" \
  -H "Authorization: Bearer $HUBSPOT_TOKEN" \
  | jq -c '.results[] | select(.properties.content != null) | {id: ("ticket-" + .id), input: (.properties.subject + "\n\n" + .properties.content), expected: ""}' \
  > cases-raw.jsonl
```

Fill each `expected` by hand — what the agent *should* have answered. One line of curation per Case; the deltas claim only what `expected` says good looks like.

## 3. Baseline, then value

```bash
kno baseline --evals cases.jsonl --agent openai:gpt-4.1 --max-cost-usd 2.00 --yes
kno value --evals cases.jsonl --pool pool.jsonl \
  --baseline-run-id <the run id from the baseline> \
  --agent openai:gpt-4.1 --max-cost-usd 10.00 --yes
```

`--yes` answers the spend confirmation (above $1.00 Kno asks; a non-TTY run exits 2 without it), and value must name the same `--agent` as the baseline — it defaults to `fake:`. Once `select` and `export` have run, [read the whole story](read-the-whole-story.md).

## 4. Read it back into HubSpot decisions

| The line says | The HubSpot action |
|---|---|
| Interval above zero | Article earns its place — pin it in chat flows or the agent's grounding |
| Interval crosses zero | No evidence yet — more tickets in that topic area |
| Tight interval near zero | Retire or merge the article |
| Delta below zero | **Harmful.** Outdated content teaching the agent a wrong answer. Retire it and check for contradictions |
| CONTROL bound below zero | Helps its own slice, hurts a neighbor — narrow the scope or fix the content |

The same recipe values playbooks and pricing pages: content as Assets, tickets as Cases, curated `expected` as the scoring standard.

*Vendor table and the general recipe: [Value your Zendesk knowledge](zendesk.md#same-recipe-different-source).*

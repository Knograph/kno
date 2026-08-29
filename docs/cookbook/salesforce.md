# Value your Salesforce knowledge: articles and resolved cases

What you're asking: **which of the things my sales or service agent reads actually make it answer better?** Salesforce holds both halves — Knowledge articles and resolved Cases.

## What maps where

| Salesforce thing | Kno thing | How |
|---|---|---|
| Knowledge article | Asset | `content` = article body (HTML stripped), `kind: "knowledge"` |
| Resolved Case | Case | `input` = subject + description; `expected` = the resolution notes |
| Price book / product description | Asset | Same shape |

## 1. Export the articles

REST API with SOQL. Your token stays in the environment:

```bash
export SF_INSTANCE=acme.my.salesforce.com
export SF_TOKEN=...

curl -s "https://$SF_INSTANCE/services/data/v61.0/query/?q=$(jq -rn --arg q "SELECT Id, Title, Summary, Body__c FROM Knowledge__kav WHERE PublishStatus='Online' AND IsLatestVersion=true" '$q|@uri')" \
  -H "Authorization: Bearer $SF_TOKEN" \
  | jq -c '.records[] | {id: ("article-" + .Id), content: (.Body__c // .Summary // ""), title: .Title, kind: "knowledge"}' \
  > pool.jsonl
```

`Body__c` is the long-text field on a custom Knowledge type — adjust the field name to your org's setup; the shape is what matters.

## 2. Build Cases from resolved Cases

```bash
curl -s "https://$SF_INSTANCE/services/data/v61.0/query/?q=$(jq -rn --arg q "SELECT Subject, Description, Resolution__c FROM Case WHERE Status='Closed' LIMIT 200" '$q|@uri')" \
  -H "Authorization: Bearer $SF_TOKEN" \
  | jq -c '.records[] | select(.Description != null) | {id: ("case-" + (.Id // .Subject)), input: (.Subject + "\n\n" + .Description), expected: (.Resolution__c // "")}' \
  > cases-raw.jsonl
```

Then curate the `expected` answers — the resolution notes tell you what *was* done; the deltas claim only what your `expected` says good looks like.

## 3. Baseline, then value

```bash
kno baseline --evals cases.jsonl --agent openai:gpt-4.1 --max-cost-usd 2.00 --yes
kno value --evals cases.jsonl --pool pool.jsonl \
  --baseline-run-id <the run id from the baseline> \
  --agent openai:gpt-4.1 --max-cost-usd 10.00 --yes
```

`--yes` answers the spend confirmation (above $1.00 Kno asks; a non-TTY run exits 2 without it), and value must name the same `--agent` as the baseline — it defaults to `fake:`. Once `select` and `export` have run, [read the whole story](read-the-whole-story.md).

## 4. Read it back into Salesforce decisions

| The line says | The Salesforce action |
|---|---|
| Interval above zero | Article earns its place — surface it in Einstein responses or pin it in the agent's grounding |
| Interval crosses zero | No evidence yet — more resolved Cases in that product area |
| Tight interval near zero | Retire or merge the article |
| Delta below zero | **Harmful.** An outdated article teaching the agent a wrong answer. Retire it, and check for contradictions with current policy |
| CONTROL bound below zero | Helps its own slice, hurts a neighbor — narrow the article's scope or fix the content |

The same recipe values price books, product descriptions, and call scripts: content as Assets, resolved Cases as Cases, curated `expected` as the scoring standard.

*Vendor table and the general recipe: [Value your Zendesk knowledge](zendesk.md#same-recipe-different-source).*

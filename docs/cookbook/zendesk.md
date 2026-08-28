# Value your Zendesk knowledge: articles, macros, and policies

What you're asking: **which of the things my support agent reads actually make it answer better?** Zendesk already has every input Kno needs — help-center articles, macros, and tickets. This recipe turns them into one baseline, one pool, and a per-asset verdict.

## What maps where

| Zendesk thing | Kno thing | How |
|---|---|---|
| Solved tickets (question → the answer that closed it) | Cases | One Case per ticket: the customer's question as `input`, the answer as `expected` |
| Help-center articles | Assets | One Asset per article: `content` is the article body |
| Macros, canned snippets | Assets | One Asset per macro |
| Policies (return policy, shipping policy) | Assets | One Asset per policy |

A Case's `expected` is what a correct answer looks like — it is how Kno scores, so this is the one place curation beats volume. Start with 50–200 tickets a human can vouch for; a larger sample of unvetted tickets teaches Kno what your agent *does*, not what it *should do*.

## 1. Export the articles

Zendesk's Help Center API. Your credentials stay in the environment — never in a file Kno reads.

```bash
export ZENDESK_SUBDOMAIN=acme
export ZENDESK_EMAIL=you@acme.com
export ZENDESK_API_KEY=...

curl -s "https://$ZENDESK_SUBDOMAIN.zendesk.com/api/v2/help_center/articles?per_page=100" \
  -u "$ZENDESK_EMAIL/token:$ZENDESK_API_KEY" \
  | jq -c '.articles[] | {id: ("article-" + (.id|tostring)), content: .body, title: .title, kind: "knowledge"}' \
  > pool.jsonl
```

The `id` is the asset's identity in every Kno report — make it something you can read back to a Zendesk URL. Strip the HTML out of `.body` before you commit to the file; what the agent reads is the text.

## 2. Build the Cases from solved tickets

The quick version: export tickets solved by a macro or article, and use that content as the expected answer.

```bash
curl -s "https://$ZENDESK_SUBDOMAIN.zendesk.com/api/v2/search.json?query=type:ticket+solved" \
  -u "$ZENDESK_EMAIL/token:$ZENDESK_API_KEY" \
  | jq -c '.results[] | {id: ("ticket-" + (.id|tostring)), input: .subject, expected: .description}' \
  > cases.jsonl
```

The curated version (better numbers): pick 50–200 tickets yourself, and write the `expected` you would have wanted the agent to give. Fifteen minutes of curation is the difference between "what the agent does" and "what good looks like". One JSON object per line, both ways.

## 3. Baseline the agent as it is today

```bash
kno baseline --evals cases.jsonl --agent openai:gpt-4.1 --max-cost-usd 2.00
```

Note the run ID in the output — the next step references it. This baseline is the reference every delta is measured against, and it seals a holdout that nothing reads until `validate`.

## 4. Value the pool

```bash
kno value --evals cases.jsonl --pool pool.jsonl \
  --baseline-run-id <the run id from step 3> --max-cost-usd 10.00
```

Kno routes each Asset to the Cases it could plausibly affect, injects it, re-measures against fresh controls, and reports one line per Asset:

```
ASSET         DELTA (95% CI, positive = goal dir)  CONTROL             NOTE
article-1203  +0.1420  [+0.0810, +0.2030]   low -0.0010
article-0587  +0.0712  [+0.0120, +0.1304]   low +0.0031 (underpowered)
return-policy -0.0611  [-0.1102, -0.0120]   low -0.0833
macro-0031    +0.0009  [-0.0401, +0.0419]
```

*(Illustrative numbers. Real runs print this shape — the delta with its interval, the harm bound over the controls, and a note when a number would be dishonest.)*

## 5. Read it back into Zendesk decisions

| The line says | The Zendesk action |
|---|---|
| Interval entirely above zero | This article earns its place. Promote it — Help Center spotlight, pinned section, or agent suggested-article ranking |
| Interval crosses zero | No evidence yet. Collect more tickets for the slices it targets, or leave it where it is |
| Delta near zero with a tight interval | Dead weight. Candidate for retirement or merging into a sibling article |
| Delta below zero | **Harmful.** This article is teaching the agent a wrong answer. Retire it, and check whether its content contradicts the current policy |
| CONTROL bound below zero on an otherwise-positive Asset | It helps its own slice and hurts a neighboring one — fix the content or narrow its scope |
| `sample too small or ragged to form an interval` | The asset routed to too few Cases to measure. More tickets in that area, or a larger eval set |

**Which decisions can be automated later:** promotion and retirement land with `select`; the untouched-holdout check lands with `validate`. Until then, treat the value table as the evidence, and the human as the decision-maker.

## The numbers only mean what the Cases mean

If your Cases are "what the agent currently does", Kno measures fit, not quality. Curated `expected` answers are the difference between measuring your knowledge base against itself and measuring it against what a good answer looks like. That choice — not the vendor, not the model — is what the deltas claim.

## Same recipe, different source

The Zendesk shape — candidate content as Assets, real questions with vetted answers as Cases — transplants to any vendor that holds both halves:

| Scenario | Popular sources | Assets | Cases |
|---|---|---|---|
| Support | Zendesk, Intercom, Salesforce Service Cloud, Help Scout | Articles, macros, snippets, policies | Solved conversations, curated Q&A |
| Coding agent | GitHub, GitLab, Confluence, Notion | Repo docs, READMEs, style guides, examples | Issues/PRs with merged fixes, code-review comments |
| Sales assistant | Salesforce, HubSpot, Gong | Playbooks, pricing pages, objection responses | Won-deal discovery questions, recorded-call transcripts |
| E-commerce bot | Shopify, Stripe, Amazon Seller Central | Product descriptions, return policies, FAQs | Pre-purchase questions with the answer that closed the sale |
| Product docs | ReadMe, Mintlify, GitBook | Doc pages | Support questions your docs should have answered |
| Internal knowledge | Guru, Glean, Notion | Snippets, runbooks, decisions | Slack answers, onboarding questions |

The recipe is the same in every row: export both halves, curate the `expected` answers, baseline, value, then act on the table.

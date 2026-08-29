# Value your Stripe knowledge: docs for a payments agent

What you're asking: **which docs actually make my payments agent answer integration questions better?** Stripe publishes its docs as markdown in a public repository — the cleanest pool in this book — and integration questions are the evals.

## What maps where

| Stripe thing | Kno thing | How |
|---|---|---|
| Doc page (markdown) | Asset | `content` = the page text, `kind: "knowledge"` |
| Support question about your integration | Case | `input` = the question; `expected` = the correct answer for *your* integration |
| Your own integration's quirks | Asset | Overrides and caveats your agent must know |

The pool comes from Stripe's published docs; the *expected* answers come from your own integration. Stripe docs are not your docs — an answer that is correct for Stripe in general may be wrong for the integration you built. That tension is exactly what Kno measures.

## 1. Export the docs

```bash
git clone --depth 1 https://github.com/stripe/docs /tmp/stripe-docs
find /tmp/stripe-docs -name "*.mdx" -o -name "*.md" | head -300 | while read f; do
  jq -nc --arg id "stripe-doc-$(basename "$f" .mdx)" --arg content "$(cat "$f")" \
    '{id: $id, content: $content, kind: "knowledge"}' >> pool.jsonl
done
```

Cap content length to what reaches your agent's context, and skip pages you know your agent will never be asked about — measurement budget is better spent on the ambiguous pages.

## 2. Build Cases from your integration questions

The machine cannot write what correct looks like for *your* integration. Curate it:

```jsonl
{"id":"int-01","input":"Why did webhook retries stop after we rotated the endpoint secret?","expected":"The webhook endpoint was rotated in the dashboard but the new secret was never deployed — update the secret and replay the missed events."}
{"id":"int-02","input":"How do we test the refund path without moving real money?","expected":"Use test-mode cards and the test clock; refunds in test mode settle instantly and cost nothing."}
```

## 3. Baseline, then value

```bash
kno baseline --evals cases.jsonl --agent openai:gpt-4.1 --max-cost-usd 2.00 --yes
kno value --evals cases.jsonl --pool pool.jsonl \
  --baseline-run-id <the run id from the baseline> \
  --agent openai:gpt-4.1 --max-cost-usd 10.00 --yes
```

`--yes` answers the spend confirmation (above $1.00 Kno asks; a non-TTY run exits 2 without it), and value must name the same `--agent` as the baseline — it defaults to `fake:`. Once `select` and `export` have run, [read the whole story](read-the-whole-story.md).

## 4. Read it back into docs decisions

| The line says | The action |
|---|---|
| Interval above zero | The doc earns its place in the agent's context — pin it in retrieval or embed it |
| Interval crosses zero | No evidence yet — more questions touching that API area |
| Tight interval near zero | Dead weight — drop it from grounding |
| Delta below zero | **Harmful.** A doc that contradicts your integration's actual behavior. Fix the integration or the doc, then re-value |
| CONTROL bound below zero | The page helps its own questions and hurts others — likely a version mismatch; pin the right API version in your agent's grounding |

The same recipe values your own API reference, changelogs, and incident postmortems: content as Assets, integration questions as Cases, curated `expected` as the scoring standard.

*Vendor table and the general recipe: [Value your Zendesk knowledge](zendesk.md#same-recipe-different-source).*

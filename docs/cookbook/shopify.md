# Value your Shopify knowledge: products and policies for a storefront bot

What you're asking: **which product content and policies actually make my storefront bot answer better?** Shopify's Admin API exports the candidate content; buyer questions supply the evals.

## What maps where

| Shopify thing | Kno thing | How |
|---|---|---|
| Product (title + body_html) | Asset | `content` = product description, `kind: "knowledge"` |
| Shop policy (refund, shipping) | Asset | One Asset per policy |
| Buyer question that a good answer resolved | Case | `input` = the question; `expected` = the answer that resolved it |

## 1. Export products and policies

```bash
export SHOP=acme.myshopify.com
export SHOPIFY_TOKEN=...

curl -s "https://$SHOP/admin/api/2024-07/products.json?limit=250" \
  -H "X-Shopify-Access-Token: $SHOPIFY_TOKEN" \
  | jq -c '.products[] | {id: ("product-" + (.id|tostring)), content: (.title + "\n\n" + (.body_html // "")), kind: "knowledge"}' \
  > pool.jsonl

curl -s "https://$SHOP/admin/api/2024-07/policies.json" \
  -H "X-Shopify-Access-Token: $SHOPIFY_TOKEN" \
  | jq -c '.policies[] | {id: ("policy-" + .title), content: (.title + "\n\n" + .body), kind: "knowledge"}' \
  >> pool.jsonl
```

## 2. Build Cases from buyer questions

The quick version exports what your store already answers (Inbox transcripts, order-notes, reviews-with-questions). The curated version — the one that decides what the numbers mean — writes the expected answer by hand:

```jsonl
{"id":"buyer-01","input":"Does this ship to Germany, and how long does it take?","expected":"Yes. Orders to Germany ship within 3-5 business days via tracked post."}
{"id":"buyer-02","input":"What is the return window for sale items?","expected":"Sale items are final sale and cannot be returned."}
```

## 3. Baseline, then value

```bash
kno baseline --evals cases.jsonl --agent openai:gpt-4.1 --max-cost-usd 2.00 --yes
kno value --evals cases.jsonl --pool pool.jsonl \
  --baseline-run-id <the run id from the baseline> \
  --agent openai:gpt-4.1 --max-cost-usd 10.00 --yes
```

`--yes` answers the spend confirmation (above $1.00 Kno asks; a non-TTY run exits 2 without it), and value must name the same `--agent` as the baseline — it defaults to `fake:`. Once `select` and `export` have run, [read the whole story](read-the-whole-story.md).

## 4. Read it back into storefront decisions

| The line says | The storefront action |
|---|---|
| Interval above zero | This product description or policy earns its place in the bot's context — prioritize it in retrieval |
| Interval crosses zero | No evidence yet — more buyer questions for that category |
| Tight interval near zero | Dead weight — trim the description or drop it from grounding |
| Delta below zero | **Harmful.** A stale policy or a description that contradicts the real terms. Fix the content, then re-value |
| CONTROL bound below zero | Helps its own products, hurts others' questions — check for a policy that over-generalizes |

The same recipe values collections, size guides, and shipping tables: content as Assets, buyer questions as Cases, curated `expected` as the scoring standard.

*Vendor table and the general recipe: [Value your Zendesk knowledge](zendesk.md#same-recipe-different-source).*

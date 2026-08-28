# Value your GitHub knowledge: repo docs for a coding agent

What you're asking: **which of the things my coding agent reads actually make it write better code?** The repo itself is the pool — READMEs, CONTRIBUTING files, design docs, style guides — and closed issues are the evals.

## What maps where

| GitHub thing | Kno thing | How |
|---|---|---|
| Closed issue whose fix merged | Case | `input` = issue title + body; `expected` = the approach the merged PR took |
| README, CONTRIBUTING, design docs | Asset | One Asset per markdown file: `content` is the text |
| Style guide, lint rationale, examples dir | Asset | Same |
| PR description of a fix | Case `expected` | What "correct" looked like for that issue |

`expected` is how Kno scores. A merged PR description is evidence of what *was* done, not necessarily what *should* be done — the same caveat as every recipe in this book: curate the ones a human vouchsafes, or you measure fit, not quality.

## 1. Export the repo docs

`gh` does the auth; the tree API does the rest:

```bash
gh api repos/acme/core --jq '.tree[] | select(.path | endswith(".md")) | .path' \
  | head -200 | while read p; do
      gh api "repos/acme/core/contents/$p" --jq 'select(.encoding == "base64") | @base64d' > /dev/null
      echo "{\"id\":\"doc-$(echo "$p" | tr '/.' '--')\",\"content\":$(gh api "repos/acme/core/contents/$p" --jq 'select(.encoding == "base64") | @base64d' | jq -Rs .),\"kind\":\"knowledge\"}" >> pool.jsonl
    done
```

That is the shape, not the gospel — dedupe, skip generated docs (CHANGELOGs, lockfiles' neighbors), and cap content length to what actually reaches your agent's context.

## 2. Build Cases from closed issues

```bash
gh issue list --repo acme/core --state closed --limit 200 \
  --json number,title,body \
  | jq -c '.[] | select(.body != null and .body != "") | {id: ("issue-" + (.number|tostring)), input: (.title + "\n\n" + .body), expected: ""}' \
  > cases-raw.jsonl
```

The `expected` field is empty because the machine cannot know what good looks like. Fill it in from the merged PR — one line of curation per Case, the step that decides what the deltas claim:

```jsonl
{"id":"issue-4821","input":"Docs: retry policy is not documented for the SDK","expected":"Document the retry policy in the SDK guide, matching the server behavior: three attempts, exponential backoff, jitter."}
```

## 3. Baseline, then value

```bash
kno baseline --evals cases.jsonl --agent openai:gpt-4.1 --max-cost-usd 2.00
kno value --evals cases.jsonl --pool pool.jsonl \
  --baseline-run-id <the run id from the baseline> --max-cost-usd 10.00
```

## 4. Read it back into repo decisions

| The line says | The repo action |
|---|---|
| Interval above zero | This doc earns its place in the agent's context. Pin it in the agent config, not just the repo |
| Interval crosses zero | No evidence yet — collect more issues touching its subject area |
| Tight interval near zero | Dead weight. Candidate for deletion or merging into the parent doc |
| Delta below zero | **Harmful.** The doc is teaching the agent a wrong pattern. Fix or retire it, then check what contradicts it |
| `sample too small or ragged` | Not enough closed issues in that area. Either expand the eval set or leave the asset unjudged |

The same shape values example repos, style guides, and API references: content as Assets, closed issues as Cases, curated `expected` as the scoring standard.

*Vendor table and the general recipe: [Value your Zendesk knowledge](zendesk.md#same-recipe-different-source).*

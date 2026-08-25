# Point Kno at your own provider

Everything so far has used `fake:`, the local agent that costs nothing. This is the recipe for measuring a real one — an OpenAI or Anthropic model, a compatible gateway, or a model server on your own machine.

Read this before the first run that can bill you.

## The short version

```bash
export OPENAI_API_KEY=sk-...

kno baseline \
  --evals cases.jsonl \
  --agent openai:gpt-4.1 \
  --max-cost-usd 2.00
```

That is a complete, capped run. Kno prices every Case from its own table, so you do not have to tell it what a call costs.

## Keys come from the environment, never from a flag

There is no `--api-key`, and there will not be one. A key on a command line is written to your shell history, shown in `ps` output to every user on the machine, and captured in CI logs.

Each scheme reads a default variable:

| Scheme | Variable |
|---|---|
| `openai:` | `OPENAI_API_KEY` |
| `anthropic:` | `ANTHROPIC_API_KEY` |

For any **other** host you must say which variable holds its key:

```bash
export GROQ_API_KEY=gsk_...

kno baseline --evals cases.jsonl \
  --agent openai:llama-3.3-70b \
  --base-url https://api.groq.com/openai/v1 \
  --key-env api.groq.com=GROQ_API_KEY
```

`--key-env` takes `host=VARIABLE_NAME`. The **name** of a variable is not a secret; the key itself still only ever comes from the environment.

This is not ceremony. Kno will not fall back to `OPENAI_API_KEY` for a host that is not OpenAI's, because that would mail your OpenAI key to whoever you pointed `--base-url` at.

A host with no binding simply gets no credential — which is correct for a local model server, and produces a 401 from anything else. For the provider's **own** default host (`api.openai.com`, `api.anthropic.com`) a missing key is refused before any request, since that host certainly needs one.

## Capping spend

Two caps, and they bound different things:

```bash
--max-cost-usd 5.00    # stop before spending more than this
--max-calls 500        # stop after this many agent calls
```

A run that hits either stops **resumably**: `--resume` continues where it left off without paying for anything twice. The exit code is `2`, not `1` — a budget stop is the run doing what it was told, not a failure.

Above **$1.00** estimated, Kno asks before starting. There is no interactive prompt yet, so it prints the figure and declines; pass `--yes` to proceed. `--yes` still prints what it is agreeing to, so the number is in your scrollback and in your CI log.

### If your model has no price

Kno's price table does not cover every model, and it deliberately refuses to guess: a longest-prefix match once resolved `claude-opus-5-fast` to `claude-opus-5` and authorized a run at the base rate, when fast mode is priced well above it.

Under a cost cap, an unpriced model is refused **once**, before any Case runs. Supply the rates yourself:

```bash
--price-input-per-mtok 3.00 --price-output-per-mtok 15.00
```

Both are required. Half a price produces an estimate that is wrong in the direction that under-reserves, which is a cap that does not bind.

`kno doctor` prints which models the table covers.

## A local model server

vLLM, Ollama, llama.cpp, LM Studio — anything speaking the OpenAI chat-completions shape:

```bash
kno baseline --evals cases.jsonl \
  --agent openai:my-local-model \
  --base-url http://localhost:8000/v1 \
  --allow-insecure-base-url \
  --allow-private-address \
  --cost-per-call-usd 0
```

Three flags need explaining, and each is a separate opt-in on purpose — somebody who needs a loopback address should not also be waiving TLS to the public internet:

- **`--allow-insecure-base-url`** permits plain `http://`.
- **`--allow-private-address`** permits loopback and private ranges. Link-local (`169.254.0.0/16`) is **not** covered and cannot be opted into: `169.254.169.254` is where cloud instance metadata lives, and a tool that fetches a URL and stores the response body has no business reaching it.
- **`--cost-per-call-usd 0`** states that these calls are free. Passing it *explicitly* is the claim; leaving it out is not, and without either that flag or `--accept-unknown-cost` Kno refuses to start rather than spending an amount nobody can state.

Older servers may need `--use-legacy-max-tokens`, which sends `max_tokens` instead of `max_completion_tokens`.

## Anthropic

```bash
kno baseline --evals cases.jsonl \
  --agent anthropic:claude-opus-5 \
  --max-output-tokens 1024 \
  --max-cost-usd 2.00
```

`--max-output-tokens` is **required** here: the Messages API requires `max_tokens`, and a cost cap cannot bound an output term that has no ceiling.

## Naming the endpoint

Two equivalent forms:

```bash
--agent openai:gpt-4.1@https://gateway.example.com/v1
--agent openai:gpt-4.1 --base-url https://gateway.example.com/v1
```

Use one or the other. Passing both is refused — two ways of saying the same thing, disagreeing, is a mistake rather than an ordering question.

The URL must be an **endpoint root**, absolute, with a scheme. Not a request URL: anything after the root would be appended to every request Kno makes, so a query string or a fragment is refused.

**Never put a credential in the URL.** `https://user:pass@host` is refused, and so is `?api_key=...`. A base URL is recorded on the Run, emitted on the event stream, and printed in `--json` — so a key placed there ends up in four durable places at once. A credential inside the URL **path** is not currently detected, because a path segment is indistinguishable from a legitimate route prefix; use `--key-env`.

## Redirects are refused

Kno does not follow them, cross-host or same-host. Go's HTTP client strips `Authorization` on a cross-domain redirect but **not** `x-api-key`, which is how Anthropic authenticates — so a base URL that redirects elsewhere would forward that key verbatim. Point `--base-url` at the endpoint directly.

## When a run stops early

Some failures cannot get better by trying again: a rejected credential, an unpaid account, a model name that does not exist, your provider's own spend cap. Kno stops the whole run on the first one, rather than making the same doomed request for every remaining Case.

The Cases it did not measure stay unmeasured — fix the problem and `--resume`, and they are picked up rather than skipped.

## Checking what is supported

```bash
kno doctor
```

Prints the adapters, which cost money, the goals, and the price table's date. It contacts nothing and reads no credential, so it is safe to run while diagnosing a failed run.

## See also

- [Score your agent for the first time](first-baseline.md) — the `fake:` walkthrough this builds on
- [Gate a deploy on Kno in CI](ci-gate.md) — exit codes and `--json`, now with real spend
- [What the numbers mean](../what-the-numbers-mean.md) — what a cost figure does and does not claim

# Kno

**Kno tells you what data actually makes your AI better.**

Give Kno your agent, your evals, and the data you're considering adding. Kno measures which documents, examples, facts, instructions, and other assets improve the agent, how much they improve it, what they cost, and where they belong.

Instead of evaluating only models and prompts, Kno treats **data as an experimental variable** and measures its marginal contribution to agent performance.

Single Go binary. No infra. Works with any OpenAI-compatible endpoint, Anthropic, or your own agent behind a shell command.

> **Status: early.** `baseline`, `value`, `select`, and `export` ship; `validate` is next. The default agent is a local fake that costs nothing, so you can see the whole loop work before pointing it at something that bills you. See [Status](#status) for what exists.

## Why Kno?

Agent teams have lots of data and very little evidence about which of it actually helps: product docs, support conversations, examples, policies, tool descriptions, few-shot demonstrations, internal knowledge, synthetic data, behavioral examples.

The usual approach is some combination of "put it in RAG", "stuff it into context", or "fine-tune on it".

Kno asks a different question:

> **Did this data actually improve the outcome enough to justify including it?**

```
Traditional evals

Prompt ─────┐
Model ──────┼──► Agent ───► Score
Tools ──────┘


Kno

                  ┌─ Document A ──► +12%
                  ├─ Example B ───►  +8%
Agent + Evals ────┼─ Policy C ────►   0%
                  └─ Dataset D ───►  -3%
                         │
                         ▼
              impact × cost × destination
```

**Kno does not replace your evals. It uses them.** Existing evals tell you whether your agent is getting better or worse. Kno uses those evals to determine *why a particular data asset changes the outcome, and whether that asset is worth keeping*.

```
Eval framework:  "Does version B perform better than version A?"

Kno:             "Which of these 500 data assets caused the improvement,
                  how confident are we, what did it cost, and where
                  should each one live?"
```

### Use Kno when you want to know

- Which documents actually improve my RAG agent?
- Which examples belong in my system prompt?
- Which few-shot examples are earning their token cost?
- Which data should I fine-tune on?
- Is this new knowledge source actually improving the agent?
- Which data is redundant?
- Which data is actively hurting performance?
- What knowledge is my agent missing entirely?
- Is the improvement worth the inference cost?

### What counts as an Asset

An asset can be almost anything you're considering feeding your agent:

`document · fact · few-shot example · policy · conversation · tool example · instruction · training example`

### Example: a support agent

Suppose your support agent is bad at refund questions. You have 300 help-center documents, 2,000 historical support conversations, 50 curated examples, an outdated refund policy, and a new one. Kno evaluates the candidate assets against your refund evals and (with a real agent) reports something shaped like this:

```text
new_refund_policy.md     +18%     keep → knowledge_base
example_42.json           +7%     keep → context
example_91.json           +1%     reject
old_refund_policy.md      -9%     reject → harmful
```

*(The numbers above are illustrative. Real runs report a delta with its confidence interval, and the destination column arrives with `select` — see [Status](#status).)*

## Install

```bash
curl -sSfL https://raw.githubusercontent.com/uknoAI/kno/main/install.sh | sh
```

The script picks the right build for your platform, **verifies the SHA-256 checksum**, verifies the cosign signature if you have `cosign` installed, and drops the binary on your PATH. It is short and worth reading before you pipe it anywhere.

Or take the archive yourself from [Releases](https://github.com/uknoAI/kno/releases) — macOS, Linux and Windows, amd64 and arm64. Or build from source:

```bash
go install github.com/knograph/kno/cmd/kno@latest
```

A `go install` binary reports the module version with no commit or date — `kno version v0.0.1`. A released binary reports both — `kno version v0.0.1 (a1b2c3d, 2026-08-26T…)`. The parenthetical is how you tell them apart in a bug report, and it is the only functional difference.

Homebrew:

```sh
brew tap uknoAI/homebrew-tap
brew install kno
```

The formula pins each platform archive's SHA-256 and is updated automatically by goreleaser on every release.

## Quickstart

**0. See the whole loop first, for free:**

```bash
kno demo
```

One command writes a small eval set and asset pool into `./kno-demo`, then runs all five
stages over them — baseline, value, select, export, report — against the built-in `fake:`
agent. It spends nothing, sends nothing anywhere, and reads no configuration: not `kno.yaml`,
not `KNO_*`. The files stay on disk afterwards, because the next thing worth doing is editing
them.

**The demo's deltas read `+0.0000`, its score reads `1.000`, and its portfolio comes back
empty — on purpose.** `fake:` answers every Case with what the Case expects, and injecting an
asset cannot change a deterministic answer, so no asset measures any effect. The intervals
around those zeros are real; the effects are zero. An empty portfolio is a legal, first-class
outcome, and the rejection log says why for each asset — which is the tool doing its job
rather than nothing happening. `kno demo` prints those three sentences itself, every time.

The four steps below are the same loop by hand, which is what you will do with your own data.

**1. Write some Cases** — one scoreable interaction per line, each with a stable `id`:

```jsonl
{"id":"refund-01","input":"How do I get a refund?","expected":"Refunds are issued within 5 business days."}
{"id":"ship-01","input":"When does my order ship?","expected":"Orders ship within 1 business day."}
```

**2. Measure the agent as it is today** — the reference every later number is compared against:

```bash
kno baseline --evals cases.jsonl
```

```
Baseline 20260821T091515-ffc3097d49da
  cases      44 scored, 0 errored (of 44 dev; 6 held back)
  score      1.000
  spent      $0.00 over 44 call(s)
  status     completed

  warning: the holdout has only 6 cases, too few for a meaningful confidence interval at validate

Scores and traces are recorded. `kno purge` removes trace content when you no longer need it.
```

**`6 held back`** is the holdout. Nothing reads it — not this run, not valuation, not selection — until `validate`. That constraint is the reason any number Kno reports later means anything. [Why](docs/mental-model.md#why-devholdout-exists).

**3. Write a pool of candidate assets:**

```jsonl
{"id":"refund-policy-v3","content":"Refunds are processed within 5 business days.","kind":"knowledge"}
{"id":"refund-example-17","content":"Example: a 30-day-old refund request is declined.","kind":"knowledge"}
{"id":"brand-guide","content":"Use sentence case everywhere.","kind":"knowledge"}
```

**4. Value them** — each asset is injected into the slices it could affect, re-measured against fresh controls, and reported with an interval:

```bash
kno value --evals cases.jsonl --pool pool.jsonl --baseline-run-id <the run id from step 2>
```

```
Planning 150 measurements over 3 assets against baseline 20260821T091515-ffc3097d49da.
Value run 20260828T233124-05cda2dcdac6 (RUN_STATUS_COMPLETED)

ASSET         DELTA (95% CI, positive = goal dir)  CONTROL             NOTE
brand-guide   +0.0000  [-0.1260, +0.1260]   low -0.1938 (underpowered)
refund-example-17  +0.0000  [-0.1260, +0.1260]   low -0.1938 (underpowered)
refund-policy-v3  +0.0000  [-0.1260, +0.1260]   low -0.1938 (underpowered)

Scores and traces are recorded. `kno purge` removes trace content when you no longer need it.
```

![Kno quickstart](docs/quickstart.gif)

**The deltas read 0.0000 here for the same reason they do in `kno demo`**, and it is worth repeating rather than assuming: `fake:` answers every Case with what the Case expects. The quickstart proves the loop runs — assets routed, injected, re-measured against controls, reported with intervals — costs nothing, and seals a holdout, not that any asset helps. Point it at a real provider and the numbers start meaning something:

```bash
export OPENAI_API_KEY=sk-...
kno baseline --evals cases.jsonl --agent openai:gpt-4.1 --max-cost-usd 2.00 --yes
kno value --evals cases.jsonl --pool pool.jsonl --baseline-run-id <run id> --agent openai:gpt-4.1 --max-cost-usd 5.00 --yes
```

Keys come from the environment, never from a flag. Kno prices each Case from its own table, so the cap binds *before* the call rather than at settlement. `kno doctor` prints which providers, models, and goals this build supports — it contacts nothing.

Full walkthrough: **[Score your agent for the first time](https://github.com/uknoAI/kno-examples/blob/main/recipes/first-baseline.md)**, then **[Point Kno at your own provider](https://github.com/uknoAI/kno-examples/blob/main/recipes/your-own-provider.md)** — or **[Score your agent against Claude](https://github.com/uknoAI/kno-examples/blob/main/recipes/anthropic.md)** for the Anthropic API, **[on Bedrock](https://github.com/uknoAI/kno-examples/blob/main/recipes/bedrock.md)** for AWS, and **[on Vertex](https://github.com/uknoAI/kno-examples/blob/main/recipes/vertex.md)** for Google Cloud. Got a support stack already? **[Value your Zendesk knowledge](https://github.com/uknoAI/kno-examples/blob/main/recipes/zendesk.md)** shows the whole recipe against real vendor data. Every recipe in one place: **[the cookbook index](docs/cookbook/README.md)**.

## How it works

Five stages, one question each:

| Stage | Question |
|---|---|
| **Baseline** | How good is the agent now? |
| **Value** | Which assets improve it? |
| **Select** | Which combination should I keep? |
| **Validate** | Does the combination still work on untouched evals? |
| **Export** | Where should each asset go? |

The deep explanation — routing, fresh controls, holdout separation, regression detection, cost-aware ranking — is **[the mental model](docs/mental-model.md)**, one page.

## Why trust the result?

Kno is deliberately opinionated about experimentation:

- **Holdouts stay sealed.** Selection never sees the validation set.
- **Deltas include uncertainty.** Kno reports confidence intervals rather than point estimates pretending to be truth — a delta without its interval is not reported at all. [What the numbers mean](docs/what-the-numbers-mean.md).
- **Regression is measured separately.** An asset that improves one class of requests can still break another; controls measure exactly that.
- **Cost is part of value.** A tiny asset producing a 5% improvement can be more valuable than a giant document producing 6% — ranking is per dollar, not per point.
- **Provider failures aren't bad scores.** Infrastructure errors don't artificially lower your baseline; they're counted separately.
- **Nothing spends your money silently.** Every path that can call a provider goes through a budget guard: estimate, confirm, checkpoint. Caps are enforced *before* the call, not discovered at settlement.
- **Interrupting is boring.** Work is checkpointed as each Case completes, in one transaction with its result. `--resume` skips what's finished and reconstructs prior spend from disk, so an interrupted run cannot pay twice.
- **Your traces stay yours, and nothing expires on its own.** Runs are stored locally in SQLite, including the agent's output — which is conversation content if your evals come from production logs. Kno itself sends nothing anywhere — there is no telemetry of content, ever. Your Cases go to whatever provider you point Kno at, and that provider's retention is theirs. `kno purge` deletes it when you decide to, keeping the scores and costs so the run stays resumable. [Retention, in full](https://github.com/uknoAI/kno-examples/blob/main/recipes/retention.md).

## Supported agents and providers

- `openai:` — any OpenAI-compatible endpoint via `--base-url` (vLLM, Ollama, llama.cpp need no key)
- `anthropic:`
- `bedrock:` — Claude models on AWS Bedrock (Converse, SigV4-signed, env-only credentials)
- `vertex:` — Claude models on Google Vertex AI (`:rawPredict`, stdlib JWT exchange, env-only credentials)
- `fake:` — the local agent that costs nothing
- `exec:` and `tuned:` arrive with the stages that need them

**Eval sources:** `--evals` takes a JSONL path, or a platform dataset directly —
`langsmith:<dataset>`, `langfuse:<dataset>`, `braintrust:<dataset>`,
`hf:<org>/<name>/<config>/<split>`. **Pools:** `--pool` takes JSONL, `csv:<file>`,
`md:<file-or-dir>`, or `hf:<org>/<name>/<config>/<split>:<kind>`. Platform credentials are
environment-only, never in `kno.yaml`.

## Evaluation best practices

Kno measures how candidate data changes the outcomes you care about — attribution quality is
bounded by the quality and granularity of your eval signal. A broad eval yields noisy
attribution; this section exists so "noisy" reads as the eval's fault, not Kno's.

- **Evaluate specific behaviors.** Prefer "answers refund-policy questions correctly" over a
  single "agent quality" score.
- **Keep Cases atomic.** One Case tests one primary behavior.
- **Use enough Cases per behavior.** A single example rarely separates improvement from
  variance — and Kno reports the interval, so underpowered measurements are labeled, not
  papered over.
- **Separate independent dimensions.** Accuracy, policy compliance, tool selection, and tone
  deserve separate Goals.
- **Use representative inputs.** The eval distribution should resemble the tasks the agent
  actually encounters.
- **Holdouts are enforced, not requested.** Kno seals a holdout at baseline time and nothing
  reads it until `validate` — leakage is a bug, not a habit to avoid.
- **Prefer deterministic scoring.** Exact match and programmatic checks beat subjective judges.
- **Define judge rubrics tightly.** An ambiguous rubric is noisy attribution with extra steps.
- **Start granular, aggregate later.** Kno rolls specific outcomes up more reliably than it
  explains a single coarse score.

Kno can check most of that for you, before you spend anything:

```bash
kno eval inspect --evals cases.jsonl
```

It reports how many distinct behaviors your tags describe, how small an effect each behavior's
Cases could separate from noise, how much of the set sits under one catch-all tag, and whether
the holdout is large enough for `validate`. It calls no model, writes nothing, and exits 0
whatever it finds. [Check whether your evals can attribute
anything](docs/cookbook/check-your-evals.md).

> **Rule of thumb:** your eval defines what "better" means. Kno tells you which data caused
> that metric to move.

Too coarse — "Is this a good support agent?" Better:

```text
- Correctly answers refund-policy questions
- Escalates account-security issues
- Never promises unsupported refunds
- Uses the correct billing tool
- Stays within response-length requirements
```

The full guide — how granular, how many Cases are enough, judge vs deterministic scoring,
multi-dimensional Goals, anti-patterns, and per-workload examples — is
**[Evaluation design](docs/evaluation-design.md)**.

## Exit codes

A CI gate branches on these, so they're a contract rather than an afterthought.

| Code | Meaning | What CI should do |
|---|---|---|
| `0` | Completed | Continue |
| `1` | Failed | Fail the build — something is broken |
| `2` | Stopped at a budget cap | Not a failure. The run did what you configured |
| `3` | Validation failed | Fail the build — the deploy gate (reserved for `kno validate`) |
| `4` | Interrupted by a signal or deadline | Not a failure. Resume it |

`2` and `4` both leave a resumable run. Reporting either as `1` would train people to ignore `1`, which is the code that actually means something is wrong. Recipe: **[Gate a deploy on Kno in CI](https://github.com/uknoAI/kno-examples/blob/main/recipes/ci-gate.md)**.

## Status

Two tables, because "stage" and "command" are two different things and one table conflating them is how they drifted apart. A **Stage** is a step of the pipeline, named by the `Stage` enum in [`proto/kno/v1/run.proto`](proto/kno/v1/run.proto). A **command** is a verb the CLI registers — some are stages, some (`report`, `demo`, `doctor`) compose or inspect them and are deliberately not stages.

Both tables are machine-checked against the code: [`docs/status.json`](docs/status.json) is the generated, machine-readable version (`make status`), and `make status-check` — inside `make check` — fails if either table, the declaration in `cli/status.go`, or the command tree drifts from the others.

### Stages

| Stage | What it does | State |
|---|---|---|
| **Baseline** | Run the agent over the dev Cases, score against the Goal, persist every result | **Shipped** |
| **Value** | Route each Asset to the slices it could affect, inject, re-run against controls, record Δ with an interval | **Shipped** |
| **Select** | Build a Portfolio under budget, with a rejection log; every decision at a Bonferroni-corrected interval | **Shipped** |
| **Validate** | Measure the Portfolio as a set against the untouched holdout | Planned |
| **Export** | Render the selected assets into the destination grammar: context pack, knowledge-base manifest, or tuning-set JSONL | **Shipped** |

`Stage` carries `STAGE_VALIDATE` already, and that is not an oversight: the schema leads the implementation ([CLAUDE.md](CLAUDE.md), "proto first"), so enum membership does not mean shipped. Which stages ship is declared once, in `cli/status.go`, and cross-checked against this table, the enum, and the command tree.

### Commands

| Command | What it does |
|---|---|
| `kno init` | Write a `kno.yaml` configuration file |
| `kno demo` | Run the whole loop against `fake:`, for free, on data it writes for you |
| `kno eval inspect` | Report whether an eval set can support attribution, before anything is spent |
| `kno mine` | Turn production transcripts into a weak-label eval set |
| `kno baseline` | Run your agent over your evals and score it |
| `kno value` | Measure the marginal value of each asset in a pool |
| `kno select` | Choose the assets that earn their place, under budget |
| `kno export` | Write a portfolio's selected assets to a destination |
| `kno report` | The one-page verdict across the recorded stages |
| `kno doctor` | Print what this build supports |
| `kno purge` | Delete stored agent output and judge rationales for a run |

## Verifying a download

Every release ships a `checksums.txt`, a keyless [cosign](https://docs.sigstore.dev/) signature over it, an SPDX SBOM per archive, and SLSA build provenance. There is no private signing key — so there is none to steal.

```bash
cosign verify-blob checksums.txt \
  --new-bundle-format --bundle checksums.txt.bundle \
  --certificate-identity-regexp '^https://github\.com/uknoAI/kno/\.github/workflows/release\.yml@refs/tags/.+$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

grep " kno_<version>_<os>_<arch>.tar.gz$" checksums.txt | shasum -a 256 -c -

gh attestation verify kno_<version>_<os>_<arch>.tar.gz --repo uknoAI/kno \
  --signer-workflow uknoAI/kno/.github/workflows/release.yml
```

Both identity flags are the part that matters. Without `--certificate-identity-regexp`, `verify-blob` accepts a valid signature from anyone; without `--signer-workflow`, `attestation verify` accepts an attestation from any workflow in the repository. The checksum line is written with `grep`+`shasum` rather than `sha256sum --ignore-missing` because macOS ships neither.

## Documentation

- **[The mental model](docs/mental-model.md)** — one page; read it and the rest should be obvious.
- **[What the numbers mean](docs/what-the-numbers-mean.md)** — what each number claims, and what it does not.
- **[Evaluation design](docs/evaluation-design.md)** — how to build evals that attribution can trust.
- **[ADR-0006: the `--json` contract](docs/adr/0006-the-json-contract.md)** — what every `--json` document promises a `jq` pipeline, which keys can move, and why an absent key is never a zero.
- **[Cookbook](docs/cookbook/)** — task-shaped recipes, including [data retention](https://github.com/uknoAI/kno-examples/blob/main/recipes/retention.md).
- **[DESIGN.md](DESIGN.md)** — architecture, and what is deliberately out of scope.
- **[CONTRIBUTING.md](CONTRIBUTING.md)** — how we work. Plan, adversarial review, then code.
- **[docs/debt.md](docs/debt.md)** — every piece of accepted debt, with a repayment trigger. Public on purpose.

## License

Apache-2.0.

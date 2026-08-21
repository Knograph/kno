# Kno

Kno measures the marginal value of every data asset you're considering feeding an LLM agent — then tells you which ones earn their place, what they cost, and where each belongs: context, knowledge base, or fine-tuning set.

Most teams curate agent data by intuition and dump the rest into JSONL. Kno replaces the guess with a measurement. It runs your agent against your evals, injects each candidate asset, re-runs the affected slice against untouched controls, and reports the delta with a confidence interval — ranked by improvement per dollar, validated as a portfolio against a holdout you never touched. Assets that do nothing get rejected with a reason. Failure modes nothing in your pool can fix get flagged as data you should start collecting.

Single Go binary. No infra. Works with any OpenAI-compatible endpoint, Anthropic, or your own agent behind a shell command.

> **Status: early.** Of the five stages, only `baseline` runs today. The default agent is a local fake that costs nothing, so you can see the whole loop work before pointing it at something that bills you. See [Status](#status) for what exists.

## Quickstart

```bash
go install github.com/knograph/kno/cmd/kno@latest
```

Write some Cases — one scoreable interaction per line, each with a stable `id`:

```jsonl
{"id":"refund-01","input":"How do I get a refund?","expected":"Refunds are processed within 5 business days."}
{"id":"ship-01","input":"When does my order ship?","expected":"Orders ship within 1 business day."}
```

Run it:

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

Next: `kno value` to measure which of your assets earn their place.
```

**`6 held back`** is the holdout. Nothing reads it — not this run, not valuation, not selection — until `validate`. That constraint is the reason any number Kno reports later means anything. [Why](docs/mental-model.md#why-devholdout-exists).

Full walkthrough: **[Score your agent for the first time](docs/cookbook/first-baseline.md)**.

## Three properties worth knowing up front

**Every reported delta carries a confidence interval, or it isn't reported.** A number without its uncertainty is a number that flatters you. [What the numbers mean](docs/what-the-numbers-mean.md).

**Nothing spends your money silently.** Every path that can call a provider goes through a budget guard: estimate, confirm, checkpoint. Caps are enforced *before* the call, not discovered at settlement.

**Interrupting is boring.** Work is checkpointed as each Case completes, in one transaction with its result. `--resume` skips what's finished and reconstructs prior spend from disk, so an interrupted run cannot pay twice.

**Your traces stay yours, and nothing expires on its own.** Runs are stored locally in SQLite, including the agent's output — which is conversation content if your evals come from production logs. Kno never sends content anywhere. `kno purge` deletes it when you decide to, keeping the scores and costs so the run stays resumable. [Retention, in full](docs/cookbook/retention.md).

## Exit codes

A CI gate branches on these, so they're a contract rather than an afterthought.

| Code | Meaning | What CI should do |
|---|---|---|
| `0` | Completed | Continue |
| `1` | Failed | Fail the build — something is broken |
| `2` | Stopped at a budget cap | Not a failure. The run did what you configured |
| `3` | Validation failed | Fail the build — the deploy gate (reserved for `kno validate`) |
| `4` | Interrupted by a signal or deadline | Not a failure. Resume it |

`2` and `4` both leave a resumable run. Reporting either as `1` would train people to ignore `1`, which is the code that actually means something is wrong. Recipe: **[Gate a deploy on Kno in CI](docs/cookbook/ci-gate.md)**.

## Status

| Stage | What it does | State |
|---|---|---|
| **Baseline** | Run the agent over the dev Cases, score against the Goal, persist every result | **Shipped** |
| **Value** | Route each Asset to the slices it could affect, inject, re-run against controls, record Δ with an interval | Next |
| **Select** | Build a Portfolio under budget, with a rejection log | Planned |
| **Validate** | Measure the Portfolio as a set against the untouched holdout | Planned |
| **Export** | Training set, report, and the gaps nothing in your pool could fix | Planned |

Provider adapters (OpenAI-compatible, Anthropic) arrive with **Value**. Until then `--agent fake:` is the only agent, and it costs nothing.

## Documentation

- **[The mental model](docs/mental-model.md)** — one page; read it and the rest should be obvious.
- **[What the numbers mean](docs/what-the-numbers-mean.md)** — what each number claims, and what it does not.
- **[Cookbook](docs/cookbook/)** — task-shaped recipes, including [data retention](docs/cookbook/retention.md).
- **[DESIGN.md](DESIGN.md)** — architecture, and what is deliberately out of scope.
- **[CONTRIBUTING.md](CONTRIBUTING.md)** — how we work. Plan, adversarial review, then code.
- **[docs/debt.md](docs/debt.md)** — every piece of accepted debt, with a repayment trigger. Public on purpose.

## License

Apache-2.0.

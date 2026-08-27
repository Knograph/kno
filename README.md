# Kno

Kno measures the marginal value of every data asset you're considering feeding an LLM agent — then tells you which ones earn their place, what they cost, and where each belongs: context, knowledge base, or fine-tuning set.

Most teams curate agent data by intuition and dump the rest into JSONL. Kno replaces the guess with a measurement. It runs your agent against your evals, injects each candidate asset, re-runs the affected slice against untouched controls, and reports the delta with a confidence interval — ranked by improvement per dollar, validated as a portfolio against a holdout you never touched. Assets that do nothing get rejected with a reason. Failure modes nothing in your pool can fix get flagged as data you should start collecting.

Single Go binary. No infra. Works with any OpenAI-compatible endpoint, Anthropic, or your own agent behind a shell command.

> **Status: early.** Of the five stages, only `baseline` runs today. The default agent is a local fake that costs nothing, so you can see the whole loop work before pointing it at something that bills you. See [Status](#status) for what exists.

## Install

```bash
curl -sSfL https://raw.githubusercontent.com/knograph/kno/main/install.sh | sh
```

The script picks the right build for your platform, **verifies the SHA-256 checksum**, verifies the cosign signature if you have `cosign` installed, and drops the binary on your PATH. It is short and worth reading before you pipe it anywhere.

Or take the archive yourself from [Releases](https://github.com/knograph/kno/releases) — macOS, Linux and Windows, amd64 and arm64. Or build from source:

```bash
go install github.com/knograph/kno/cmd/kno@latest
```

A `go install` binary reports the module version with no commit or date — `kno version v0.0.1`. A released binary reports both — `kno version v0.0.1 (a1b2c3d, 2026-08-26T…)`. The parenthetical is how you tell them apart in a bug report, and it is the only functional difference.

### Verifying a download

Every release ships a `checksums.txt`, a keyless [cosign](https://docs.sigstore.dev/) signature over it, an SPDX SBOM per archive, and SLSA build provenance. There is no private signing key — so there is none to steal.

```bash
cosign verify-blob checksums.txt \
  --certificate checksums.txt.pem --signature checksums.txt.sig \
  --certificate-identity-regexp '^https://github\.com/knograph/kno/\.github/workflows/release\.yml@refs/tags/.+$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

grep " kno_0.0.1_darwin_arm64.tar.gz$" checksums.txt | shasum -a 256 -c -

gh attestation verify kno_0.0.1_darwin_arm64.tar.gz --repo knograph/kno \
  --signer-workflow knograph/kno/.github/workflows/release.yml
```

Both identity flags are the part that matters. Without `--certificate-identity-regexp`, `verify-blob` accepts a valid signature from anyone; without `--signer-workflow`, `attestation verify` accepts an attestation from any workflow in the repository. The checksum line is written with `grep`+`shasum` rather than `sha256sum --ignore-missing` because macOS ships neither.

Homebrew is coming: the formula is written and the tap repository is not created yet ([docs/debt.md#73](docs/debt.md#73)).

## Quickstart

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

That run used `fake:`, the local agent that costs nothing. Pointing it at a real provider is one flag:

```bash
export OPENAI_API_KEY=sk-...
kno baseline --evals cases.jsonl --agent openai:gpt-4.1 --max-cost-usd 2.00
```

Keys come from the environment, never from a flag. Kno prices each Case from its own table, so the cap binds *before* the call rather than at settlement. `kno doctor` prints which providers, models, and goals this build supports — it contacts nothing.

Full walkthrough: **[Score your agent for the first time](docs/cookbook/first-baseline.md)**, then **[Point Kno at your own provider](docs/cookbook/your-own-provider.md)**.

## Three properties worth knowing up front

**Every reported delta carries a confidence interval, or it isn't reported.** A number without its uncertainty is a number that flatters you. [What the numbers mean](docs/what-the-numbers-mean.md).

**Nothing spends your money silently.** Every path that can call a provider goes through a budget guard: estimate, confirm, checkpoint. Caps are enforced *before* the call, not discovered at settlement.

**Interrupting is boring.** Work is checkpointed as each Case completes, in one transaction with its result. `--resume` skips what's finished and reconstructs prior spend from disk, so an interrupted run cannot pay twice.

**Your traces stay yours, and nothing expires on its own.** Runs are stored locally in SQLite, including the agent's output — which is conversation content if your evals come from production logs. Kno itself sends nothing anywhere — there is no telemetry of content, ever. Your Cases go to whatever provider you point Kno at, and that provider's retention is theirs. `kno purge` deletes it when you decide to, keeping the scores and costs so the run stays resumable. [Retention, in full](docs/cookbook/retention.md).

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

Provider adapters are **shipped**: `openai:` (and any OpenAI-compatible endpoint via `--base-url`), `anthropic:`, and `fake:`. `exec:` and `tuned:` arrive with the stages that need them.

## Documentation

- **[The mental model](docs/mental-model.md)** — one page; read it and the rest should be obvious.
- **[What the numbers mean](docs/what-the-numbers-mean.md)** — what each number claims, and what it does not.
- **[Cookbook](docs/cookbook/)** — task-shaped recipes, including [data retention](docs/cookbook/retention.md).
- **[DESIGN.md](DESIGN.md)** — architecture, and what is deliberately out of scope.
- **[CONTRIBUTING.md](CONTRIBUTING.md)** — how we work. Plan, adversarial review, then code.
- **[docs/debt.md](docs/debt.md)** — every piece of accepted debt, with a repayment trigger. Public on purpose.

## License

Apache-2.0.

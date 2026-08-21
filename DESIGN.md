# Kno — Design: Agent Data Valuation Engine (OSS)

*Project: **Kno** · binary: `kno` · org: `github.com/knograph` · the OSS engine beneath the KnoGraph platform.*

## Naming

The OSS project lives in the KnoGraph namespace deliberately: **Kno** is the engine, **KnoGraph** is the platform, and the brand relationship is legible on sight (the Terraform/HashiCorp, Temporal-OSS/Temporal-Cloud pattern). The binary is `kno`, which makes every command read as a sentence about the product's job: `kno value`, `kno select`, `kno validate` — *know the value, know what to select, know it validates*. The pun is the positioning. Checked against the CLI ecosystem: no active `kno` tool collides (nearest is `okn`, an unrelated knowledge-wiki CLI; the defunct Kno tablet company died in 2013). Before scaffolding, sweep and reserve: `github.com/knograph/kno`, `kno` on Homebrew/pypi/npm (fallbacks: `knograph-kno`, `pip install knograph`), and the `kno.dev` / `kno.sh` domains if available.

## Thesis

Teams optimizing LLM agents — through context engineering, RAG curation, or fine-tuning — face a data problem, not a diagnosis problem. Failure analysis is table stakes; promptfoo, DeepEval, Phoenix, and every platform's native tooling can tell you *that* your agent failed and roughly *why*. What nobody answers is the economic question:

> **Which data assets are actually worth including — in the context, in the knowledge base, in the fine-tuning set — to move the outcome I care about, and which are dead weight or actively harmful?**

The default practice today is dumping everything available into JSONL and hoping the tuning run sorts it out. That's expensive, it degrades results (redundant and contradictory data measurably hurt), and it teaches the team nothing about which data to keep collecting.

`kno` treats data selection as a measured portfolio decision. Point it at an agent, a pool of candidate assets, and a goal; it computes the **marginal contribution of each asset to the goal**, the **cost of carrying it** (tokens, acquisition, maintenance), decides **which mechanism it belongs in** (context, knowledge base, or tuning set), selects the optimal portfolio under budget, validates the portfolio *as a set*, and exports it in standard formats any downstream pipeline (OpenAI FT, Bedrock, axolotl, an internal trainer at Sierra or Salesforce) consumes directly. Where the pool can't cover a failure mode at any price, it says so — telling you what data to start collecting.

One-sentence positioning: **the ROI layer for agent data — every asset earns its place with a measurement, or it doesn't ship.**

Failure diagnosis still exists inside the system, but demoted: it is an internal routing mechanism that makes valuation affordable. It is not the product.

## Vocabulary

Ten words, used the same way in the CLI, the API, the SDKs, and the docs. The portfolio metaphor is deliberate and carried everywhere — this is an ROI tool, and its language should sound like one.

| Term | Meaning |
|---|---|
| **Case** | One scoreable eval interaction (input + expected outcome or rubric) |
| **Evals** | The set of cases; split **dev/holdout** at ingestion, holdout untouched until Validate |
| **Asset** | One candidate data unit — an example, document, fact, or feature — carrying its own cost vector |
| **Pool** | The collection of candidate assets under consideration |
| **Goal** | The outcome metric, with direction; composable and weighted |
| **Valuation** | An asset's measured record: Δgoal (with CI), control Δ, cost, injection mode |
| **Portfolio** | The selected subset of assets, chosen under budget, with a rejection log |
| **Destination** | Where an asset belongs: `context` \| `knowledge_base` \| `tuning_set` |
| **Bridge** | The measurement funnel that connects in-context valuations to fine-tuning outcomes |
| **Holdout** | The untouched eval slice that produces the only number you may put in a slide |

Two deliberate renames from earlier drafts, for IA reasons: *DataUnit → Asset* (a portfolio holds assets; the metaphor now closes), and the eval-case source is split out of Pool (see Core abstractions) because "the exam" and "the study material" are different things with different adapters, and conflating them made `--pool` ambiguous.

## What developers and companies actually need

| Need | Who has it | OSS answer |
|---|---|---|
| "I have 10k examples / 200 docs. Which subset should go into the tuning run or context?" | Every team past the demo phase, before every tuning run | `value` + `select` — measured marginal contribution per asset, portfolio selection under budget |
| "What's the ROI? Is this data worth its token/collection cost?" | Anyone paying for context windows, retrieval, or FT compute | Cost model per asset; rank by Δgoal per dollar, not Δgoal alone |
| "Which of this belongs in RAG vs. the fine-tuning set?" | Every team that has shipped facts into a tuning run and regretted it | Mechanism routing — every asset gets a destination, with the measurement to back it |
| "Is my selected set actually good *together*?" | Anyone who's been burned by contradictory docs | `validate` — the portfolio runs as a set, against the holdout, before export |
| "What data am I *missing*? What should we instrument or collect?" | Teams whose pool can't fix the problem | Coverage gaps → data acquisition recommendations |
| "Turn my production logs into an eval set — I don't have one" | Almost everyone (the real cold-start) | `mine` — extract cases + weak labels from transcripts and human corrections |
| "Pull candidate data from where it actually lives" | Everyone with data in Notion/Drive/warehouses | Format adapters + a first-party **MCP pool adapter** — any MCP server becomes a pool |
| "Give my FT pipeline a curated set in a format it already speaks" | Fine-tuning practitioners | `export` — validated JSONL, provenance-tagged |
| "Run it in CI, no infra" | Everyone at adoption time | Single static binary, SQLite state, budget caps, resume |
| Hosted dashboards, continuous valuation, multi-agent estates, SSO | Enterprises, later | **Out of OSS scope** — the platform |

## The core loop

Five pure-ish stages over explicit state. Purity is a hard rule: the OSS goroutine executor and the platform's durable-workflow executor (Temporal) must run the same stages.

```
             ┌──────────┐   ┌───────────┐   ┌──────────┐   ┌──────────┐   ┌──────────┐
 Evals   ──▶ │ BASELINE │──▶│   VALUE   │──▶│  SELECT  │──▶│ VALIDATE │──▶│  EXPORT  │
 Agent   ──▶ │ run+score│   │ route,    │   │ portfolio│   │ portfolio│   │ JSONL +  │
 Goal    ──▶ │ all cases│   │ marginal  │   │ under    │   │ as a SET │   │ report + │
 Pool    ──▶ │ (dev/    │   │ Δ & cost  │   │ budget   │   │ on       │   │ gaps     │
             │ holdout) │   │ per asset │   │          │   │ holdout  │   │          │
             └──────────┘   └───────────┘   └──────────┘   └──────────┘   └──────────┘
                                  ▲
                        internal: DIAGNOSE routes each
                        asset to the slices it could
                        plausibly affect (cost control)
```

**1 — Baseline.** Run the agent on the evals, score against the goal, persist every trace. The evals are split **dev/holdout at ingestion** and the holdout is never touched until Validate. This is not optional; selection without a holdout systematically inflates reported gains (winner's curse — assets that win on the dev slice win partly by luck).

**2 — Value (the differentiated step).** For each asset in the pool:

- *Route by relevance:* internal diagnosis clusters baseline failures and maps the asset to the slices it could plausibly affect. Most assets map to few slices; many map to none — "this asset is irrelevant" is a cheap, valuable early answer.
- *Route by mechanism:* classify the asset's kind — **knowledge** (facts, policies, docs) vs. **behavior** (format, tone, tool-use patterns, reasoning demonstrations) — because kinds are valued differently and belong in different destinations (see The Fine-Tuning Bridge).
- *Measure:* inject the asset, re-run the mapped dev slices **plus untouched control slices**, repeated trials (or temperature-0 with a stated caveat). Record: Δgoal on affected slices with a confidence interval, Δ on controls (regression signal), variance.
- *Cost:* every asset carries a cost vector — context tokens (recurring, per-call), fine-tuning tokens (one-time), acquisition/labeling cost (user-supplied or defaulted), and a staleness/maintenance flag. **The ranking metric is Δgoal per unit cost, not raw Δgoal.** A mediocre 200-token asset can out-rank a strong 8,000-token one. This is the ROI in "output ROI."

*Injection honesty.* "In the prompt" and "in the knowledge base behind a retriever" are different treatments. Every valuation is labeled with its mode — `context_injection` (upper bound; bypasses retrieval) or `knowledge_add` (deployment-faithful; requires an agent whose index we can write). Context-only deltas are reported as bounds, never as deployment predictions. This also preserves the `missing_data` vs `retrieval_miss` distinction, which context injection alone collapses.

**3 — Select.** Portfolio construction under budget (`max_context_tokens`, `max_training_examples`, `max_cost_usd`): greedy on Δ-per-cost with redundancy penalties (near-duplicate assets don't both get picked). Selection outputs the portfolio *and* the rejection log — every excluded asset with its reason (`no_effect`, `regression`, `redundant_with:<id>`, `cost_dominated`, `wrong_mechanism`). "Include nothing new" is a legal, first-class outcome.

**4 — Validate.** Assets were valued individually; they ship as a set. Interaction effects are real — two individually-helpful docs can be jointly contradictory. Validate runs the **entire portfolio together** against the **untouched holdout**: confirms the aggregate gain, catches pairwise poison, and produces the number you're allowed to put in a slide. If the set underperforms its parts, the report says so and flags the suspect interactions. (Terminology note: `validate` here means *holdout confirmation*, not schema validation — the help text and docs say so explicitly, because the word is overloaded in dev vocabulary and we use it anyway for its CI ergonomics: "validation failed" is exactly what a deploy gate should say.)

**5 — Export & Gaps.** Three artifacts:

- `training_set.jsonl` (OpenAI chat format) / context bundle / KB add-list — every asset provenance-tagged with its destination, measured contribution, CI, cost, and injection mode.
- `report.md` / `report.json` — the portfolio: selected (by destination), rejected (with reasons), holdout-validated aggregate gain, total cost, ROI.
- **Data acquisition recommendations** — failure clusters that *no asset in the pool* improved: "nothing in your pool addresses the entitlement-downgrade cluster (23% of failures); the human-corrected resolutions in that cluster suggest collecting X." The tool's most forward-looking output, and the one no eval framework ships.

## The Fine-Tuning Bridge

The epistemically hard problem: in-context (ICL) gains do not reliably predict fine-tuning gains. They are different mechanisms — ICL favors knowledge injection; FT favors behavior, format, and reasoning patterns — and they diverge exactly where a naive tool would mislead. `kno` closes the gap with a funnel of increasingly faithful, increasingly expensive measurements, plus one design decision that dissolves most of the problem outright.

```
  pool (all assets)
        │
        ▼
  ┌─────────────────────┐   knowledge assets ──▶ valued via context/KB injection
  │ 1. MECHANISM ROUTE  │                        (ICL measurement is faithful here)
  │    knowledge vs.    │                        destination: RAG / context — NOT the tuning set
  │    behavior (judge) │
  └─────────┬───────────┘
            │ behavior assets only (the FT-destined minority)
            ▼
  ┌─────────────────────┐   cheap, high-recall screen: per-asset ICL delta
  │ 2. ICL SCREEN       │   drops no-effect and regressive assets early
  └─────────┬───────────┘
            │ shortlist
            ▼
  ┌─────────────────────┐   LoRA on a small open model (1–8B) via FT APIs
  │ 3. PROXY-FT CONFIRM │   group-level ablations (cluster-in vs cluster-out)
  │    (group ablation) │   catches interference/forgetting before real tuning
  └─────────┬───────────┘
            │ confirmed set
            ▼
  ┌─────────────────────┐   user fine-tunes their real model, then:
  │ 4. POST-TUNE        │   kno validate --agent tuned:<model>
  │    VALIDATE         │   same untouched holdout — the final ROI number
  └─────────┬───────────┘
            ▼
  every (ICL Δ, FT Δ) pair ──▶ 5. LEARNED TRANSFER PRIOR (per kind, goal, model family)
```

**Tier 1 — Mechanism routing (v0.1; dissolves ~the majority of the gap).** Fine-tuning is a *bad* vehicle for knowledge (unreliable retention, staleness you can't patch); it's the *right* vehicle for behavior and format. ICL is the reverse. So route first: knowledge assets are valued with context/KB injection — where ICL measurement is faithful — and recommended for RAG or context, never the tuning set. Only behavior assets face the bridge at all. This is also the stronger product claim: `kno` doesn't just rank your data, it tells you **where each asset belongs**.

**Tier 2 — ICL screen (v0.1).** For behavior assets, the per-asset ICL delta remains the cheap, high-recall filter. Its job is recall, not precision: kill the no-effect and regressive assets before anything expensive runs.

**Tier 3 — Proxy fine-tuning (v0.2–0.3; the direct measurement, made affordable).** For the surviving shortlist, actually fine-tune — on a small proxy, not the production model. LoRA on a 1–8B open model with a few hundred examples runs in minutes for single-digit dollars via hosted FT APIs (Together, Fireworks, OpenAI's small tiers) — which matters architecturally: the Go engine orchestrates HTTP calls through the `Tuner` interface; **no torch in the OSS binary**. Two cost controls make it tractable:

- **Group-level ablation, not per-asset:** fine-tune cluster-in vs. cluster-out (leave-one-group-out attribution), then rank *within* a group using the Tier-2 ICL signal. For 6 behavior clusters: ~7 LoRA runs (all-in + 6 leave-one-out), not 500 per-asset runs.
- **Shortlist-only:** proxy-FT runs only on what the ICL screen passed.

Worked cost: 7 LoRA runs × ~$3–8/run (hosted, 3B model, ~500 examples, 2–3 epochs) + eval passes on the proxy ≈ **$30–80 per bridge confirmation** — comparable to the valuation run itself. Proxy→target transfer is imperfect but well-validated as a selection signal (this is how domain-reweighting methods like DoReMi operate), and it delivers what ICL categorically cannot: an **interference read** — whether tuning on a group regresses the control slices (catastrophic-forgetting risk) *before* the production model is touched.

**Tier 4 — Post-tune validation (v0.3; closes the loop).** `kno validate --agent tuned:<model>` re-scores the same untouched holdout against the user's actually-tuned model. `kno` is the eval harness on **both sides** of the tuning run; the before/after on one holdout is the final, honest ROI number.

**Tier 5 — The learned transfer prior (v0.4 / platform; the weakness becomes the moat).** Every completed loop yields a supervised pair — (ICL-measured Δ, FT-validated Δ) — per asset kind, goal type, and model family. Recorded locally, these calibrate the user's own ICL rankings after a few cycles. Aggregated across deployments as abstractions (never pooled content), they become the compounding **valuation prior**: the map of which data kinds transfer, which don't, and by how much. Collecting that dataset requires sitting on both sides of the tuning run — which Tier 4 already does — so the bridge's error term, systematically observed, becomes the proprietary asset. (Same moat shape as KnoGraph's intervention-effectiveness mapping.)

**Advanced tier — gradient-based influence (post-v0.4 / platform).** For open-weight targets, LESS-family influence estimation scores per-asset effect on validation loss from gradient features without per-candidate retraining. Real and differentiating, but white-box-only and torch-native — it ships as an optional Python sidecar (`value --method gradient`), never in the Go binary, and its noisy estimates still get confirmed by Tier 3 at the top of the ranking.

## Core abstractions

The exam and the study material are different things: `Evals` supplies cases (what we test on); `Pool` supplies assets (what we might add). Splitting them keeps `--pool` unambiguous and reflects reality — case sources (transcripts, eval files) and asset sources (docs, KBs, warehouses) are different adapters.

```go
// Agent is anything invokable on a case. Injection is a *capability*,
// not a requirement — adapters advertise what they support via
// Capabilities(), and the report labels every valuation with the mode used.
type Agent interface {
    Invoke(ctx context.Context, c Case) (Response, error)
}

// Capable is implemented by adapters (agents, pools, tuners) to declare
// what they support; the engine degrades gracefully per-adapter and the
// CLI shows the capability matrix at connect time.
type Capable interface {
    Capabilities() Capabilities // e.g. {context_inject, knowledge_write, stream}
}

type ContextInjector interface {         // upper-bound mode
    WithContext(a Asset) (Agent, error)
}
type KnowledgeInjector interface {       // deployment-faithful mode
    WithKnowledge(ctx context.Context, a Asset) (Agent, func() error, error) // returns rollback
}

// Evals supplies scoreable cases. Adapters: jsonl, csv, transcripts (via mine).
type Evals interface {
    Cases(ctx context.Context) (iter.Seq[Case], error)
}

// Pool supplies candidate assets. Adapters: jsonl, csv, parquet,
// markdown-dir, mcp (any MCP server), exec plugins (Ring 2).
type Pool interface {
    Assets(ctx context.Context) (iter.Seq[Asset], error)
}

// Goal scores an outcome. Composable, weighted. Defined as YAML + BAML
// prompt files — extending a goal is a prompt edit, never a Go plugin.
type Goal interface {
    Score(ctx context.Context, c Case, r Response) (Score, error)
}

// Tuner submits and tracks fine-tuning jobs on hosted FT APIs.
// First-party: openai, together, fireworks. Powers the bridge (Tier 3)
// and post-tune validation (Tier 4).
type Tuner interface {
    Submit(ctx context.Context, job TuningJob) (JobRef, error)
    Status(ctx context.Context, ref JobRef) (JobStatus, error)
    Model(ctx context.Context, ref JobRef) (AgentRef, error) // the tuned model, as an agent ref
}

// Asset carries its own economics and destination.
type Asset struct {
    ID          string
    Content     []byte
    Kind        Kind        // knowledge | behavior (judged; user-overridable)
    Destination Destination // context | knowledge_base | tuning_set (assigned by routing)
    Cost        CostVector  // context_tokens, ft_tokens, acquisition_usd, staleness
    Provenance  Provenance
}
```

Agent and model references use one URI-ish scheme everywhere — `openai:gpt-4.1`, `anthropic:claude-sonnet-4-6`, `openai:llama3:8b@http://localhost:8000/v1`, `tuned:<job-ref>`, `exec:kno-agent-mybot` — documented once as "agent refs," used identically in config, flags, API, and SDKs.

All supporting types (`Case`, `Score`, `Valuation`, `Portfolio`, `Report`) are defined **once, in protobuf** — schema source of truth from day one, so the later API, SDKs, plugins, and platform are codegen, not rewrites.

## Extensibility: three rings

The connector question is really three different problems, solved in three rings with different clocks. The design rule underneath all of them: **the connector is the commodity; the loop is the product.**

**Ring 0 — frozen contracts (v0.1).** `Evals`, `Pool`, `Agent`, `Goal`, `Tuner` in Go + proto, plus the `Capable` capability declaration. Everything else is downstream of these five interfaces, so they get the design attention now and a stability promise at 1.0.

**Ring 1 — first-party, in-tree, compiled in (v0.1+).** Curated adapters that ship in the binary and carry the quality bar (tests, recorded fixtures, docs):

- *Agents:* `openai-compatible` (one adapter, `base_url`-configurable — covers OpenAI, Together, Fireworks, Groq, vLLM, Ollama, and most of the long tail), `anthropic`, `http`, `shell`.
- *Pools/Evals:* jsonl, csv, parquet, markdown-dir, transcripts — and the **MCP pool adapter**: `kno` acts as an MCP *client*, so any of the hundreds of existing MCP servers (Notion, Drive, Slack, warehouses…) becomes a pool with zero Kno-specific connector code. We inherit an ecosystem instead of building one. ("Kno speaks MCP" is a launch line.)
- *Tuners:* openai, together, fireworks — the real fragmentation in the FT ecosystem, and therefore where first-party effort concentrates.

Community PRs into Ring 1 are welcome but curated; this ring is where "it just works" reputation lives.

**Ring 2 — the exec plugin protocol (community ring, any language; v0.3, experimental until 1.0).** No Go plugin machinery (`go-plugin` RPC, `.so` loading — heavy, brittle, Go-only). Instead, the pattern git, kubectl, and Docker credential-helpers proved: **any executable named `kno-pool-<name>`, `kno-agent-<name>`, `kno-tuner-<name>` on `$PATH` is a plugin.** It speaks newline-delimited JSON (or connect-rpc over stdio) matching the proto schemas, and opens with a versioned handshake:

```
$ kno-pool-notion --kno-handshake
{"protocol":"kno.plugin.v1","type":"pool","name":"notion",
 "capabilities":["assets"],"schema":"kno.v1"}
```

`kno plugins list` discovers what's on `$PATH` and prints each plugin's capability matrix. A connector can be written in Python or TypeScript in an afternoon — no Go, no SDK required (generated proto types make it nicer). The existing `shell` adapter is the primordial version of this; Ring 2 formalizes its handshake. The protocol ships marked **experimental** and says so loudly until 1.0 — communities forgive evolving protocols; they don't forgive silent breaks. The "registry" at OSS stage is a GitHub topic (`kno-plugin`) plus an `awesome-kno` index — zero infrastructure, community-ownable.

Timing rationale: Ring 2 lands at v0.3 deliberately, *after* the Ring-0 interfaces have survived contact with real users. Freezing a plugin protocol before that is how projects end up maintaining two protocols forever.

## Judge integrity

Scoring and routing use LLM judges (BAML: typed, schema-enforced, model-swappable, testable in isolation). Judges are the epistemic foundation, so they get explicit treatment: `examples/` ships a small human-labeled calibration set; `kno judge calibrate` reports judge-human agreement before you trust a run; judge model ≠ agent model is the stated default (correlated blind spots); every judge prompt lives in `judge/baml_src/` where changing it is a reviewable diff.

## Developer Experience

DX is a feature with a spec, not a vibe. The bar: **a developer should never wonder what just happened, what it cost, or what to do next.** Every command answers all three.

### CLI feel — the charm.sh stack

The CLI is the product's face, built on the charm.sh toolkit (native Go — same binary, zero extra install):

| Tool | Where it's used |
|---|---|
| **fang** | CLI framework layer over cobra: styled help, errors, `--version`, manpages, shell completion for free |
| **huh** | `kno init` — a form-driven wizard (agent endpoint → evals/pool paths → goal rubric → budgets) instead of "go edit YAML" |
| **bubbletea + bubbles** | live run dashboard: per-stage progress, spend-vs-budget meter, cases/sec, ETA, current asset under valuation |
| **lipgloss** | all styled output — the portfolio table, deltas colored by CI-crossing-zero (not by sign — honesty in the palette) |
| **glow** | `kno report` renders `report.md` beautifully in-terminal; no browser required |
| **log (charmbracelet)** | structured, leveled, human-first logging; `--json` flips to machine output for CI |
| **vhs** | every README demo is a scripted `.tape` — docs GIFs regenerate in CI and never rot |
| **gum** | used in `examples/` shell scripts so even the demo scripts feel polished |

Principles behind the polish:

- **Cost is always visible.** Any run estimated over a threshold prints the estimate and asks (huh confirm) before spending. The dashboard shows live spend against `max_cost_usd`. Nobody gets a surprise bill; `--yes` exists for CI.
- **Every command ends with "next."** `baseline` ends with "Run `kno value` to value your pool." `value` ends with the top-3 preview and "Run `kno select`." The CLI teaches the loop by using it.
- **One happy path, then control.** `kno run` executes the whole loop (baseline → value → select → validate → export) with confirmations at the spend gates — the quickstart is two commands (`kno init`, `kno run`), and the individual verbs exist for when you want the reins.
- **Interruption is boring.** Ctrl-C prints "Checkpointed at case 412/1000 — `kno value --resume` continues." Resume is the default; restart is the flag.
- **Errors follow one grammar:** what failed → why (verbatim upstream error if any) → the exact command or config line that fixes it. An unreachable agent endpoint error includes the curl to reproduce it.
- **TTY-aware.** Piped or in CI, all TUI degrades to plain deterministic lines; `--json` everywhere; exit codes are meaningful (`0` ok, `1` error, `2` budget-stopped, `3` validation-failed — so CI can gate on validation).

### CLI surface

Verbs are the loop; the help output is a mental-model diagram:

```bash
kno init                           # huh wizard → kno.yaml + goal rubric
kno mine --logs ./convos/          # cold start: transcripts + human corrections → evals
kno run                            # the whole loop, one command, spend-gated
kno baseline                       # run + score, dev/holdout split, traces to SQLite
kno value                          # route + marginal Δ + CI + cost per asset
kno select --budget-tokens 12000   # portfolio under budget; rejection log included
kno validate                       # portfolio as a SET vs holdout; the honest number
kno validate --agent tuned:<ref>   # post-fine-tune: same holdout — closes the loop
kno export --format openai-ft      # provenance-tagged JSONL + report + gaps
kno report                         # glow-rendered report in the terminal
kno judge calibrate                # judge-vs-human agreement on the calibration set
kno plugins list                   # discover kno-* executables on $PATH (v0.3)
kno serve                          # local connect-rpc API on :8080
```

Naming notes with reasons: the cold-start command is `mine`, not `bootstrap` — in a tool that computes confidence intervals, "bootstrap" collides head-on with statistical resampling, and `kno mine` says exactly what it does (mine your logs). `validate` is kept despite its schema-validation overload because "validation failed" is precisely what a CI deploy gate should print (see the Validate stage note).

One config file (`kno.yaml`), heavily commented by the wizard, every field mirrored by a flag and a `KNO_*` env var, in that precedence order. `mine` is in v0.1 deliberately: the number-one adoption killer for eval tooling is "I don't have an eval set," and production transcripts where a human corrected the agent are weak labels sitting in everyone's logs.

### API surface

Same proto, two protocols via connect-rpc (gRPC + REST/OpenAPI on one port, zero drift). Design rules:

- **Resource-shaped, not RPC-soup:** `runs`, `valuations`, `portfolios`, `reports` as resources; a valuation run is `POST /v1/runs` returning a run ID immediately (long operations are async by default; nothing blocks a connection for 40 minutes).
- **Progress is streamable:** `GET /v1/runs/{id}/events` — SSE on REST, server-stream on gRPC — emitting the same events the TUI dashboard consumes. One event schema, two renderers.
- **Idempotency keys on every mutation** — retried CI jobs must not double-spend LLM budget.
- **Errors mirror the CLI grammar** (`code`, `message`, `fix`, `docs_url`) so SDK exceptions are actionable, not opaque.
- **Dry-run everywhere:** every expensive endpoint accepts `estimate_only=true` and returns the cost estimate the CLI would have shown.

### SDKs

Generated from proto for all languages (buf/Fern); hand-polished where adoption lives. **Python first** — the fine-tuning ecosystem — and polish means notebook-native, not just typed:

```python
import kno

k = kno.connect()                                # local serve or platform URL
run = k.value(pool="./candidates", agent="openai:gpt-4.1", goal="goals/resolution.yaml")
run.watch()                                      # live progress bar in the notebook cell
port = run.select(budget_tokens=12_000)
port.to_dataframe()                              # valuations as pandas — repr shows top assets, CIs, destinations
port.rejected                                    # the rejection log, filterable
port.export("openai-ft")                         # writes JSONL + returns the path
```

TypeScript second (one SDK covers Node + TS). Go consumers import the proto or the engine itself; Rust stays generated-only until demand exists. Every SDK ships with the same three examples as the docs quickstart, so code and docs never diverge.

### Documentation

- **Quickstart** (one page): brew install → `kno init` → `kno run` on your own logs, first portfolio in under 15 minutes, mirrored by a vhs GIF at the top of the README.
- **The mental model** (one page): the vocabulary table, the five-stage loop, dev/holdout, injection modes, the bridge funnel — the page that makes everything else obvious. Most tools skip this page; it's the highest-leverage doc in the repo.
- **Cookbook:** "Curate a fine-tuning set," "Decide RAG vs. tune," "Gate deploys on `kno validate` in CI," "Bring your own judge," "Write a pool plugin in Python."
- **Honesty docs:** a page titled *What the numbers mean* — injection modes, CIs, winner's curse, proxy transfer limits. Publishing your epistemics is a trust feature and a differentiator.
- API reference generated from proto comments (buf docs) — comments in the proto are the single source, so reference rot is impossible.

## Repository layout

```
kno/
├── proto/              # THE contract: case, asset, valuation, portfolio + plugin handshake schemas
├── core/               # pipeline stages (baseline, value, select, validate, export) — pure
├── bridge/             # mechanism routing, proxy-FT orchestration (Tuner clients), transfer log
├── judge/
│   └── baml_src/       # scoring + routing prompts; calibration harness
├── adapters/
│   ├── agent/          # openai-compatible, anthropic, http, shell + injector capabilities
│   ├── evals/          # jsonl, csv, transcripts (mine)
│   ├── pool/           # jsonl, csv, parquet, markdown-dir, mcp
│   └── tuner/          # openai, together, fireworks
├── plugin/             # Ring 2: exec protocol, handshake, discovery (kno plugins list)
├── stats/              # trials, confidence intervals, dev/holdout split, redundancy detection
├── executor/           # goroutine pool + SQLite checkpointing (resume, never restart)
├── store/              # SQLite run/trace/valuation storage
├── tui/                # bubbletea dashboard, lipgloss styles, event renderers
├── cli/                # fang/cobra binary: the OSS product
├── api/                # connect-rpc (gRPC + REST from one proto definition; SSE events)
└── examples/           # toy agent + pool + calibration set; gum-polished scripts; vhs tapes
```

Dependency rule: `core` imports nothing above it. `cli`, `tui`, and `api` are thin shells over identical engine calls and one shared event stream — the open-core seam is a directory boundary, not a fork.

## Cost model (worked, because this is where trust dies)

Naive valuation is combinatorial. Worked example — 50 assets, 400-case dev set, 60 baseline failures in 6 clusters, 40 control cases, 3 trials:

- **Naive** (every asset × full dev set × 3): 50 × 400 × 3 = **60,000 agent calls** + judge calls. Dead on arrival.
- **Routed** (diagnosis maps each asset to ~1.5 clusters ≈ 15 failure cases + 40 controls, 3 trials): 50 × 55 × 3 ≈ **8,250 agent calls**, and ~30% of assets route to zero slices and cost only a routing call. Realistic total with judging: ~12–15k LLM calls ≈ **$15–40** at current mid-tier pricing.
- **Bridge confirmation** (behavior shortlist only): ~7 group-ablation LoRA runs × $3–8 ≈ **$30–80**, v0.2+.

Budgets are first-class config (`max_llm_calls`, `max_cost_usd`, `sample_rate`, `trials`); the CLI prints an estimate and confirms before any run over threshold; checkpointing means an interrupted run resumes rather than re-spends.

## Technology choices

| Choice | Why | Honest tradeoff |
|---|---|---|
| **Go** engine | I/O-bound orchestration; goroutines fit fan-out; static binary = frictionless install; largest realistic infra-OSS contributor pool | Lose Rust's compile-time state rigor — enforce pipeline invariants in tests |
| **charm.sh** CLI stack | Best-in-class terminal UX, native Go, zero extra runtime; vhs keeps docs demos honest | TUI code is a real maintenance surface — confined to `tui/`, always degradable to plain output |
| **BAML** judges | Typed LLM functions; custom judges are prompt edits; model-swappable | Codegen step for contributors; Go client newer than Python/TS — pin versions, confine to `judge/` |
| **MCP client** for pools | Inherit hundreds of existing data-source servers instead of building connectors | MCP servers vary in quality; capability detection + recorded fixtures per server |
| **Exec plugins** (Ring 2) | Any-language community connectors; battle-tested pattern (git, kubectl); no `.so`/RPC machinery | Process-per-plugin overhead; a versioned handshake to maintain forever after 1.0 |
| **SQLite** | Zero-config for CLI + CI | Platform swaps Postgres behind the `store` interface |
| **Goroutine executor** | No infra dependency; checkpoint/resume covers the real failure mode | Temporal-grade durability is platform-tier; stage purity makes the swap a registration exercise |
| **Proto-first** | One schema → Go today, SDKs + plugins + API tomorrow; docs generated from proto comments | Ceremony now, no schema-drift rewrite later |
| **Hosted FT APIs via `Tuner`** | No torch in the binary; proxy-LoRA is an HTTP call | Vendor dependency for Tier 3 — mitigated by multiple first-party Tuner implementations |

## What is deliberately NOT in the OSS project

Stated in the README — it builds trust and marks the commercial line:

- Continuous/scheduled valuation (OSS is run-based; the platform watches data value drift as the business changes)
- Hosted API, dashboards, teams, SSO/RBAC, audit trails
- **Managed, hosted connectors** — auth lifecycle, credential refresh, monitoring, SLAs. Community-built connectors (Ring 2) are encouraged and always will be; the platform sells *operation* of connectors, not their source. The commodity/product line: connectors are the commodity, the loop and its accumulated judgment are the product.
- Cross-agent / multi-vendor estate valuation
- The compounding **valuation prior** (Tier 5 at scale) — which data kinds move which goal types, learned across deployments as abstractions, never from pooled customer content. OSS measures from scratch each run (plus its own local calibration); the platform starts warm.
- Gradient-influence at platform scale (the Python sidecar ships OSS; the managed, always-on version doesn't)

The OSS engine is complete and genuinely useful standalone. The platform sells continuity, scale, and accumulated judgment — not withheld features.

## Milestones

- **v0.1 (launchable):** `init` (huh wizard), `mine`, `run`, `baseline`, `value` (mechanism + relevance routing, context-injection mode, CIs), `select`, `export`, `report` (glow), TUI dashboard, budget confirm, resume. Ring-0 contracts + Ring-1 adapters: openai-compatible, anthropic, shell agents; jsonl/csv/parquet/markdown pools. *Value prop: "it told me which 12% of my data earns its place, where each piece belongs, what it costs, and what to stop dumping into JSONL."*
- **v0.2:** `validate` (set-level + holdout ceremony hardened), knowledge-injection mode for writable KBs, redundancy detection, `judge calibrate`, `Tuner` interface + proxy-FT bridge (Tier 3) behind `--bridge`.
- **v0.3:** post-fine-tune validation (`validate --agent`), `serve` + connect-rpc + SSE events, **MCP pool adapter**, **Ring-2 exec plugin protocol (experimental) + `kno plugins list`**, OTel export, generated Python SDK.
- **v0.4:** hand-polished Python SDK, community goal/judge packs + `awesome-kno` index, Anthropic + local-model Tuner adapters, local transfer-prior calibration (Tier 5, single-tenant), gradient-influence sidecar (experimental).

## Success criteria

1. A stranger goes from `brew install` to a ranked, costed, destination-labeled portfolio on their own logs in under 15 minutes via `kno init` + `kno run` — and the vhs GIF proving it is the top of the README.
2. At least one team **shrinks** a planned fine-tuning set based on the rejection log — proof the ROI frame landed, not just the ranking.
3. At least one team moves assets from a planned tuning set into RAG (or vice versa) on the strength of mechanism routing — proof the bridge design landed.
4. The first external PR is a Ring-1 adapter or judge prompt; the first Ring-2 plugin appears within a month of the protocol shipping — proof the extension surface is where intended.
5. Someone asks "can this continuously re-value our data as the business changes?" — that email is the platform's first qualified lead, and it is the KnoGraph question asked from the data side.
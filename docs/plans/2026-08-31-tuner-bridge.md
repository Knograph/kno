# The `Tuner` interface and the proxy-FT bridge, behind `--bridge`

DESIGN's Tier 3 (DESIGN.md:134): for the surviving behavior shortlist, actually fine-tune — on a
small proxy, not the production model. LoRA on a 1–8B open model via hosted FT APIs, orchestrated
over HTTP, **no torch in the OSS binary**. The v0.2 milestone line (DESIGN.md:398) names it
`Tuner` interface + proxy-FT bridge (Tier 3) behind `--bridge`.

Fires [`docs/debt.md#83`](../debt.md#83), whose live trigger is **"the first Tuner PR"**. Does not
fire [`#78`](../debt.md#78) (its live half triggers on the first writable Destination adapter —
that is [the knowledge-injection plan](2026-08-31-knowledge-injection.md)) or
[`#79`](../debt.md#79) (triggers when `costOf` prices cache writes; nothing here touches `costOf`).
Dispositions for all three are in **Debt** below.

Every provider API fact **not settled by the Phase-1 review below** is tagged ***(verify)***. The
ones that were checked against current vendor documentation are stated plainly and carry no tag; the
rest remain unconfirmed, and the plan is written so that a wrong one costs a fixture re-record
rather than a redesign. One of them was checked and came back **refuted**, which is why this plan
grew a second spend dimension.

**Phase-1 re-reviewed 2026-08-31 — verdict: amend; amendments applied.** Three things changed, and
one of them is large. A vendor claim this plan asserted is **refuted**: Together does not auto-serve
a fine-tuned model. Reaching one over HTTP requires creating a **dedicated endpoint**, billed per
minute per running replica, idle included — so the bridge has **two** new spend shapes, not one, and
the sentence claiming otherwise designed a prime-directive-4 violation into the plan. Step 2 gains
**(f)** and **(g)**: a priced, capped, sequentially-deployed, settled-forward hosting dimension with
its own line in the consent quote, its own refusal for an unpriced serve rate, an unconditional
teardown, and a resume-time endpoint sweep that is its true-up; `Tuner` grows `Deploy`/`Teardown`;
Step 5's "no new code" economy is corrected and the "exactly one new spend shape" claim is withdrawn
*(F1, F3)*. The primary-group derivation, previously implied, is spelled out as an explicit
intersection of `AssetRouting.CaseIDs` against each `ClusterSnapshot.CaseIDs` and given an
acceptance criterion — a hidden derivation was exactly the "silently drops Cases" risk this plan
names *(F2)*. Several ***(verify)*** tags are resolved as settled fact and deleted: hosted FT
requires at least one `assistant` message per example, Together publishes per-token LoRA training
rates (roughly $0.48–$3.20 per million tokens, by model size and method), and a job-list endpoint
exists for the adopt-by-suffix path (`GET /fine_tuning/jobs`). One tag is *strengthened* rather than
removed: `Idempotency-Key` is documented for OpenAI's Agentic Commerce API and **not** confirmed for
`/fine_tuning/jobs`, with Together and Fireworks genuinely unknown — so the no-retry rule is the
primary control, not a belt over braces. Confirmed sound and not reworked: settle-at-submission
(checked against `Restore`'s real contract), the un-submittable-job window, the group-level-only
result claim, #83's disposition, the fixture story, the `--bridge` default refusal, the holdout
canary, and the event schema.

## Problem

Kno measures an Asset's contribution **in context** and reports it as an upper bound
(`docs/what-the-numbers-mean.md:106`). For `KIND_BEHAVIOR` Assets routed to
`DESTINATION_TUNING_SET`, that bound is measuring the wrong mechanism: ICL favors knowledge, FT
favors behavior and format, and they diverge exactly where the tool would mislead. Tier 1
(mechanism routing) dissolves most of the gap by never sending knowledge to the tuning set. What
remains — "does this behavior group actually transfer when you tune on it, and does tuning on it
regress everything else" — is not answerable without tuning something.

Nothing implements it. `core.Tuner` is **declared** at `core/ring0.go` (`Submit`/`Status`/`Model`),
`proto/kno/v1/tuner.proto` defines `TuningJob`/`JobRef`/`JobState`/`JobStatus` in full,
`core/types.go` aliases all three messages, `agentref.SchemeTuned = "tuned"` parses, `bridge/` and
`adapters/tuner/` each contain exactly one `doc.go`. The contract is complete and the implementation
is empty — which is the good starting position and also the trap: it is easy to write an adapter
against those signatures that spends $80 the engine cannot account for.

Because **that** is the actual problem this plan solves. A fine-tuning job is unlike every other
spend path Kno has. An `Invoke` costs fractions of a cent, is retryable, is cancellable, and settles
within seconds with a `usage` block. A `Submit` costs $3–8 (DESIGN.md:139, DESIGN.md:364), commits
the money the instant the provider accepts it, cannot be un-submitted, takes minutes to hours, and
reports its true cost — if at all — only at the end. Prime directive 4 says no spend path without
estimate + confirm + checkpoint. Applying it here is the whole design.

## Design

### Step 0 — what already exists, stated up front

Written against the actual tree, because the cheapest way to get this plan wrong is to design
types that are already there:

- **`core.Tuner` exists** (`core/ring0.go`), with exactly three methods: `Submit(ctx, *TuningJob)
  (*JobRef, error)`, `Status(ctx, *JobRef) (*JobState, error)`, `Model(ctx, *JobRef) (*AgentRef,
  error)`. Its godoc already says "Every Submit is a spend path, so it must pass the budget guard
  first." **This plan adds exactly two methods to it and changes no existing signature** — `Deploy`
  and `Teardown`, forced by Step 2(f)'s refuted vendor assumption that a tuned model is reachable the
  moment it exists *(F1)*. That is a pre-1.0 public-surface addition with a CHANGELOG note, in the
  same class as the `store` interface addition below. Everything else this plan says about `Tuner`
  is what the engine around it must do.
- **`proto/kno/v1/tuner.proto` exists and is unreferenced by any Go code outside `gen/`.**
  `JobStatus` already models the two states a naive enum would have collapsed:
  `JOB_STATUS_VALIDATING_FILES = 6` and `JOB_STATUS_DEPLOYING = 7`. `TuningJob` already carries
  `ablation_group = 7` and `estimated_cost_usd_micros = 8`, whose godoc reads *"A job is never
  submitted without one."* `JobState` already carries `actual_cost_usd_micros = 6` and an
  `optional double progress = 3` so "not reported" cannot read as 0%.
- **`agentref.SchemeTuned = "tuned"`** is in `knownSchemes` and has no adapter behind it — the ref
  grammar for Tier 4 is already parseable.
- **Clusters exist.** `value.ClusterSnapshot{Tag, CaseIDs, NDropped}` on `value.Plan.Clusters`,
  computed by `cluster()`/`snapshotClusters()` in `core/value/route.go`, persisted as the gob blob
  `Run.value_plan` (field 29), consumed by `core.ComputeGaps` with `core.MinClusterCases = 5`.
  The bridge does **not** invent a grouping.
- **The budget guard is complete for the shapes it already sees**: `Authorize` → `Reservation` →
  `Settle`/`Release`, `PreConfirm` with `permitted()` bounding, `Restore` for resume, `Overshoot`
  for observability, `addSpend` refusing negative and saturating. `store.SettledSpend` sums
  `outcomes` + `measurements` + `runs.orphan_*` in one statement (`store/sqlite.go:794`) and is the
  only durable record of money spent.
- **What does NOT exist**: any `Tuner` implementation; any training price anywhere (the `pricing`
  table's four rates are `input`, `cachedRead`, `cacheWrite`, `output` — all inference); any durable
  job record (`store` has `runs`, `outcomes`, `events`, `measurements`, `valuations`, `portfolios`,
  `gaps` at `schemaVersion = 5`, and no generic key-value or job table); `kno validate` in any form;
  a `STAGE_BRIDGE`; any `Capabilities` field describing tuning.

### Step 1 — the training file is the `tuning_set` export, and it is broken today

`core.renderTuningSet` (`core/export.go`) is the file the Tuner parses. The select/export plan
pinned it as "OpenAI chat format JSONL … this is the file the Tuner adapters will parse". Reading
it:

```go
ex := chatExample{Messages: []chatMessage{{
    Role:    "user",
    Content: string(contentOf(assets, e)),
}}}
```

**One `user` message per line and no assistant turn.** A supervised fine-tuning example with no
assistant message has no target to train on, and every hosted FT API rejects such a file at
validation time — OpenAI's fine-tuning API requires **at least one `assistant` message per
example**, confirmed, and Together's and Fireworks' supervised formats are the same shape
***(verify the latter two)*** —
which is precisely what `JOB_STATUS_VALIDATING_FILES` exists to surface. Today's artifact is
therefore not a training set; it is a list of prompts wearing the JSONL of one. This is a
pre-existing defect the bridge is the first consumer to notice, and it is fixed **in the Export
stage, in this plan's first PR**, not worked around in the Tuner:

- Two renderers for one grammar is the drift the vocabulary rule exists to stop, and it would let
  the file a user exports diverge from the file Kno trained on — the one divergence that would make
  every bridge number unreproducible.
- A behavior Asset's `content` (`Asset.content`, bytes, "passed through unmodified") is the
  demonstration. The renderer parses it as chat JSONL when it already is one (a single JSON object
  with a `messages` array), and otherwise emits it as a single `assistant` message with the
  `system` role carrying the Goal's instruction. **A behavior Asset whose content is neither is
  refused at export**, naming the Asset — never silently shipped as an untrainable line. A pool that
  cannot express a demonstration is a pool problem, and refusing is how the user finds out before
  paying $8 to be told the same thing by a provider.
- This is a behavior change with a golden-file diff, a CHANGELOG entry, and a
  `docs/cookbook/export-a-tuning-set.md` update in the same PR.

The bridge then calls the **same** `render` path with `destinationEntries` filtered to a group's
Asset IDs. Acceptance criterion 3 pins that the bytes the bridge submits for the all-in job are
byte-identical to `kno export --destination tuning_set` over the same Portfolio.

Consequence for the durable record: **the training file is never persisted.** It is Asset content —
customer data — and `store.Purge` covers `outcomes`, not a job table. Only its SHA-256 is stored.
Resume re-renders it from the Portfolio and the pool, which the export determinism goldens already
guarantee is byte-identical.

### Step 2 — money, which is the hard part

Seven questions, answered in order.

**(a) Is a job's cost knowable before submission?** Yes, to a bound, and only after a price table
exists for it. Hosted FT is priced per training token times epochs, and Together publishes exactly
that shape for LoRA: a per-token training rate varying by base-model size and method, roughly
**$0.48 to $3.20 per million training tokens**. The per-provider remainder is still
***(verify)*** — whether a per-job floor applies, and whether epochs multiply the published rate or
are already folded into it — but the *shape* the estimate assumes is confirmed, which is the part a
wrong answer would have cost a redesign. Training tokens
are computable locally from the rendered file with the machinery already in `pricing`:
`pricing.CountTokens(sizeBytes, model)` over `bytesPerToken = 2.0`, `safetyMargin = 1.5`,
`perMTok()` rounding up and saturating. So:

- `pricing` gains a **training** dimension: `TrainPrice(scheme, baseModel) (*knov1.Price, bool)`
  keyed the same `table[scheme][model]` way, carrying a train-per-Mtok rate and an optional
  per-job floor. It is a **separate lookup from `Lookup`**, not a fifth field on the inference
  `Price`, because a model can be trainable and not servable and vice versa, and because a nil rate
  on the existing message already means "not billed separately" rather than free.
- An **unpriced base model is refused.** There is no `--accept-unknown-cost` escape for the bridge.
  That flag is defensible for a $0.0004 call whose cap is enforced by the call ceiling; it is not
  defensible for an $8 irreversible commitment where the estimate *is* the only control. The escape
  hatch is the explicit one, mirroring `--price-input-per-mtok`: `--price-train-per-mtok`, which
  must be passed, is recorded on the run, and is echoed in the consent quote as user-supplied.
- The estimate is **pessimistic by construction**, per the doctrine `core.Estimator`'s godoc already
  states for inference ("The estimate must be PESSIMISTIC — the most a call could cost … a bound
  that can be too low is not a bound"): `ceil(train_tokens × safetyMargin) × epochs × rate`, plus
  the floor, plus a provider-class headroom multiplier ***(verify — providers bill packed sequences,
  padding, and a validation split; the multiplier is a documented constant per provider, like
  `pricing.RegionalMultiplierPct`'s +10%, and the pricing drift detector's scope extends to it)***.

**(b) The moment money becomes unrecoverable is `Submit`'s response, not the job's completion.**
Therefore the guard settles there. The rule, in order, and it is the plan's load-bearing sequence:

1. `est := tuner.EstimateJob(job)` — local arithmetic, no network, exactly as `core.Estimator`
   requires of its inference twin. Written into `TuningJob.estimated_cost_usd_micros`.
2. `res, err := guard.Authorize(ctx, budget.Estimate{Calls: 1, CostUSDMicros: est, Tokens:
   trainTokens})`; `defer res.Release()`. Refused ⇒ nothing is submitted, run stops
   `RUN_STATUS_BUDGET_STOPPED`, exit 2, resumable.
3. **Write the durable job row `state = submitting`, with its estimate in its cost columns, and
   fsync it, BEFORE the HTTP request leaves.** Write-ahead intent. A crash between the write and the
   provider's response must leave evidence.
4. `ref, err := tuner.Submit(ctx, job)`.
5. On success: `res.Settle(budget.Spend{Calls: 1, CostUSDMicros: est, Tokens: trainTokens})` and
   update the row to `submitted` with the `JobRef`. **The estimate is settled now, at submission,
   not at completion.** A reservation is in-memory and dies with the process; settled spend is
   durable through `SettledSpend`. Holding a reservation across a job that runs for forty minutes
   would mean a killed run loses the accounting entirely and a resume authorizes the same $8 again —
   verbatim the failure `Guard.Restore`'s godoc describes ("up to twice the intended spend across
   one kill/resume cycle").

**(c) What if the real cost exceeds the estimate?** You cannot un-submit, so the only honest move is
to record it. At terminal status, `JobState.actual_cost_usd_micros` is compared to the estimate:

- `actual > estimate`: the difference is spend nobody authorized. It cannot go through `Settle` —
  the reservation is closed and `once` makes a second `Settle` a no-op — so it goes through the path
  built for exactly this shape: `store.RecordOrphanSpend(ctx, runID, budget.Spend{CostUSDMicros:
  delta})` plus `guard.Restore(...)` so the in-process guard stops authorizing against money it
  does not have, plus a `SettlementOvershoot` event. `Guard.Overshoot()` then reports it and the
  report prints it. `RecordOrphanSpend`'s godoc is written in Case terms but its contract is
  run-scoped ("The amount is recorded against the RUN, so this method cannot say which Case it
  belonged to"), and its column is already in `SettledSpend`'s sum — so the true-up survives a
  resume for free. The godoc is amended in this PR to say "spend the guard did not authorize",
  which is the general statement it was always making.
- `actual < estimate`: **no refund.** `addSpend` refuses a negative report by design, and crediting
  back would let a run spend past what a human consented to on the strength of a cheaper-than-feared
  invoice. The over-estimate is reported ("estimated $6.00, billed $4.10") on the run and in the
  report, and is the raw material for calibrating the multiplier in (a). Accepted risk: **a bridge
  run's recorded spend is an upper bound**, and `kno report` says so in those words.
- `actual` absent or zero: several providers do not return a per-job cost ***(verify per
  provider)***. The estimate **stands** as the recorded spend and every rendering labels it
  `estimated`, never `billed`. Never guessed, never zeroed — a zero would credit the run by the
  whole estimate at reconciliation time.

**(d) What must a killed-and-resumed run never re-pay for?** Any group whose job row exists. The
resume rules:

- Row `submitted`/terminal: the estimate is already inside `SettledSpend`; the group is **not
  re-submitted**. If not terminal, the run resumes **polling** the recorded `JobRef`.
- Row `submitting` (crash inside the request window): **never re-submitted blind.** The adapter
  lists the provider's jobs and adopts one whose model-name `suffix` matches — `TuningJob.suffix`'s
  godoc already says it exists "for traceability back to the run", so the suffix carries
  `kno-<run_id>-<group>` and is the adoption key. OpenAI exposes the job-list endpoint this path
  requires (`GET /fine_tuning/jobs`), so the mechanism is confirmed implementable at least once;
  ***(verify Together and Fireworks expose an equivalent list endpoint and honor a caller-supplied
  suffix)***. No match ⇒ the row is closed `abandoned`, its
  estimate stays settled, and an `OrphanSpend` event records that money may have been spent on a job
  Kno cannot see. Conservative in the only safe direction.
- **A submit POST is never retried on a transport-transient error.** This is a deliberate divergence
  from `errs.ErrTransportTransient`'s usual "back off and retry" handling, and it must be stated
  where a reader will find it: a retried submit against a provider without idempotency is a second
  $8 job. The hedge here got **weaker** on review, not stronger: OpenAI documents `Idempotency-Key`
  for its Agentic Commerce API and **not** for `/fine_tuning/jobs`, and Together's and Fireworks'
  support is genuinely unknown ***(verify all three)***. Where a provider does support one, a key
  derived from `sha256(run_id | ablation_group | training_file_sha256)` is sent and a retry is safe.
  Where it does not — which is the assumption the code is written under — the adapter classifies the
  failure and stops, and the `submitting` row plus the adopt-by-suffix path is the recovery. **The
  no-retry rule is therefore the primary control, not a belt over braces**, and no code path may
  weaken it on the strength of an unconfirmed key.
- Resume of the **eval passes** on a tuned model is ordinary: they are `RecordMeasurement` rows
  keyed `(run_id, asset_id, case_id, arm, trial)` like any other measurement.

**(e) How is a submitted-but-unsettled job recorded?** As **settled spend at its estimate, in a
durable row, from before the request left**. There is no such thing as an unsettled job in this
design, and that is the point: the window in which money is spent and unrecorded is reduced to the
few milliseconds between the fsync and the socket write, and even that window leaves a `submitting`
row behind. The row is the ledger; the reconciliation at terminal status is a true-up, never the
first record.

**(f) Serving the tuned models is a SECOND spend shape, and this plan previously denied it.** *(F1)*
Together does **not** auto-serve a fine-tuned model. Its documentation
(`docs.together.ai/docs/deploying-a-fine-tuned-model`) is explicit: a finished job produces weights,
and reaching them over HTTP requires creating a **dedicated endpoint**, which bills **per minute per
running replica, including while idle**. The only alternative it offers is a free local download,
which is useless to an HTTP-only orchestrator that ships no inference runtime by design. So
`Tuner.Model` returning an `AgentRef` is not a lookup — it names a model, it does not promise the
model is reachable — and behind it sits a deploy → wait-for-ready → serve → tear-down lifecycle with
the eval passes running inside it.

This is the plan's largest correction, because the sentence "the bridge introduces exactly one new
spend shape, `Submit`" was **false**, and the falsehood was load-bearing: N+1 dedicated endpoints,
live for the duration of every eval pass across 8 groups, at a per-minute rate `Guard` had never
heard of, uncapped and invisible. Hosting therefore gets everything `Submit` gets, adapted to the
one way it genuinely differs — hosting accrues **incrementally and can be stopped**, where `Submit`
commits at once and cannot:

- **`Tuner` grows two methods**, additively: `Deploy(ctx, *JobRef) (*Endpoint, error)` and
  `Teardown(ctx, *Endpoint) error`. `Endpoint` is a Go struct in `core` — id, provider, served model,
  replica count, `ReadyAt` — not a proto message, matching the `TuningJobRecord` reasoning below.
  A Tuner whose provider genuinely does auto-serve implements `Deploy` as a no-op returning a
  zero-rate `Endpoint`, which is how the interface stays honest for a provider Together is not.
- **Priced, or refused.** `pricing` gains `ServePrice(scheme, servedModel) (*knov1.Price, bool)`
  carrying a per-minute rate and a replica count, keyed the same `table[scheme][model]` way as
  `TrainPrice`. **An unpriced serve rate is refused exactly as an unpriced train rate is**, with
  `--price-serve-per-minute` as the same explicit escape and no `--accept-unknown-cost` path. An
  endpoint whose rate is unknown is an unbounded meter, which is worse than an unpriced job.
- **Capped and serialized.** `--bridge-max-live-endpoints` defaults to **1**: deploy one model, run
  that model's eval passes, tear it down, then deploy the next. N+1 endpoints are never live at once
  by default, which converts an uncapped N+1-way idle bill into a bounded sequential one and trades
  wall-clock for money deliberately. `--bridge-max-serve-minutes` (default 30, **per endpoint**) is a
  hard ceiling: reaching it **tears the endpoint down** and reports that group `unknown` rather than
  continuing to bill for a measurement that is already late.
- **Settled forward, per minute.** A ticker settles one minute's rate at a time into the durable job
  row — `serve_minutes` and `serve_cost_usd_micros` columns, updated on each tick — through the same
  `Authorize`/`Settle` pair. This is deliberately **not** settle-at-submission: hosting is stoppable,
  so holding a pessimistic reservation across it would refuse work the user could afford, and a
  per-minute settle loses at most one minute of accounting to a crash. Reaching the budget cap
  mid-serve tears the endpoint down; it does not merely stop the next job.
- **Teardown is unconditional and `defer`-shaped.** It runs on success, on eval-pass failure, on cap,
  on timeout, and on `Ctrl-C`. A **failed teardown is never swallowed**: it emits
  `TuningEndpointChanged` with state `leaked`, fails the run, and the error names the provider's
  endpoint id and console page — because a leaked endpoint bills after Kno has exited, which is the
  one failure in this plan that keeps costing money with nobody watching.

**(g) Hosting overshoot has a true-up, and it is the sweep.** *(F3)* Step 2(c) reconciles `Submit`
cost only, and hosting can overshoot in exactly one way: wall-clock minutes billed beyond the last
settled tick, which is what a crash or a failed teardown leaves behind. So a resumed bridge run's
**first** act, before any deploy and before any submit, is to list the provider's endpoints and
adopt-or-destroy every one whose name carries this run's `suffix`. Minutes accrued between the last
settled tick and the observed teardown are recorded through `store.RecordOrphanSpend` plus
`guard.Restore` with a `SettlementOvershoot` event — the same mechanism (c) uses, applied to the
second dimension. An endpoint the provider no longer lists is settled at `--bridge-max-serve-minutes`
rather than at the last tick, which is the conservative direction. `kno doctor` reports any
`tuning_jobs` row carrying an endpoint id with no teardown timestamp, so a leak is visible rather
than resident.

The store gains, additively (the precedent is the select/export plan's `WritePortfolio`/`Portfolio`
— a pre-1.0-permitted interface break with a CHANGELOG migration note and all fakes updated):

```
WriteTuningJob(ctx, runID string, j *TuningJobRecord) error   // insert or replace, keyed (run_id, ablation_group)
UpdateTuningJob(ctx, runID string, j *TuningJobRecord) error
TuningJobs(ctx, runID string) ([]*TuningJobRecord, error)
```

`TuningJobRecord` is a **Go struct in `store`**, like `store.Outcome` and `store.Measurement` — not
a proto blob — so its cost columns are real columns and `SettledSpend` can sum them in the same
statement as the other three sources. Fields: `AblationGroup`, `State`, `Provider`, `ProviderJobID`,
`BaseModel`, `Suffix`, `TrainingFileSHA256`, `TrainTokens`, `Epochs`, `LoRARank`,
`EstimatedCostUSDMicros`, `ActualCostUSDMicros` (nullable — absent is not zero), `Status`
(`JobStatus`), `SubmittedAt`, `TerminalAt`, `ErrorText`, and the hosting dimension from (f):
`EndpointID` (nullable), `DeployedAt`, `TornDownAt` (nullable — non-null-`EndpointID`-with-null-
`TornDownAt` is the leak `kno doctor` reports), `ServeMinutes`, `ServeCostUSDMicros`. **No training
data, no Asset content, no credential.** `schemaVersion` goes 5 → 6 with a `tuning_jobs` table;
`SettledSpend`'s SQL gains **two** further `COALESCE` terms, one per spend dimension, so a fresh
process reads back both.

### Step 3 — group ablation: what the groups are, and what the result is a claim about

**The groups are the failure clusters that already exist.** `value.Plan.Clusters` — normalized tags,
deduplicated, with `NDropped` accounting — read back from `Run.value_plan` on the Value run the
Portfolio names (`Portfolio.source_run_id`). Not new clustering, not embeddings, not a judge call.
DESIGN's worked example says "6 behavior clusters"; the engine already produces tag-keyed clusters
and already has an opinion (`MinClusterCases = 5`) about when one is too small to measure.

**Membership is exclusive, and this is the sharpest correctness point in the design.** An Asset
routed to several clusters is assigned to exactly **one** — its primary group: most routed Cases,
tie-break by tag then Asset ID, deterministic.

**How "most routed Cases" is actually computed, spelled out because it was implied.** *(F2)*
`cluster()` (`core/value/route.go`) puts a multi-tagged Case into **every** matching cluster, so the
overlap this section warns about is real and not hypothetical. But `AssetRouting.CaseIDs` on
`value.Plan.Routed` carries no per-cluster tag — it is a flat list of the Cases an Asset was routed
to — so the count is not read off any field. It is an **intersection**, and leaving that unstated is
exactly the hidden gap that silently drops Cases:

> For Asset `a` and cluster `c`, `n(a, c) = |routed(a).CaseIDs ∩ c.CaseIDs|`, where both operands are
> the persisted `value.Plan` values read back from `Run.value_plan`. The primary group is
> `argmax_c n(a, c)` over clusters with `n(a, c) > 0`; ties break by cluster tag, then by Asset ID.
> An Asset with `n(a, c) = 0` for every `c` has no primary group and is reported `unknown`.

`ClusterSnapshot.NDropped` is **not** part of the count — dropped Cases were never routed, so
including them would let an Asset's primary group be decided by Cases it was never measured on.
Acceptance criterion 28 pins the derivation, including the case where an Asset's larger cluster is
the one it shares fewer routed Cases with. Leave-one-group-out does not decompose under
overlapping membership: an Asset in both A and B is still present in the "leave out A" training set
via B, so the A-LOO delta measures B's contribution and attributes it to A. Overlap would make every
number quietly wrong in a way no test that only checks arithmetic would catch. An Asset routed to
zero clusters cannot be group-ablated at all and is reported `unknown` — never folded into a group,
never given a bridge verdict it did not earn.

**The population.** Only Assets the Select stage placed in `DESTINATION_TUNING_SET`. Per
`core.destinationFor` (`core/select.go:573`) that is `KIND_BEHAVIOR` plus anything the pool pinned
by hand. The bridge **refuses** a Portfolio whose tuning-set entries include a `KIND_KNOWLEDGE`
Asset that was not user-overridden — Tier 1's whole claim is that knowledge never faces the bridge,
and paying $8 to fine-tune on a routing bug is not a measurement.

**The jobs.** 1 all-in + N leave-one-out = N+1, matching DESIGN.md:136 ("~7 LoRA runs (all-in + 6
leave-one-out), not 500 per-asset runs"). `--bridge-max-groups` defaults to 6. Beyond the cap the
run is **refused**, not merged: a merged group's LOO delta is uninterpretable, and the fix line
("raise --bridge-max-groups; each group adds one fine-tuning job") states the price of the answer.
A group below `MinClusterCases` is **skipped and reported**, not tuned — the measurement it would
buy is already known to be underpowered.

**What the result attributes, and to what.** The LOO measurement produces, per group:

- `Δ_group = score(all-in model) − score(leave-group-out model)` over the **dev** Cases of that
  group's cluster, paired per Case, with a confidence interval from `stats/interval` — the same
  machinery, the same `Sidedness` and `method` labelling, the same multiplicity discipline the
  Select stage applies (Bonferroni over N groups, not over Assets — N is small, which is the one
  place this design is statistically comfortable).
- An **interference read**: `Δ_control` over the control partition (`Plan.ControlCaseIDs`), the same
  reserved slice the harm test already uses. This is the thing DESIGN says ICL "categorically
  cannot" give (DESIGN.md:139), and it is gated exactly like Value's regression verdict: a group is
  never accused of interference on an underpowered control.

**It does not produce a per-Asset fine-tuning delta, and must never appear to.** DESIGN's own
answer is "then rank *within* a group using the Tier-2 ICL signal." So an Asset's ORDER stays the
ICL `delta_per_cost` the Select stage computed; the bridge supplies a **gate**: a group whose
`Δ_group` interval crosses zero is unconfirmed, and its Assets carry a rejection reason naming that.
`WRONG_MECHANISM` is the wrong reuse — it means "routed to the wrong destination", which is a claim
about routing, not about a measured failure to transfer. So: a new additive enum value
`REJECTION_REASON_BRIDGE_UNCONFIRMED = 10`. Adding it is a schema change with a docs obligation
(`what-the-numbers-mean.md` gains a section), counted here as the addition it is.

A test asserts that no `Valuation` written by a bridge run carries a per-Asset `delta_goal` sourced
from the bridge, and that no rendering prints a per-Asset number labelled fine-tuning-measured.

### Step 4 — `--bridge` gating, the refusal, and learning the price first

**Off by default, and behind a flag rather than a config default**, because every other spend path
in Kno is bounded by per-call estimates in fractions of a cent, is retryable, and is cancellable.
The bridge is a small number of large irreversible commitments. A default-on bridge would mean a
single `y` at the existing prompt authorizes $80.

Surface: `kno bridge --select-run-id <id> --pool <p> --bridge --tuner together:<base-model>`.
`--bridge` is the arming flag; without it the command **plans and prints and submits nothing**.
`kno.yaml` gains a `bridge:` block whose `enabled` key defaults to false and which cannot be the
only thing that arms it — the flag must also be present. Two independent gates, because a committed
config file is a thing people copy.

The default refusal is `errs.ErrConfirmationRequired` (`Code: "CONFIRMATION_REQUIRED"`, `Message:
"the action was not confirmed, so nothing was changed"`, exit 1) — the sentinel whose godoc already
explains why it is non-zero ("A scheduled job that forgets the confirmation flag would otherwise
exit 0, log success, and have done nothing"). Its `Fix`:

> re-run with --bridge to submit; without it this printed the plan and spent nothing. 8 fine-tuning
> jobs on together:meta-llama/Llama-3-8b, about $47.20, plus up to $12.00 to host each tuned model
> while its eval passes run — about $59.20 total. Each job is charged when it is submitted and
> cannot be un-submitted; hosting is charged per minute per endpoint, including while idle.

**How the user learns the cost before enabling.** The un-armed run *is* the estimator. It reads the
Portfolio, forms the groups, renders every training file, counts tokens, prices them against both
the training and the serving rate, and prints the per-job table, the hosting line, and the total —
over the network zero times, spending zero. That is the sentence the
refusal points at, and acceptance criterion 12 makes it testable: an un-armed bridge run with a
transport that fails on any dial still exits 0 and prints the plan.

**Confirmation when armed.** One `Guard.PreConfirm` over the whole bridge — every job plus the proxy
eval passes — through the existing `consentDialog` (`cli/consent.go:96`) and `confirmFunc`
(`cli/render.go:60`), unchanged. `confirmThresholdUSD = 1.00` and a bridge is $30–80 in
training alone, so it always asks. Three additions to the quote, all because the existing sentence
("This run would spend about $47.20 (uncapped).") is true and insufficient for this shape:

- the job list, one line each, so the number is decomposable;
- a separately-labelled **hosting** line — `N+1 endpoints × --bridge-max-serve-minutes × rate` —
  stated as a cap-bounded worst case rather than a prediction, because unlike `Submit` it is
  stoppable and usually costs less *(F1)*. The quote's total is the sum of both dimensions plus the
  eval-pass inference estimate;
- one sentence of irreversibility, now covering both shapes: *"Each job is charged when it is
  submitted. A cap reached mid-bridge stops the next job and tears down any live endpoint; it does
  not refund a job already sent."*

`--yes` skips the prompt as everywhere else. It does **not** substitute for `--bridge`: one is
consent to spend, the other is consent to use this mechanism at all.

### Step 5 — which adapter ships first: Together

`adapters/tuner/together`, then `adapters/tuner/fireworks`, then `adapters/tuner/openai` (DESIGN
puts additional Tuners at v0.4). The argument, weakest-to-strongest:

1. DESIGN.md:134 names Together first in "Together, Fireworks, OpenAI's small tiers".
2. `TuningJob.lora_rank` is a field this plan must populate or delete. Together exposes LoRA rank as
   a first-class training parameter ***(verify)***; OpenAI's fine-tuning tiers do not expose a LoRA
   rank at all ***(verify)***. Shipping OpenAI first would leave the proto field unpopulated by its
   first and only implementation — the shape debt #4 caught elsewhere, a contract describing behavior
   the code does not have.
3. The proxy DESIGN describes is "a 1–8B **open** model" (Llama-3-8B, Qwen). OpenAI's FT-able models
   are neither open nor in that class, so an OpenAI-first bridge would measure transfer to a
   different model family than the one the design argues about.
4. Together publishes a per-token training rate — **confirmed**, per base-model size and method,
   roughly $0.48–$3.20 per million training tokens — which is what makes the local pessimistic
   estimate in Step 2(a) possible at all. A quote-on-request provider could not be estimated locally
   and would therefore fail Step 2(a)'s unpriced-model refusal rather than silently guess.
5. Fireworks second, deliberately and soon: one Tuner cannot tell you whether the interface is
   general or is Together's shape wearing an interface. The bedrock/vertex plan made the same
   argument for two adapters in one plan; here they are sequential because the second one's value is
   as a falsifier, and a falsifier is worth more after the first has shipped.

Counter-argument, taken seriously: the `tuning_set` artifact is already OpenAI chat JSONL, so
OpenAI-first gets the file format for free. Rejected — Together accepts the same OpenAI chat JSONL
***(verify)***, so the format is free either way, and the expensive half (LoRA on a 1–8B open model
for single-digit dollars) is not.

Adapter posture, ported wholesale rather than reinvented: `New(opts Options) (*Tuner, error)` with
plain Go types (`internal/transport` is internal to `adapters/agent`, so the same rule that keeps
`anthropic.Options` transport-free applies); credentials env-only through
`transport.ParseKeyBindings` / `KeyBindings.Resolve` with `--key-env host=VAR`, default
`TOGETHER_API_KEY` bound to the default host and **to nothing else**; `endpointsec`-equivalent
redirect refusal, plain-HTTP refusal, private-address refusal with the same opt-outs; a
`TOGETHER_AUTH` `errs.Actionable` sentinel modeled on `anthropic.ErrAuthentication`, naming the
variable, stating that no file, profile, or metadata service is read; response size caps; no headers
in any log or error.

**The tuned model is invoked as an ordinary Agent — once it has been deployed.** *(F1)*
`Tuner.Model` returns an `AgentRef`, and a Together dedicated endpoint speaks the OpenAI-compatible
API ***(verify the served route's exact shape)***, so once Step 2(f)'s `Deploy` has returned a ready
`Endpoint`, the eval passes go through `adapters/agent/openaicompat` and the existing
`Estimator`/`Guard` path with no new **inference** code.

What this plan previously concluded from that — **"the bridge introduces exactly one new spend
shape, `Submit`"** — is **withdrawn as false**. Together does not auto-serve a fine-tuned model;
reaching one over HTTP requires a dedicated endpoint billed per minute per replica, idle included.
The bridge introduces **two** new spend shapes, `Submit` and `Serve`, and Step 2(f) prices, caps,
quotes, and settles both. A serverless or shared-inference route for LoRA adapters would collapse
the second one back to zero and is worth checking before implementation begins — but this plan does
**not** assert one exists ***(verify: no serverless LoRA-adapter serving path was confirmed for
Together at the time of this review, and the design must not be built assuming one)***. The economy
that survives is narrower and still real: no new inference code and no new measurement code, only a
new lifecycle around them.

### Step 6 — the fixture story when a fixture costs $8

The repo's fixture discipline (`adapters/agent/anthropic/testdata/fixtures/README.md`: an
allowlist of `case.txt`, `request.json`, `response.json`, `status`, `note.txt`; **no headers
recorded in either direction**, enforced by `TestFixturesCarryNothingTheyShouldNot`) assumes a
fixture costs a fraction of a cent. Four changes, each with its reason:

1. **Hand-authored first, from the published spec**, exactly as the bedrock/vertex plan did. A
   `TuningJob` request body and a `JobState` response are small documents; getting them from the
   spec and marking them ***(verify)*** is honest and costs nothing.
2. **The allowlist changes shape**, and the README changes with it. A tuning fixture's input is a
   file, not a Case: `case.txt` is replaced by `training_data.jsonl`, a synthetic 3-example file
   checked in beside the fixture. The allowlist test is extended rather than relaxed.
3. **A new fixture kind: the poll sequence.** `poll-01.json` … `poll-NN.json`, replayed in order, so
   the state machine `VALIDATING_FILES → QUEUED → RUNNING → DEPLOYING → SUCCEEDED` is deterministic
   and every branch — including `FAILED` with provider error text, and `SUCCEEDED` with no
   `actual_cost_usd_micros` — has a fixture. No existing fixture has this shape and it is the one
   the bridge actually needs.
4. **Live recording is once, manual, and double-gated.** `make record-fixtures` already refuses
   without `KNO_MAX_COST_USD` (`live_spend_guard`) and the tuner's `TestRecord` additionally
   refuses without `KNO_ALLOW_TUNING_SPEND=1`, because that guard was written for cents and this is
   dollars. One real job is recorded, checked in, and **not** re-recorded on a schedule.

**The bridge is excluded from the nightly live matrix, deliberately.** CLAUDE.md permits nightly
live-API tests "with a capped budget"; a nightly fine-tune is $3–8 × 365 ≈ $1,100–2,900/year for an
OSS repo, which is not a capped budget, it is a subscription. The exclusion is an accepted risk with
a named alternative: a manual re-record before each minor release, in the release runbook, in the
same list as the ledger review.

### Step 7 — the holdout, and `validate`

**A bridged result is not validated on the holdout, and that is the design, not a gap.** DESIGN
separates Tier 3 (proxy confirm, v0.2) from Tier 4 (post-tune validation, `kno validate --agent
tuned:<ref>`, v0.3). The proxy eval passes run on the proxy model over the **dev** slice, through
`core.Seal` like every stage before Validate. Opening the holdout for a model the user will never
deploy spends the holdout — the one resource that cannot be replenished — on a proxy.

It is also not implementable: **`kno validate` does not exist in any form.** `STAGE_VALIDATE = 4`
is in `run.proto`, `errs.ErrHoldoutSealed` is defined and returned by no production path, `Seal`'s
godoc says "Validate reads the holdout through a separate path that does not exist yet —
deliberately", and `cli/status.go:88` declares the stage `stagePlanned`/`v0.2`. This plan does not
build it and does not depend on it.

`Tuner.Model` returning an `AgentRef` is precisely the seam Tier 4 will use, and
`agentref.SchemeTuned` already parses it. Building the interface for a stage that does not exist is
correct here because the interface is already written; wiring the stage is not.

Canary: a bridge run over Evals containing a distinctively-tagged holdout Case must never see it,
in the shape of `TestValueNeverTouchesTheHoldout` (`core/value_loop_test.go:281`) and
`TestSelectHoldoutCanary` (`core/select_test.go:656`).

### Step 8 — stage, events, and the spine

- **`STAGE_BRIDGE = 6`** on `run.proto`'s `Stage` enum, additive. Not folded into `STAGE_VALUE`: a
  bridge run has its own budget, its own resume semantics (adopt-don't-resubmit), and its own
  terminal states, and reusing `STAGE_VALUE` would make `kno value --resume` adopt tuning jobs.
- Three additive `oneof payload` members on `event.proto`, continuing from 25:
  `TuningJobSubmitted tuning_job_submitted = 26` (group, provider, job id, base model,
  `estimated_cost_usd_micros`, train tokens — **no training data, no Asset content**),
  `TuningJobStateChanged tuning_job_state_changed = 27` (job id, `JobStatus`, optional progress,
  optional `actual_cost_usd_micros`, provider error text), `BridgeGroupMeasured
  bridge_group_measured = 28` (group, `Δ_group` + `Interval`, `Δ_control` + `Interval`,
  `control_underpowered`, verdict), and — from Step 2(f) — `TuningEndpointChanged
  tuning_endpoint_changed = 29` (job id, endpoint id, state `deploying`/`ready`/`torn_down`/`leaked`,
  accrued serve minutes, accrued serve cost, provider error text; **no endpoint URL that could carry
  a token, no credential**). Per CLAUDE.md: new user-visible state is a new event type, never a side
  channel; the TUI renders these, the CLI prints them, `--json` serializes them. A leaked endpoint is
  user-visible state with money attached, so it gets an event rather than a log line.
- `SettlementOvershoot` and `OrphanSpend` are **reused**, not duplicated, for (c) and the abandoned
  job. Both already exist and both already mean what is needed.

### Debt

- **[`#83`](../debt.md#83) — FIRES. Disposition: repay.** Its live trigger is "the first Tuner PR",
  re-dated on 2026-08-29 specifically so it could not self-satisfy, and its own text names the
  repayment: "The landing PR adds the reader with the standard dependency justification." The
  parquet pool reader lands in this plan's PR series, pure-Go, no cgo, with the dependency
  justification in the PR body (what it does, why stdlib cannot, license, maintenance signal). It is
  an independent workstream — `adapters/pool/parquet`, blocking nothing — and it is genuinely
  motivated here: the entry's own reasoning is that "tuning sets are the realistic source" of
  parquet, and this is the plan that consumes tuning sets. A second re-dating is not available: the
  entry has been re-dated once already, and the ledger rules permit repay, re-date-with-a-written-
  reason, or promotion to won't-fix with an ADR — and a re-date whose reason is "we did the thing
  the trigger names but not the payment" is the carryover the rules forbid.
- **[`#78`](../debt.md#78) — does NOT fire.** Its live half's trigger is "when the first writable
  Destination adapter lands (v0.2 knowledge injection)". Nothing in this plan writes a Destination;
  Export writes a local file and the Tuner reads it. Stated here so the record shows it was read and
  considered rather than missed. Its disposition belongs to
  [the knowledge-injection plan](2026-08-31-knowledge-injection.md).
- **[`#79`](../debt.md#79) — does NOT fire.** Its trigger is "when `costOf` prices cache writes".
  This plan does not touch `costOf`, does not price cache writes, and does not change the per-Case
  cost path. It does introduce a **second** unmodeled pricing dimension — training tokens, and the
  provider-class headroom multiplier in Step 2(a) — and that gets its **own** ledger entry rather
  than being folded into #79, whose specific subject is prompt-cache write pricing on the inference
  path. Folding two unrelated pricing gaps into one row is how a trigger stops meaning anything.
- **New entry proposed**: *"The proxy-FT cost estimate's provider headroom multiplier is a
  documented constant, not a measured one; the per-minute serve rate is likewise a documented
  constant; and a job's recorded spend is an upper bound when the provider reports no actual cost."*
  Trigger: **when a second Tuner adapter lands, or when the pricing drift detector gains a
  training-price or serve-price check — whichever is first.** Owner: @devarispbrown.
- **Second new entry proposed** *(F1)*: *"A bridge run can leak a dedicated inference endpoint that
  keeps billing after Kno exits, when teardown fails and the process does not survive to sweep it.
  `kno doctor` reports the leak from the durable row; Kno cannot destroy an endpoint without a
  process, and the sweep only runs on resume."* Trigger: **when a second Tuner adapter lands, or
  when any supported provider offers a server-side endpoint TTL or auto-expiry Kno can set at deploy
  time — whichever is first.** Owner: @devarispbrown. Neither half can be satisfied by this plan's
  own PR series, which ships one Tuner and sets no TTL.

## Acceptance criteria

Numbered, testable, each naming an observable.

1. `kno bridge --select-run-id X --pool P` **without** `--bridge` exits 0, prints a per-job table
   and a total, and makes **zero** HTTP requests: a test injects an `http.RoundTripper` that fails
   every dial and the command still succeeds.
2. `kno bridge --select-run-id X --pool P --tuner together:M` **with** `--bridge` on a non-TTY and
   without `--yes` exits 2 with `errs.ErrBudgetExceeded`'s decline path and submits nothing; a test
   asserts the fake Tuner's `Submit` was called zero times.
3. For every group, the bytes handed to `Tuner.Submit` as `TuningJob.training_data` are
   byte-identical to what `kno export --destination tuning_set` writes for the same Portfolio
   filtered to that group's Asset IDs. Pinned by a golden shared between `core/export_test.go` and
   the bridge test.
4. `renderTuningSet` emits at least one `assistant` message per line for every Asset it renders; a
   behavior Asset whose content is neither chat JSONL nor renderable as an assistant turn is refused
   at export with `errs.ErrInvalidInput` naming the Asset ID, and a test asserts the old
   single-`user`-message shape is no longer produced by any input.
5. `Submit` is never called without a preceding successful `Guard.Authorize`: a test wraps the Guard
   and fails if `Submit` is reached with no open reservation, and a second test with
   `MaxCostUSDMicros` set one micro-dollar below a single job's estimate asserts zero `Submit` calls
   and `RUN_STATUS_BUDGET_STOPPED` / exit 2.
6. A `tuning_jobs` row with `state = "submitting"` exists **before** `Submit` is entered: the fake
   Tuner queries the store from inside `Submit` and asserts the row is present with the estimate in
   its cost columns.
7. `Submit` returning a `JobRef` settles the estimate immediately: `Guard.Spent().CostUSDMicros`
   equals the estimate before `Status` is ever called, and `store.SettledSpend` reports the same
   figure from a fresh process.
8. A run killed after `Submit` and resumed calls `Submit` **zero** additional times for that group,
   polls the recorded `JobRef` instead, and `Guard.Restore(store.SettledSpend(...))` reports the
   estimate exactly once — asserted with a kill/resume integration test in the shape the existing
   resume tests use.
9. A row left in `submitting` by a crash inside the request window causes the adapter to list
   provider jobs and adopt by `suffix`; with no match the row becomes `abandoned`, an `OrphanSpend`
   event is emitted, `SettledSpend` still counts the estimate, and `Submit` is called zero times.
10. `JobState.actual_cost_usd_micros` above the estimate records the delta through
    `RecordOrphanSpend`, emits `SettlementOvershoot`, and makes `Guard.Overshoot()` non-zero by
    exactly the delta. Below the estimate records **nothing** and leaves `Spent()` unchanged; absent
    (zero) records nothing and every rendering of that job says `estimated`, never `billed`.
11. A base model with no training price is refused at planning time with `errs.ErrInvalidInput`
    naming the model, the scheme, and `pricing.Version`, and naming `--price-train-per-mtok` as the
    fix. `--accept-unknown-cost` does **not** satisfy it: a test passes the flag and asserts the
    refusal stands.
12. The consent quote for an armed bridge contains one line per job, the total, and the literal
    irreversibility sentence; a golden pins it. A test asserts the quote's total equals the sum of
    the per-job estimates plus the eval-pass estimate.
13. An Asset routed to two clusters appears in **exactly one** group; a fixture with deliberate
    overlap asserts each Asset ID occurs in exactly one group's member list, and that removing the
    exclusivity rule makes the test fail.
14. An Asset routed to zero clusters gets no bridge verdict: its group is reported `unknown` and it
    receives no `BRIDGE_UNCONFIRMED` rejection.
15. A Portfolio whose `DESTINATION_TUNING_SET` entries include a `KIND_KNOWLEDGE` Asset without
    `user_overridden` is refused before any job is submitted, naming the Asset.
16. A cluster with fewer than `core.MinClusterCases` dev Cases is **skipped**, reported as skipped,
    and costs zero jobs — the test reads the exported constant, so changing it changes the verdict.
17. Group count above `--bridge-max-groups` is refused with a fix line naming the flag and the price
    per group; no job is submitted.
18. No `Valuation` written by a bridge run carries a `delta_goal` attributed to fine-tuning, and no
    rendering prints a per-Asset fine-tuning number; a test greps both human and `--json` output for
    the per-Asset shape and fails on a hit.
19. Every group's `Δ_group` is reported with an `Interval` or is not reported at all — prime
    directive 5, enforced the way `TestValuationOmitsDeltaWithoutInterval` enforces it for Value.
20. A bridge run over Evals containing a holdout Case tagged with a sentinel never yields, scores, or
    trains on that Case; the sentinel appears nowhere in any training file, event, or output.
21. No event payload, log line, span, or error message contains `TuningJob.training_data`,
    `Asset.content`, or any credential; a test drives a full fixture-backed bridge with sentinel
    Asset content and asserts the sentinel appears in no captured output at any verbosity.
22. Fixtures contain only the allowlisted filenames (`training_data.jsonl`, `request.json`,
    `response.json`, `poll-NN.json`, `status`, `note.txt`) and no headers in either direction; the
    key-material scan covers the tuner packages.
23. The full poll sequence `VALIDATING_FILES → QUEUED → RUNNING → DEPLOYING → SUCCEEDED` replays
    from fixtures with no network, and a `FAILED` fixture surfaces the provider's error text
    verbatim (per `JobState.error`'s godoc) with exit 1.
24. A job that exceeds `--bridge-timeout` stops **waiting** without cancelling: exit 4
    (`ExitInterrupted`), the message names the provider job id and `--resume`, the row stays
    non-terminal, and `Submit` is not called again on resume.
25. `kno bridge --help` mentions `--bridge`, `--tuner`, `--bridge-max-groups`, `--bridge-timeout`,
    `--price-train-per-mtok`, `--price-serve-per-minute`, `--bridge-max-live-endpoints`,
    `--bridge-max-serve-minutes`, the sentence that jobs cannot be un-submitted, and the sentence
    that hosting a tuned model is billed per minute including while idle. Snapshot-tested.
26. `store.SettledSpend` sums `tuning_jobs` alongside `outcomes`, `measurements`, and
    `runs.orphan_*`; a migration test at `schemaVersion` 5 → 6 asserts an existing database upgrades
    and that a pre-migration run's spend is unchanged.
27. `make typecheck-proto` passes: `STAGE_BRIDGE`, `REJECTION_REASON_BRIDGE_UNCONFIRMED`, and the
    four event members are additive and `buf breaking` is green against `main`.
28. *(F2)* The primary group is the **intersection** derivation, not the cluster size: a fixture in
    which Asset `a` is routed to 3 Cases of cluster A (which has 40 Cases) and 5 Cases of cluster B
    (which has 6) assigns `a` to **B**. A second fixture pins the tie-break (equal intersections ⇒
    lower tag, then lower Asset ID), and a third asserts `ClusterSnapshot.NDropped` never changes a
    primary-group assignment. Removing the intersection and counting `len(routed.CaseIDs)` instead
    makes all three fail.
29. *(F1)* A base model with no **serve** price is refused at planning time, in the same shape as
    criterion 11, naming `--price-serve-per-minute` as the fix; `--accept-unknown-cost` does not
    satisfy it, and zero jobs are submitted.
30. *(F1)* At most `--bridge-max-live-endpoints` (default 1) endpoints are live at any instant: a
    fake Tuner records `Deploy`/`Teardown` timestamps and a test asserts no two live intervals
    overlap across an 8-group bridge, and that every `Deploy` is followed by a `Teardown`.
31. *(F1)* `Teardown` is called on **every** exit path — success, eval-pass failure, budget cap,
    `--bridge-timeout`, and `Ctrl-C` — asserted once per path. A `Teardown` that returns an error
    fails the run, emits `TuningEndpointChanged` with state `leaked`, and the error text contains
    the endpoint id; a test asserts the run does not report completed and the error is not swallowed.
32. *(F1)* Hosting settles forward: with a fake clock advanced 7 minutes, `Guard.Spent()` and
    `store.SettledSpend` each report 7 minutes at the serve rate **before** `Teardown` is called, and
    a process killed at minute 4 leaves 4 minutes settled in the durable row.
33. *(F1, F3)* A resumed run's first action is the endpoint sweep: with an endpoint still live from a
    killed process, resume lists it, tears it down, records the minutes since the last settled tick
    through `RecordOrphanSpend`, emits `SettlementOvershoot`, and makes `Guard.Overshoot()` non-zero
    by exactly that delta — asserted **before** any `Submit` or `Deploy` call in the resumed run. An
    endpoint the provider no longer lists settles at `--bridge-max-serve-minutes`, not at the last
    tick.
34. *(F1)* Reaching `--bridge-max-serve-minutes` tears the endpoint down, reports that group
    `unknown`, and issues no further eval-pass calls against it; reaching the budget cap mid-serve
    does the same and exits 2.
35. *(F1)* The consent quote contains a separately-labelled hosting line and a total equal to
    training + hosting + eval-pass estimates; a golden pins it, and a test asserts the hosting line
    is absent (or reads `$0.00`) for a fake Tuner whose `Deploy` is a no-op.
36. *(F1)* `kno doctor` lists every `tuning_jobs` row with a non-null endpoint id and a null teardown
    timestamp, naming the run, the group, and the endpoint id.

## Alternatives considered

**Settle at completion instead of at submission.** The obvious reading of "settle what was actually
spent". Rejected: the reservation is in-memory, `Guard.Restore` deliberately does not restore
reservations ("its work either completed and was persisted … or it did not happen"), and a job runs
for minutes to hours. A kill during that window would leave $8 spent and zero recorded, and the
resume would authorize it again — the exact doubling `Restore`'s godoc exists to prevent. Settling
the pessimistic estimate at submission and truing up afterwards is strictly safer in the only
direction that matters, and its cost (recorded spend is an upper bound) is disclosed rather than
hidden.

**Per-Asset fine-tuning, not group ablation.** The measurement everyone actually wants. Rejected on
arithmetic that is not close: DESIGN's own worked example is 500 per-asset runs against 7, at $3–8
each. It is not a budget question, it is an order-of-magnitude question, and the design's answer —
group-level ablation plus within-group ICL ranking — is the reason the bridge exists at all.

**A new clustering for the bridge (embeddings, or a judge call per Asset).** Rejected twice over:
`value.ClusterSnapshot` already exists, is deterministic, is persisted, and is what `ComputeGaps`
already reasons about — a second grouping would make the gaps report and the bridge report describe
different partitions of the same Assets. And a judge call per Asset is a new spend path bolted onto
a plan whose entire subject is spend discipline.

**OpenAI's fine-tuning tier first.** Rejected in Step 5: the file format is free either way, and the
LoRA-on-an-open-1-to-8B-model half — which is what makes the measurement cost single-digit dollars
and is the only reason Tier 3 is affordable — is not OpenAI's.

**Ship the `Tuner` adapter without the bridge orchestration (`kno tune`, a thin CLI over Submit).**
Superficially attractive as a smaller first PR. Rejected: an adapter that can spend $8 with no
group design, no LOO attribution, and no interval is a money path with no measurement attached to
it, which inverts the product. It also front-loads exactly the code whose correctness nobody can
check yet.

**Allow `--accept-unknown-cost` for unpriced base models.** Consistent with the inference path, and
rejected for the asymmetry that makes the consistency false: on the inference path an unknown price
is bounded by `--max-calls` and each call is fractions of a cent, so the flag degrades gracefully.
Here the estimate is the *only* control on a single irreversible $8 commitment. `--price-train-per-
mtok` is the honest escape: it makes the user state the number rather than waive it.

**Keep all N+1 endpoints live for the whole bridge and evaluate in parallel.** *(F1)* Faster in
wall-clock and the obvious shape once you know hosting exists. Rejected as the default: it multiplies
an idle-billed per-minute meter by N+1 and makes the worst case scale with the slowest group rather
than the sum of the groups. `--bridge-max-live-endpoints` raises it for a user who states the price;
1 is the default because the failure mode of the alternative is a bill nobody quoted.

**Download the LoRA weights and serve them locally, as Together's free alternative invites.**
Rejected: DESIGN's whole premise for Tier 3 is "no torch in the OSS binary" (DESIGN.md:134), and a
local serving runtime is a larger dependency than the entire bridge. It also relocates the cost from
dollars to the user's hardware without removing it.

**Cancel a job that outlives `--bridge-timeout`.** Rejected as the default: cancelling discards money
already committed, and providers commonly bill partial training anyway ***(verify)***. Stopping the
wait preserves the option; `--bridge-cancel-on-timeout` is the opt-in, and `JOB_STATUS_CANCELLED`'s
godoc ("Cancelled by the user or by the budget guard") already anticipates both callers.

## Affected packages

`bridge/` (group formation, LOO orchestration, poll loop, reconciliation — currently one `doc.go`);
`adapters/tuner/together` (new), later `adapters/tuner/fireworks`; `adapters/agent/pricing`
(training-price **and serve-price** dimensions, provider headroom constant, `Models`-style refusal
listing);
`adapters/pool/parquet` (new, debt #83); `core/` (`renderTuningSet` fix in `core/export.go`; the
bridge's stage entry point; **`core/ring0.go` gains `Tuner.Deploy`/`Tuner.Teardown` and the
`Endpoint` struct** — a pre-1.0 public-surface addition, see Step 2(f)); `stats/interval` (reused unchanged;
Bonferroni over N groups reuses the Select-stage helper); `store/` (`tuning_jobs` table,
`schemaVersion` 5→6, three interface methods, `SettledSpend` fourth term, all fakes updated);
`proto/kno/v1` (`STAGE_BRIDGE`, `REJECTION_REASON_BRIDGE_UNCONFIRMED`, four event members);
`cli/` (`kno bridge`, flags including `--bridge-max-live-endpoints`, `--bridge-max-serve-minutes`
and `--price-serve-per-minute`, consent quote additions, the `kno doctor` leak report, `--json`
shape, help snapshots); `tui/` (renderers for the four new events); `docs/` (mental model,
what-the-numbers-mean, a "Confirm a tuning set with the bridge" cookbook entry, the adapters matrix,
the pricing page);
`docs/debt.md` (#83 repaid, one new entry); `CHANGELOG.md`; `Makefile` (`record-fixtures` gate).

## Proto / schema impact

Verified against `proto/kno/v1/`. **Additive only.**

| Change | File | Note |
|---|---|---|
| `STAGE_BRIDGE = 6` | `run.proto` | `Stage` currently ends at `STAGE_EXPORT = 5` |
| `REJECTION_REASON_BRIDGE_UNCONFIRMED = 10` | `valuation.proto` | enum currently ends at `UNDERPOWERED = 9` |
| `TuningJobSubmitted tuning_job_submitted = 26` | `event.proto` | oneof currently ends at `export_written = 25` |
| `TuningJobStateChanged tuning_job_state_changed = 27` | `event.proto` | |
| `BridgeGroupMeasured bridge_group_measured = 28` | `event.proto` | |
| `TuningEndpointChanged tuning_endpoint_changed = 29` | `event.proto` | Step 2(f); the hosting dimension is user-visible state |

`tuner.proto` needs **no change**: `TuningJob`, `JobRef`, `JobState`, `JobStatus` are already
complete for this design, including `ablation_group`, `estimated_cost_usd_micros`, `suffix`, and
`optional progress`. `Endpoint` is likewise a Go struct in `core`, not a proto message: it is an
adapter-lifecycle type with one implementation, and committing the wire contract to it would repeat
the mistake `lora_rank` nearly made. `internal/schema`'s `TestEveryEnumHasUnspecifiedZero` continues to pass — no
new enum is introduced, only values on existing ones. `buf breaking --against main` passes: no field
is renumbered, retyped, or removed. The durable job record is a Go struct in `store`, not a proto,
matching `store.Outcome`/`store.Measurement` — so the wire contract carries no field that exists only
to serve SQLite.

## Edge cases

| Case | Behavior |
|---|---|
| A job that never completes | `--bridge-timeout` (default 60m per job) stops **waiting**, not the job. Exit 4, message names the provider job id and `--resume`. Row stays non-terminal; resume polls, never re-submits. `--bridge-cancel-on-timeout` opts into cancelling. |
| Provider outage mid-run, before any submit | Ordinary transient handling; nothing was spent; run stops resumable. |
| Provider outage mid-run, during a submit | **No retry** without a provider idempotency key. Row stays `submitting`; resume adopts by `suffix` or abandons with `OrphanSpend`. |
| Provider outage during polling | Backoff and continue; the job is running and billing regardless. Exhausting the retry budget is the timeout path, not a failure — money is already committed. |
| Cost overrun (`actual > estimate`) | `RecordOrphanSpend` + `Guard.Restore` + `SettlementOvershoot`; `Overshoot()` non-zero; the report prints the breach and names it as under-estimation. |
| Cost underrun (`actual < estimate`) | No credit, no negative settle. Reported as "estimated X, billed Y". Recorded spend is an upper bound and says so. |
| Provider reports no cost at all | Estimate stands; every rendering says `estimated`. Never zeroed. |
| Resumed run, job already terminal | Reconcile from the recorded `JobState`, run the eval passes, no `Submit`. |
| Resumed run under a **lower** `--max-cost-usd` | `Restore` is additive and does not check the cap, so `Overshoot()` reads non-zero immediately. Per its godoc that is "a supported stop, not an estimation error" — the run refuses further submits and says which. |
| Revoked/rotated key mid-run | Poll returns 401. Run stops with the `TOGETHER_AUTH` refusal naming the env var **and the live provider job id**, so the user can cancel it themselves. The estimate stays settled. Kno never keeps a loss it does not mention. |
| Key missing at start | Refused at construction, before planning, naming the variable and stating no file/profile/metadata is read. Zero jobs. |
| Training file fails provider validation | `JOB_STATUS_VALIDATING_FILES → FAILED`. Provider error verbatim. Whether a validation failure is billed is provider-specific ***(verify)***; the estimate stays settled either way, which is the conservative direction. |
| A group renders an empty training file | Refused before submit — a zero-example fine-tune is a paid no-op. |
| All-in job succeeds, one LOO job fails | The bridge reports per-group verdicts for the groups it measured and `unknown` for the failed one. It does **not** substitute the all-in score. Partial is reported as partial. |
| Two groups tie on primary membership for an Asset | Deterministic tie-break: tag, then Asset ID. Pinned by test. |
| Cluster below `MinClusterCases` | Skipped, reported, zero jobs. |
| More groups than `--bridge-max-groups` | Refused with a fix naming the flag and the per-group price. |
| Portfolio has zero tuning-set entries | "Nothing to bridge" — legal, reported, exit 0, zero jobs, no prompt. |
| Knowledge-kind Asset in the tuning set | Refused before any submit unless `user_overridden`. |
| Source Value run was budget-stopped | Same posture as Select: refused unless `--allow-partial`; the source status travels onto the bridge run. |
| `Run.value_plan` empty or undecodable | No clusters to group by. Refused with a fix pointing at re-running `kno value` — never a single implicit "everything" group. |
| Two bridge runs against one Portfolio concurrently | Different run IDs ⇒ different `suffix` ⇒ different provider jobs. Money is spent twice, correctly, and both are recorded. Kno does not lock a Portfolio. |
| Ctrl-C at the consent prompt | `errs.ErrInterrupted`, exit 4, nothing submitted — the existing `consentDialog` path. |
| Ctrl-C between authorize and submit | `defer res.Release()`; the `submitting` row (if written) is adopted-or-abandoned on resume. |
| Tuned model unreachable for eval passes | The eval passes fail as ordinary agent errors; the training spend stays recorded; the group reports `unknown` rather than a delta over nothing. |
| `Deploy` fails after a successful job | Group reports `unknown`. Training spend stays settled; zero serve minutes are billed because nothing came up. Never retried into a second endpoint without a teardown of the first. |
| Endpoint never becomes ready | `--bridge-max-serve-minutes` tears it down, the accrued minutes are settled, the group reports `unknown`. Waiting longer only spends more on a replica that is not answering. |
| `Teardown` fails | Run fails, `TuningEndpointChanged` with state `leaked`, error names the endpoint id and the provider console. The row keeps its endpoint id and null teardown timestamp, so `kno doctor` and the next resume both see it. |
| `Ctrl-C` while an endpoint is live | Deferred teardown runs. If it cannot complete, the leak is reported in the same words as the row above — Kno never exits quietly on a live meter. |
| Resume finds an endpoint still running | Swept first, before any submit or deploy: torn down, minutes since the last settled tick recorded through `RecordOrphanSpend` + `SettlementOvershoot`. |
| Resume finds a run's endpoint gone | Settled at `--bridge-max-serve-minutes`, the conservative direction, and reported as an estimate rather than a measurement. |
| Provider auto-serves (a future Tuner) | `Deploy` is a no-op returning a zero-rate `Endpoint`; the hosting line in the quote reads `$0.00`; no serve minutes accrue. The second spend shape costs nothing where it does not exist. |
| Serve rate unpriced | Refused at planning, like an unpriced train rate. `--price-serve-per-minute` is the only escape; `--accept-unknown-cost` does not apply. |

## Test plan

- **Money, first-class.** A `fakeTuner` with programmable latency, terminal state, and reported cost
  drives: authorize-before-submit; settle-at-submission; write-ahead row ordering (asserted from
  inside `Submit`); overshoot/underrun/absent-cost reconciliation; cap-refusal before submit;
  double-`Settle` no-op; negative-cost report refused by `addSpend`.
- **Hosting, first-class too** *(F1)*: a fake clock plus a fake Tuner recording `Deploy`/`Teardown`
  timestamps drives per-minute settle; non-overlapping live intervals; teardown on all five exit
  paths; teardown-failure propagation (verified failing when the propagation is removed); the
  serve-minutes cap; the serve-price refusal; the zero-rate auto-serve Tuner.
- **Kill/resume integration**, in the shape of the existing resume tests: kill after the row write,
  after `Submit`, mid-poll, after terminal, and **mid-serve with an endpoint live** — assert
  identical final results, **zero** additional `Submit` calls, and that the resumed run's first
  provider interaction is the endpoint sweep, with `SettledSpend` read from a fresh process.
- **Adapter tests against recorded fixtures only.** Full poll sequences per terminal state; the
  allowlist test; the key-material scan; `httptest` transport behavior; redirect refusal;
  private-address refusal; response-size cap; credential-refusal matrix; `goleak.VerifyTestMain`.
  A deliberate NO: no test in PR CI calls a real FT endpoint.
- **Group ablation as a pure function.** The intersection derivation as a table test, including the
  large-cluster/small-overlap case and `NDropped` irrelevance *(F2)*; membership exclusivity (with a
  mutation test: remove the rule, the test fails); zero-cluster Assets; tie-breaks;
  `MinClusterCases` boundary read from the exported constant; determinism across map-iteration
  order.
- **Statistics.** `Δ_group` interval method and sidedness labelled and pinned; the
  no-interval-no-number invariant; Bonferroni over N groups characterized; the interference read
  gated on a powered control, verified failing with the gate removed.
- **Export.** The training-file golden shared with the bridge; the assistant-turn requirement; the
  refusal for un-renderable behavior Assets; byte-identity between `kno export` and what was
  submitted.
- **Secrets and content.** Sentinel Asset content driven through a whole fixture-backed bridge,
  asserted absent from stdout, `--json`, events, spans, and logs at every verbosity — the shape
  `observe`'s existing test uses.
- **Holdout canary** for the bridge stage.
- **Store.** Migration 5→6 up and idempotent; `SettledSpend` four-way sum; fakes updated;
  round-trip of `TuningJobRecord`; a test asserting the record contains no training data.
- **CLI.** Help snapshots; `--json` equivalence golden; exit codes 0/1/2/4 per the table above; the
  un-armed run with a failing dialer.
- **Fuzz.** The `JobState` / poll-response parser joins `make fuzz-short`'s targets — it is a parser
  over provider-controlled input, which is the class debt #4 tracks.

## Rollback

Delete `bridge/`'s implementation, `adapters/tuner/*`, `cli/bridge.go`, and the pricing training and
serve dimensions. What does **not** roll back cleanly, and is therefore called out: the
`renderTuningSet` fix (a behavior change with a golden diff — reverting it re-ships an untrainable
artifact, so it should not be reverted), the `store` interface addition and its migration (a
forward-only schema bump; a 6 → 5 downgrade is not supported and the CHANGELOG says so), the
parquet pool adapter (independent, keep), and the `Tuner.Deploy`/`Teardown` additions (removable, but
a public-surface removal needing its own CHANGELOG note under the pre-1.0 allowance — and removing
them while any Tuner ships would restore the false "one spend shape" model, so they go only when the
bridge does). The three proto additions are additive and unreferenced
once the code is gone. `STAGE_BRIDGE` stays in the enum: removing an enum value is the breaking
change the additions were designed to avoid.

## Docs impact

`docs/mental-model.md` — the bridge funnel moves from future tense to present for Tier 3, with the
exclusive-membership rule stated in one sentence. `docs/what-the-numbers-mean.md` — the existing
"The fine-tuning bridge is a selection signal, not a guarantee" section gains what a **group** delta
claims and does not claim, why there is no per-Asset FT number, what `BRIDGE_UNCONFIRMED` means, and
the sentence that a bridge run's recorded spend is an upper bound. New cookbook entry "Confirm a
tuning set with the bridge", including the un-armed dry run as step one. `docs/cookbook/export-a-
tuning-set.md` updated for the assistant-turn change. Adapters matrix gains a Tuner column. Pricing
page gains **both** new dimensions — training per token and serving per minute per replica, idle
included — and the headroom multiplier, plainly; the cookbook entry's steps name the deploy and the
teardown rather than hiding them behind "run the eval passes". `docs/cookbook/` and the retention
page gain what `kno doctor` reports about a leaked endpoint. CLI help snapshots.
CHANGELOG under Unreleased, with the `renderTuningSet` change called out as behavior-changing.
`docs/debt.md`: #83 repaid, two new entries.

## Accepted risks

- **Recorded spend is an upper bound.** Estimates are settled and never credited back. Disclosed in
  the report, the docs, and the consent quote rather than smoothed away.
- **Proxy→target transfer is imperfect.** DESIGN says so (DESIGN.md:139) and `what-the-numbers-
  mean.md` already says a Tier-3 number is "a *ranking* signal. Only step 4 is a result." This plan
  does not improve that; it makes sure nothing in the output pretends otherwise.
- **Exclusive group membership discards information.** An Asset that genuinely serves two behaviors
  is measured as serving one. The alternative — overlapping LOO — is not conservative, it is wrong,
  and the loss is stated in the docs rather than papered over.
- **Vendor dependency for Tier 3**, exactly as DESIGN's technology table admits. Mitigated by
  Fireworks second, on a short leash, as a falsifier for the interface.
- **The bridge has two spend dimensions, not one.** The plan asserted otherwise before Phase 1
  refuted it *(F1)*. Hosting is priced, capped at `--bridge-max-serve-minutes`, serialized to one
  live endpoint by default, quoted on its own line, and settled per minute — but it is a second meter
  and the honest statement is that the bridge costs more than the job list says.
- **A leaked endpoint bills after Kno exits.** Teardown is unconditional and its failure is loud, the
  durable row records the leak, `kno doctor` reports it and the next resume sweeps it — but if the
  process dies and is never resumed, only the user can stop the meter. Second new ledger entry above.
- **Sequential endpoints trade wall-clock for money.** An 8-group bridge deploys and tears down 9
  times in series. Deliberate: the parallel alternative multiplies an idle-billed meter by N+1.
- **The provider headroom multiplier and the serve rate are documented constants, not measurements.**
  New ledger entry above, with a trigger that cannot self-satisfy.
- **No nightly live coverage for the bridge.** Manual re-record per minor release, in the runbook.
  Provider API drift will be found by a human, later than CI would have found it.
- **Most provider facts in this plan are still ***(verify)***, and one that was not checked turned
  out to be wrong in the expensive direction.** Phase 1 settled four (the assistant-message
  requirement, Together's published per-token training rates, OpenAI's job-list endpoint, and the
  absence of documented FT idempotency) and **refuted** one — Together's auto-serving — which cost a
  whole spend dimension, not a fixture. The design is arranged so the remaining tags cost a fixture
  and an adapter method; the lesson recorded here is that a *pricing-model* assumption is not in that
  class and must be checked before implementation, not during it.

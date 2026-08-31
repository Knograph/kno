# Knowledge-injection mode and the first writable Destination

DESIGN's v0.2 line (DESIGN.md:398) names "knowledge-injection mode for writable KBs". Two things
hide behind that phrase and this plan separates them, because conflating them is how a measurement
mode becomes a production write nobody consented to:

- **The measurement half** — `INJECTION_MODE_KNOWLEDGE`. Kno writes one Asset into the agent's real
  index, measures through the agent's real retriever, rolls the write back. Deployment-faithful,
  ephemeral, transactional.
- **The write half** — `DESTINATION_KNOWLEDGE_BASE` becomes writable. v0.1 renders a MANIFEST and an
  add-list and a human applies it (`core.renderKnowledgeBase`'s godoc: "The adapters that WRITE a
  knowledge base are v0.2"). v0.2 lets Kno apply it. Permanent, destructive, outward-facing.

They ship together because each is incoherent alone: the write half without the measurement half
applies a context-mode upper bound through a retrieval mechanism it never measured, and the
measurement half without the write half produces a number the user cannot act on with the tool that
produced it. They are gated separately, which is the whole point of separating them.

Fires [`docs/debt.md#78`](../debt.md#78) — its live half's trigger is verbatim "when the first
writable Destination adapter lands (v0.2 knowledge injection)". Does not fire
[`#79`](../debt.md#79) or [`#83`](../debt.md#83); see **Debt** below. Every vendor API fact **not
settled by the Phase-1 review below** is tagged ***(verify)***; the ones that were checked against
current Qdrant documentation are stated plainly, and two of them turned out to be hard constraints
rather than conveniences.

**Phase-1 re-reviewed 2026-08-31 — verdict: amend; amendments applied.** Six findings folded, one of
them self-inflicted and exactly the failure this repo's ledger history names. The proposed
concurrent-writer ledger entry's trigger — "when a second `KnowledgeBase` adapter lands" — is
**satisfied by Step 2 of this very plan**, which ships `adapters/knowledge/http` beside the Qdrant
adapter as a deliberate falsifier; a trigger its own PR satisfies is a carryover wearing a
disposition, almost verbatim what #78's own history calls out, so it is replaced with one this PR
cannot reach *(F1)*. Step 5 gains the mechanism that actually un-strands
`DESTINATION_KNOWLEDGE_BASE` — what causes a `KIND_KNOWLEDGE` Asset's Valuation to be measured in
knowledge mode in the first place — because the routing rule alone left #132 fixed in the enum and
unfixed in practice *(F2)*. The undo prompt now states, in its own words rather than in a general
disclaimer, that undoing an **update** deletes rather than reverts, and a double-undo criterion is
added *(F3)*. The `kno_managed` ownership predicate — correctly named as the worst blast radius, but
previously tested with a single planted foreign entry — gains a negative-space property test over a
collection full of foreign data *(F4)*. #78's sequencing gains an explicit go/no-go **date** instead
of "before the writable adapter merges" *(F5)*, and the `--yes-write` muscle-memory risk is promoted
out of Step 3(b) into **Accepted risks**, where a reader looking for the honest list will find it
*(F6)*. Several ***(verify)*** tags are resolved as settled fact and deleted: `PUT
/collections/{c}/points` upserts by id with overwrite semantics, `POST .../points/delete` accepts a
filter, `POST .../points/scroll` paginates by filter, and `GET /collections/{c}` returns
`config.params.vectors.size`. Two are upgraded from hedge to **hard constraint**: Qdrant point IDs
must be an unsigned 64-bit integer or a UUID — arbitrary strings are rejected — so `uuidv5` is the
only legal encoding of an Asset ID rather than a nicety; and server-side inference is
**Qdrant-Cloud-managed only**, so `--embed-model` is *unconditionally* required for the self-hosted
OSS deployment this plan itself calls the common case. Confirmed sound and not reworked: the
confirm, undo, idempotence, credential, provenance-payload and partial-write mechanisms, and the
measurement-half / write-half split.

## Problem

`Kno` reports two kinds of number and says so in `docs/what-the-numbers-mean.md:106`: a
context-injection delta is an **upper bound** ("if this Asset were reliably in the prompt"), while a
knowledge-mode delta "*is* a deployment prediction, and it requires an agent whose index Kno can
write." Only knowledge mode preserves the difference between *the data was missing* and *the data
was there and retrieval missed it* — `core.ring0.go`'s own words, and the reason
`InjectionMode` is a required field on every `Valuation`.

Today only the bound exists. The whole knowledge-mode path is declaration without implementation:

- `core.KnowledgeInjector` is declared in `core/ring0.go` with the `rollback func() error` contract
  and the "`defer rollback()` is the sanctioned idiom" godoc. **No type in the repository implements
  `WithKnowledge`.** Every agent adapter hard-codes `Capabilities().KnowledgeWrite = false`
  (`openaicompat`, `anthropic`, `bedrock`, `vertex`, `exec`, `fake`).
- `INJECTION_MODE_KNOWLEDGE` exists in `common.proto` and is never set.
- `Budget.max_knowledge_base_bytes = 6` exists in `portfolio.proto` and is never read.
- `core.destinationFor` (`core/select.go:573`) **never assigns `DESTINATION_KNOWLEDGE_BASE`**: a
  `KIND_BEHAVIOR` Asset goes to `TUNING_SET`, everything else to `CONTEXT`, and `KNOWLEDGE_BASE` is
  reachable only when the pool pinned `Asset.destination` by hand. So even the export grammar that
  does exist is, in practice, addressed to a Destination the router does not use.
- `core.Export`'s own godoc: "It never touches the Destination itself."

There is no vector-store client, no retriever, no index-writing code, and no rollback machinery
anywhere in the repo. This plan builds the smallest thing that makes both numbers real and lets a
user act on the second one — without turning `kno export` into a command that can quietly rewrite
the index a production agent reads.

## Design

### Step 0 — what already exists

- `core.KnowledgeInjector` — **unchanged by this plan.** `WithKnowledge(ctx, a *Asset) (agent
  Agent, rollback func() error, err error)`.
- `knov1.Capabilities.KnowledgeWrite` — the declared field, false everywhere. The enforcement
  pattern already exists too: `adapters/agent/anthropic/config_test.go:112` asserts
  `any(a).(core.KnowledgeInjector)` agrees with `GetKnowledgeWrite()`. That test shape becomes the
  conformance rule for every adapter.
- `errs.ErrCapabilityUnsupported` (`Code: "CAPABILITY_UNSUPPORTED"`, `Fix: "run `kno doctor` to
  print the capability matrix, then pick a supported injection mode"`) — the refusal for asking an
  adapter for knowledge mode when it cannot do it. Already written, never returned.
- `adapters/internal/endpointsec` — `Check`, `CheckAddress`, `CheckResolved`,
  `WithResolvedAddressCheck`, `RefuseRedirect`. The host policy for **non-agent** adapters. Exactly
  the seam a KB client needs, already built and tested.
- `transport.ParseKeyBindings` / `KeyBindings.Resolve(host, defaultHost, defaultEnvVar)` — the
  "name the variable, never the key" credential mechanism behind `--key-env host=VAR`.
- `core.Export` / `refuseExistingTarget` / `writeAtomic` — the local-artifact path and its
  overwrite policy, whose semantics this plan must find a live analogue for.
- `ExportResult.SelectRunID`, surfaced as `select_run_id` by `cli/jsonreport.go:356`
  (`exportReport`) — the provenance link, fixed in #156, and the thing a KB reader must be able to
  recover.
- **What does not exist**: any `KnowledgeBase` contract; any KB adapter; any embedding path; any
  `--apply`-shaped flag anywhere in the CLI; any event describing a remote write (`ExportWritten`
  carries `destination`, `asset_count`, `bytes_written`, `path` — all file-shaped, and no
  `manifest_path`).

### Step 1 — what "writable KB" means, and the interface

A writable KB is **the index the agent's own retriever reads**. That definition is load-bearing and
it decides the adapter choice in Step 2: a system that is *upstream* of that index — a docs
platform feeding a nightly ingest — is not a writable KB for measurement purposes, because writing
to it does not put the Asset in front of the agent during the measurement. It would produce a
number about nothing.

New Ring-0 contract, `core/ring0.go`, in the vocabulary (`Asset`, `Portfolio`, never "document",
"record", or "chunk"):

```go
// KnowledgeBase is an index Kno can write Assets into and take them out of again.
type KnowledgeBase interface {
    // Describe reports the target's identity and what it supports, checked
    // before anything is written.
    Describe(ctx context.Context) (*KnowledgeTarget, error)

    // Inspect reports what Kno has already written for these Assets, so a plan
    // can be computed without writing anything.
    Inspect(ctx context.Context, scope Scope, assetIDs []string) (map[string]KnowledgeState, error)

    // Write upserts Assets under Kno-managed identifiers. Idempotent on
    // (scope, asset id).
    Write(ctx context.Context, scope Scope, assets []*Asset) (WriteResult, error)

    // Remove deletes exactly the Kno-managed entries named, and nothing else.
    Remove(ctx context.Context, scope Scope, assetIDs []string) error
}
```

`Scope` is `{RunID, Ephemeral bool}`: an ephemeral scope is the measurement half's per-run
namespace, a durable scope is the export half's. One interface, two lifetimes, and the namespace
distinction is in the type rather than in a convention someone can forget.

Adding to Ring-0 is a real decision, defended: `Destination` is a vocabulary word and a *writable*
Destination is a contract, not an implementation detail; `cli`/`api`/`tui` must construct one the
way `resolvePool` constructs a `core.Pool`, which requires the interface to live where `core` can
name it; and the precedent is `Tuner`, declared in Ring-0 before any adapter existed. The rejected
alternative is in **Alternatives considered**.

**The measurement wrapper.** `core.KnowledgeInjector` returns an `Agent`, and this is the subtle
part: the agent Kno returns from `WithKnowledge` is **the inner agent, unchanged**. Kno does not add
a retriever, does not inject content into the prompt, and does not know how the agent retrieves.
It changes the index the agent already reads, and hands the same agent back. That is precisely what
makes the number deployment-faithful, and it is why the existing interface signature works untouched.

```
adapters/agent/knowledge.Wrap(inner core.Agent, kb core.KnowledgeBase) core.Agent
```

implementing `core.KnowledgeInjector` and `core.Capable` — delegating every capability to `inner`
except `KnowledgeWrite`, which becomes true. `WithKnowledge` writes one Asset under the ephemeral
scope, returns `(inner, rollback, nil)` where `rollback` calls `Remove`. A **failed rollback is
never swallowed**: it emits an event, fails the run, and the error names the exact scope and Asset
ID to delete by hand. Ring-0's godoc already demands the `defer`; this plan adds the part it could
not: what happens when the deferred call errors.

### Step 2 — which adapter ships first: Qdrant

`adapters/knowledge/qdrant`, against the Qdrant HTTP API — the four endpoints this plan needs are
confirmed against current documentation and carry no hedge: `PUT /collections/{collection}/points`
upserts **by point id, with overwrite semantics**, `POST /collections/{collection}/points/delete`
accepts a **filter**, `POST /collections/{collection}/points/scroll` **paginates by filter**, and
`GET /collections/{collection}` returns `config.params.vectors.size`. Auth is an `api-key` header
***(verify the header name against the deployment's configured auth)***. The argument:

1. **It is the index, not upstream of it.** A vector store is what a RAG agent's retriever queries.
   Writing to Notion or Confluence puts nothing in front of the agent until an ingest job runs, so a
   measurement through them measures the ingest schedule.
2. **Removal is an exact inverse.** Measurement mode needs a write it can undo within a run.
   Point-delete-by-id is that. A docs platform's create/archive is not: archived pages leave
   tombstones, versions are immutable, and "undo" is a new revision. A measurement whose control arm
   is polluted by the treatment arm's residue is not a control arm.
3. **No SDK.** Plain REST + one header, which is the zero-dependency posture the bedrock/vertex plan
   defended and the `endpointsec` package already serves. Self-hosted Qdrant on `localhost` is the
   common case, which is exactly what `--allow-private-address` exists for.
4. **Deterministic identifiers — and this is a constraint, not a convenience.** Qdrant restricts a
   point id to an **unsigned 64-bit integer or a UUID**; an arbitrary string is **rejected**. An
   `Asset.id` is a string, so `uuidv5(knoNamespace, scope + "/" + asset_id)` is not one option among
   several — it is the only legal encoding of an Asset ID as a point id, and it happens to give
   idempotence by construction rather than by a pre-check. Any future `KnowledgeBase` adapter whose
   target accepts raw string ids still uses the same derivation, because the identifier must be
   stable across adapters for the undo manifest to mean one thing.
5. **Payload filtering** is what makes provenance queryable — `points/scroll` paginates by payload
   filter, confirmed, and Step 6 depends on it.

**The embeddings problem, stated rather than dodged.** Writing to a vector store requires a vector,
and computing one is another model call to another provider — a spend path this plan would
otherwise smuggle in. Resolution:

- `--embed-model <agent-ref>` is **required** for a Qdrant target, pointing at an
  OpenAI-compatible `/v1/embeddings` endpoint reached through the existing `openaicompat` transport
  and credential machinery. Embedding spend flows through `budget.Guard` like every other call:
  priced off the `input` rate of the existing `pricing` table, refused when unpriced unless
  `--price-input-per-mtok` is given, counted in the consent quote.
- **Kno never chooses an embedding model.** A collection's vectors were produced by *some* model,
  and writing vectors from a different one poisons the index silently — nearest-neighbour search
  will still return results, they will just be wrong, and nothing errors. So `Describe` reads the
  collection's configured vector size from `GET /collections/{c}` — `config.params.vectors.size`,
  confirmed discoverable — and **refuses** when `--embed-model`'s output dimension does not match.
  The refusal names both numbers. This is the single most dangerous silent failure in the whole plan
  and it is refused, not warned about.
- **There is no server-side-inference exemption for this plan's primary target, and the earlier hedge
  overstated one.** Qdrant's server-side inference is **Qdrant-Cloud-managed only**; self-hosted and
  OSS Qdrant — which Step 2(3) itself calls "the common case" — has no such mode. So `--embed-model`
  is **unconditionally required** for the deployment this plan is actually aimed at, and the plan
  states it plainly rather than leaving a conditional a reader could take as an escape. A Cloud
  target that does embed server-side may make it optional later ***(verify against Cloud's managed
  inference API when a Cloud adapter path is actually built)***; nothing in this milestone depends on
  it, and the adapter always reports which mode it is in.

**Second adapter, deliberately: `adapters/knowledge/http`** — the bring-your-own-KB shape, two
user-supplied URLs (write, remove) and a JSON body of `{scope, asset_id, content, metadata}`. It
ships in the same milestone as the falsifier: one adapter cannot tell you whether `KnowledgeBase` is
a general contract or Qdrant's shape wearing an interface, and it is the designed contributor
on-ramp CLAUDE.md's community section calls for.

### Step 3 — the write path is destructive and outward-facing

The five questions, each answered with a mechanism.

**(a) Dry-run first — and it is the default.** `kno export --destination knowledge_base --kb
qdrant:<url>/<collection>` computes and prints a plan: N to write, M unchanged, K changed, the
target, the ID scheme, the byte total against `Budget.max_knowledge_base_bytes`. It **writes
nothing**. `--apply` performs it. The default stays non-mutating so that `core.Export`'s existing
contract ("It never touches the Destination itself") remains literally true for every invocation
that does not opt in, and so that the mutation is visible in the shell history of whoever ran it.

**(b) A confirm distinct from the spend confirm.** Yes, and the two flags are separate:

| Question | Instrument |
|---|---|
| May I spend money? | `--yes`, `confirmThresholdUSD`, `consentDialog`, `Guard.PreConfirm` — unchanged |
| May I mutate a system your production agent reads? | `--apply`, plus a TTY prompt that `--yes` does **not** suppress; `--yes-write` suppresses it |

The reason they cannot be one: `--yes` is the flag people paste into CI to unblock spend. If it also
unblocked KB mutation, the first copied CI job would rewrite a production index, and the person who
copied it would have consented to a dollar figure. Two different risks, two different words. A
`--apply` run below the spend threshold still prompts for the write; a `--yes --apply` run on a TTY
still prompts for the write. The write prompt states the target, the collection, the counts, and the
sentence that Kno can remove what it writes but cannot restore what was there before.

Non-TTY without `--yes-write` fails closed with `errs.ErrConfirmationRequired` (exit 1, "the action
was not confirmed, so nothing was changed") — the sentinel whose godoc already explains why a
non-confirmed destructive action must not exit 0.

**(c) Idempotence, by construction.** The identifier is `uuidv5(knoNamespace, scope + "/" +
asset_id)`, so an upsert of the same Asset replaces rather than duplicates — re-export cannot
double-write even if every guard above failed. On top of that, each entry carries
`kno_content_sha256`, so an entry whose hash is unchanged is **skipped, not rewritten**: a second
`--apply` of an unmodified Portfolio issues zero writes and the dry-run plan reports `0 to write, N
unchanged`. Acceptance criterion 9 pins both halves — the plan's counts and the collection's point
count.

**(d) Rollback — two different honest answers.**

For the **measurement** half: rollback is required, exact, and enforced. The ephemeral scope carries
`kno_ephemeral_run_id`, `Remove` deletes by that filter, and every valuation defers it. Crash
recovery: a run's first act is to sweep any `kno_ephemeral_run_id` entries left by an earlier
process of the same run; `kno doctor` reports leftovers from any run in the store, so an orphan is
visible rather than silently resident in a production index forever.

For the **export** half: **Kno cannot restore, and must not imply that it can.** An upsert
overwrites whatever was at that identifier. What Kno promises instead, in three parts:

1. **It never overwrites something it did not write.** Every Kno entry carries `kno_managed: true`.
   Pre-flight `Inspect` refuses when a target identifier exists **without** that marker — that is
   somebody else's data at an ID Kno computed, and the run stops rather than resolving it.
2. **An undo manifest**, written locally beside the export run as `<export-run-id>.kb-undo.json`,
   listing every identifier Kno is about to write, its Asset ID, its prior `kno_content_sha256` where
   one existed, and the target. It is written **before each batch**, not after the run — a crash
   mid-write must leave a manifest that covers everything possibly written, so the manifest is
   deliberately a superset of what actually landed.
3. **`kno export --undo <export-run-id>`** removes exactly those identifiers. It is a compensating
   action, not a transaction, and the docs say so in these words: **"Kno can remove what it wrote.
   It cannot restore what your knowledge base held before."**

   *(F3)* One consequence is sharper than that sentence conveys, and the **prompt itself** must say
   it rather than leaving it to a general disclaimer three paragraphs up. The manifest records a
   prior entry's `kno_content_sha256`, **not its prior content**. So undoing a run that *updated* a
   pre-existing Kno-managed entry **deletes that entry rather than reverting it** — a strictly
   stronger loss than "cannot restore", because the user is left with less than they started with,
   not with what they started with. The `--undo` prompt reads, in these words:

   > Undo removes N entries from <target>. M of them existed before this run and were updated by it —
   > undo DELETES those; it does not revert them to their earlier content. Kno can remove what it
   > wrote. It cannot restore what your knowledge base held before.

   `--undo` is itself a destructive outward-facing write, so it is gated by the same write confirm as
   `--apply` (`--yes-write` on a non-TTY). A second `--undo` of the same run is an idempotent **no-op
   that exits 0**: identifiers already gone are removed without error, nothing is re-derived from the
   KB, and no manifest is rewritten. Criterion 30 pins it.

**(e) Credentials, environment-only.** `QDRANT_API_KEY` bound to the target host through the
existing `--key-env host=VAR` mechanism (`transport.ParseKeyBindings` / `Resolve`) — the name of the
variable is not a secret, the key never appears in `kno.yaml`, a fixture, a log line, a span, or an
error. `endpointsec.Check` applies with the same `--allow-insecure-base-url` /
`--allow-private-address` opt-outs the agent adapters use, and `RefuseRedirect` is unconditional. A
new `errs.Actionable` sentinel `QDRANT_AUTH`, modeled on `anthropic.ErrAuthentication`, whose `Fix`
reads:

> export QDRANT_API_KEY, or bind a key for this host with --key-env host=VAR. Kno reads only the
> environment; it never reads a config file, a credential helper, a cloud profile, or a metadata
> server.

An unauthenticated local Qdrant is legitimate and is not refused for a private address the user
already allowed — the same asymmetry `anthropic.resolveKey` already implements (missing key is fatal
only for the default host).

### Step 4 — the `--force` analogue when the target is alive

v0.1 refuses an existing file unless `--force`, because "an overwritten export is a silent
mutation". The live analogue must define *exists* on **the thing Kno would replace**, not on the
target: refusing a non-empty collection would refuse every real knowledge base, which is not a
safety property, it is a bug that trains people to always pass `--force`.

| Target state | Behavior |
|---|---|
| No Kno-managed entries | Write freely (still gated by `--apply` + the write confirm) |
| Kno-managed entries from the **same** `select_run_id` | Idempotent: unchanged entries skipped, changed ones updated. **No `--force` needed** — this is a re-export, and re-export must be safe |
| Kno-managed entries from a **different** `select_run_id` | **Refused without `--force`.** A different Portfolio is a different decision, and replacing one with another is the silent mutation the file rule exists to stop. The refusal names both run IDs |
| A target identifier exists **without** `kno_managed` | Refused **even with `--force`** — Kno does not own it, and `--force` means "replace my previous export", never "take somebody else's data" |
| Collection does not exist | Refused. Kno never creates one: creating a collection commits to a vector dimension and a distance metric that belong to the user's retrieval design, not to an export command |

The local manifest is still written for a KB export. The artifact goes remote; the audit trail stays
on disk, where `writeAtomic` and `refuseExistingTarget` apply unchanged.

### Step 5 — routing, so that a writable Destination has something to write

`core.destinationFor` never assigns `KNOWLEDGE_BASE`. That is correct today — a Destination nothing
can write is one no Asset should be routed to — and it must change with this plan or the writable
adapter has an empty input. The rule:

> A `KIND_KNOWLEDGE` Asset measured in `INJECTION_MODE_KNOWLEDGE` routes to
> `DESTINATION_KNOWLEDGE_BASE`. A `KIND_KNOWLEDGE` Asset measured in `INJECTION_MODE_CONTEXT` keeps
> routing to `DESTINATION_CONTEXT`.

**What causes a knowledge Asset to be measured in knowledge mode.** *(F2)* The rule above is a
routing *consequence*, and on its own it would leave #132 fixed in the enum and unfixed in practice:
if every Valuation keeps coming out `INJECTION_MODE_CONTEXT` by default, the newly writable
Destination is un-stranded in code and still receives nothing. The mechanism is the KB target itself,
and there is no second flag to forget:

> `kno value --kb qdrant:<url>/<collection> --embed-model <ref>` measures every `KIND_KNOWLEDGE`
> Asset in `INJECTION_MODE_KNOWLEDGE` **by default**. Supplying an index Kno can write *is* the
> opt-in.

`--injection-mode context` forces the upper-bound measurement for a run that has a target but wants
the cheaper number. `--injection-mode knowledge` **without** `--kb` is refused with
`errs.ErrInvalidInput` naming `--kb` — never silently downgraded, because a silent downgrade would
relabel a deployment prediction as an upper bound, which is the precise dishonesty
`what-the-numbers-mean.md:106` exists to prevent. `KIND_BEHAVIOR` and unclassified Assets are
unaffected; mode selection applies to knowledge-kind Assets only, and a run may carry both modes
across different Valuations while never mixing them within one (criterion 18).

Without `--kb`, nothing routes to `DESTINATION_KNOWLEDGE_BASE`, and that is correct rather than a
residual strand: a Destination is worth routing to only when something can write it, and in a run
with no writable target nothing can. Criterion 31 pins all three arms.

**The destination follows the mode that produced the number**, and this is the honest form of it: a
context-mode delta is a claim about being in the prompt, so its Destination is the prompt; a
knowledge-mode delta is a claim about being retrieved, so its Destination is the index. Routing a
context-bound Asset into a knowledge base would apply an upper bound through a mechanism that was
never measured — the exact conflation `InjectionMode` exists as a required field to prevent. The
pool's explicit `Asset.destination` still wins, as it does today.

`Budget.max_knowledge_base_bytes` (field 6, defined and unread) becomes the Select stage's budget
dimension for the knowledge-base Destination, exposed as `--max-knowledge-base-bytes`. It exists,
it is the right unit for an index, and leaving it unread while adding a new cap would be the
vocabulary drift the rules forbid.

### Step 6 — provenance a KB reader can act on

Every written entry carries, in its payload:

| Key | Value |
|---|---|
| `kno_managed` | `true` — the ownership marker Step 3(d)(1) depends on |
| `kno_asset_id` | `Asset.id` |
| `kno_select_run_id` | the Select run whose Portfolio this came from — the same value `kno export --json` prints as `select_run_id` |
| `kno_export_run_id` | the Export run that wrote it |
| `kno_portfolio_rank` | `PortfolioEntry.rank` |
| `kno_content_sha256` | the idempotence key |
| `kno_written_at` | RFC 3339 |
| `kno_version` | the Kno version that wrote it |

So a reader of the KB answers "which entries did Kno write, and from which Portfolio" with one
payload filter, in either direction, without Kno present. `kno report --export-run-id <id>` prints
the same linkage from the local side.

**No score, no delta, no interval, no rejection reason in the payload.** A KB payload is read by a
retriever and can surface in a prompt; a measurement number leaking into the agent's context would
contaminate the measurement that produced it, and would put Kno's own reasoning into the user's
production answers. This is enforced by a test that asserts the written payload keys are exactly the
allowlist above.

### Step 7 — the spine

`ExportWritten` is file-shaped: `destination`, `asset_count`, `bytes_written`, `path`. A remote write
has no path and its interesting numbers are different, so rather than overload it, a new additive
oneof member continuing from 25:

- `KnowledgeBaseWritten knowledge_base_written = 26` — `target` (host + collection, **never** the
  key), `select_run_id`, `written`, `skipped_unchanged`, `updated`, `bytes_written`, `dry_run`
  (bool), `undo_manifest_path`.

Plus `KnowledgeWriteRolledBack knowledge_write_rolled_back = 27` for the measurement half's rollback
failures, which is user-visible state with nowhere else to go. Content never appears in either —
`ExportWritten`'s godoc already sets the rule ("The destination is named but its CONTENT is not").

### Debt

- **[`#78`](../debt.md#78) — FIRES. Disposition: repay, in this milestone, before the writable
  adapter merges.** The entry was split on 2026-08-28; its pairing-scheme half was paid then, and
  its measurement-design half was re-dated to a trigger chosen specifically because "nothing in this
  plan touches [it], so the trigger cannot be satisfied by accident." That trigger is this plan. The
  work — splitting the baseline's trials across the routed sample, which changes the pairing scheme,
  the consent quote, and the fresh control arm — is a Value-stage change and gets its **own Phase 0
  plan**, landing in the same milestone and merging **before** the writable adapter, not after.

  It is also genuinely more urgent here than it was when deferred, and the record should say why
  rather than treat the trigger as arbitrary: knowledge mode makes each measurement *stateful*. A
  context-mode trial is a stateless call; a knowledge-mode trial is write → invoke → roll back
  against a live index. Trials multiply that, and the trial-splitting question — how many
  measurements the routed sample actually needs — stops being a cost optimization and becomes a
  question about how long a production index is held in a mutated state. The deferral's original
  reasoning ("the cost of deferral is bounded") no longer holds unchanged.

  A third re-dating is not available. The ledger rules permit repay, re-date-with-a-written-reason,
  or promotion to won't-fix with the rationale moved into an ADR. If Phase 1 of the trial-splitting
  plan concludes the work is larger than the milestone, the only honest outcome is the ADR, not
  another trigger.

  *(F5)* "Before the writable adapter merges" is a sequencing rule, not a schedule, and this
  milestone is gated on **two sequential Phase-0/Phase-1 cycles** — the trial-splitting plan's, then
  this one's implementation review. A gate with no date is how a milestone slips without anyone
  deciding to slip it, so: **go/no-go on 2026-09-14.** By that date the trial-splitting plan must
  have cleared its own Phase 1 with its objections accepted or resolved. If it has not, the writable
  adapter does **not** merge into v0.2 on the strength of an unpaid #78, and the disposition that day
  is the ADR — promotion to won't-fix with the reasoning recorded — not a third re-dating and not a
  silent carry into v0.3. The date is recorded in the ledger entry itself, so a lapse is visible to
  the release-tag check rather than only to the people who remember this paragraph.
- **[`#79`](../debt.md#79) — does not fire.** Trigger: "when `costOf` prices cache writes." This
  plan does not touch `costOf`. It does add an embedding spend path (Step 2), which is priced off the
  existing `input` rate and is not a cache-write question. Recorded here so the reading is on the
  record.
- **[`#83`](../debt.md#83) — does not fire.** Trigger: "the first Tuner PR." Nothing here is a
  Tuner. Its disposition belongs to [the bridge plan](2026-08-31-tuner-bridge.md).
- **New entry proposed**: *"Kno's knowledge-base writes are last-writer-wins. Kno takes no lock —
  a KB has none it owns — so a concurrent writer's change is detected on the next dry-run as
  'changed outside Kno' and is otherwise overwritten."*

  **Trigger, corrected** *(F1)*: the trigger first proposed here — "when a second `KnowledgeBase`
  adapter lands" — **self-satisfies**. Step 2 of this very plan ships `adapters/knowledge/http`
  beside the Qdrant adapter, deliberately, as the falsifier for the interface. An entry whose trigger
  is discharged by its own PR is a carryover wearing a disposition, which is almost verbatim what
  #78's own history calls out, and it is the one thing the ledger rules exist to stop. The trigger is
  therefore: **when a *third* `KnowledgeBase` adapter lands, or when any target Kno supports exposes
  optimistic concurrency (a version, ETag, or compare-and-set) — whichever is first.** Neither half
  can be reached by this plan's PR series, which ships exactly two adapters against a target with no
  concurrency primitive. Owner: @devarispbrown.

## Acceptance criteria

1. `kno export --destination knowledge_base --kb qdrant:...` **without** `--apply` exits 0, prints
   `N to write / M unchanged / K changed` and the target, and issues **zero** write or delete
   requests — asserted by a fake `KnowledgeBase` that fails the test if `Write` or `Remove` is
   called.
2. `--apply` on a non-TTY without `--yes-write` exits 1 with `errs.ErrConfirmationRequired` and
   issues zero writes. `--yes` alone does **not** satisfy it: a test passes `--yes --apply` on a
   non-TTY and asserts the same refusal.
3. `--apply --yes-write` writes, and the run records `RUN_STATUS_COMPLETED` with a
   `KnowledgeBaseWritten` event whose `dry_run` is false and whose counts match the plan the dry-run
   printed for the same inputs.
4. Two consecutive `--apply --yes-write` runs of the same Portfolio: the second reports `0 to write,
   N unchanged`, issues zero `Write` calls, and the fake KB's entry count is unchanged.
5. Changing one Asset's content and re-applying updates exactly that entry: one `Write`, `N-1`
   skipped, and the entry's `kno_content_sha256` changes while every other entry's is byte-identical.
6. A target holding Kno-managed entries from a **different** `select_run_id` is refused without
   `--force`, and the refusal names both run IDs. With `--force` it proceeds.
7. A target identifier that exists **without** `kno_managed` is refused **even with `--force`**,
   naming the identifier, and zero writes are issued.
8. A collection that does not exist is refused with a fix that does **not** offer to create it.
9. `--embed-model` whose output dimension differs from the collection's configured vector size is
   refused before any write, naming both numbers.
10. Every written payload's key set is exactly the Step 6 allowlist. A test asserts that no key
    matching `delta`, `score`, `interval`, `gain`, or `rejection` appears in any payload, driven
    over a Portfolio whose entries carry all of them.
11. `kno_select_run_id` in the payload equals `select_run_id` in `kno export --json` for the same run
    — one assertion over both outputs.
12. The undo manifest is written **before** the first batch: a fake KB that panics inside the first
    `Write` still leaves a manifest on disk covering that batch's identifiers.
13. `kno export --undo <export-run-id>` removes exactly the manifest's identifiers and nothing else;
    a test plants a non-Kno entry and asserts it survives.
14. A crash after batch 1 of 3 leaves the run `RUN_STATUS_FAILED`, exits 1, names the undo command,
    and does **not** report success — a test asserts the exit code is not 0 and the phrase "partial"
    appears with the count actually written.
15. Measurement mode: `WithKnowledge` writes exactly one entry under the ephemeral scope, returns
    the **inner agent unchanged** (asserted by pointer identity), and `rollback()` removes exactly
    that entry.
16. A `rollback()` that errors fails the run, emits `KnowledgeWriteRolledBack`, and the error names
    the scope and Asset ID. A test asserts the failure is not swallowed and the run does not report
    completed.
17. A run interrupted mid-valuation leaves ephemeral entries; the next process of the same run
    sweeps them before its first measurement, and `kno doctor` lists any that remain from other runs.
18. A `Valuation` produced through the wrapper carries `mode = INJECTION_MODE_KNOWLEDGE`; one
    produced without it carries `INJECTION_MODE_CONTEXT`. A test asserts the two are never mixed
    within one Valuation.
19. Asking for knowledge mode with an agent that is not a `KnowledgeInjector` is refused at wiring
    time with `errs.ErrCapabilityUnsupported` — before any Case runs, not per Case.
20. `Capabilities().KnowledgeWrite` is true **iff** the value implements `core.KnowledgeInjector`,
    asserted for the wrapper and for every existing adapter, in the shape of
    `anthropic/config_test.go:112`.
21. A `KIND_KNOWLEDGE` Asset with a `INJECTION_MODE_KNOWLEDGE` Valuation routes to
    `DESTINATION_KNOWLEDGE_BASE`; the same Asset with a context-mode Valuation routes to
    `DESTINATION_CONTEXT`. An explicit `Asset.destination` still wins over both.
22. `--max-knowledge-base-bytes` binds: a Portfolio exceeding it rejects entries with
    `COST_DOMINATED` and the byte total in the plan never exceeds the cap.
23. Embedding calls flow through `budget.Guard`: a cap below one embedding's estimate refuses before
    any KB write, exits 2, and issues zero `Write` calls.
24. No credential appears in any request log, span, error, event, fixture, or the undo manifest; the
    secrets scan covers `adapters/knowledge/`.
25. `endpointsec` applies: a plain-HTTP target is refused without `--allow-insecure-base-url`, a
    private address without `--allow-private-address`, and a redirect is refused unconditionally.
26. Missing `QDRANT_API_KEY` for a non-private host is refused at construction with the `QDRANT_AUTH`
    fix text naming the variable and stating no file, profile, or metadata service is read.
27. The dry-run plan and the applied result are the same numbers for an unchanged target: a golden
    pins both renderings, and `--json` emits one document whose counts equal the human ones.
28. `kno export --help` mentions `--kb`, `--apply`, `--yes-write`, `--undo`, `--embed-model`, the
    sentence that Kno can remove what it wrote but cannot restore what was there, and the sentence
    that undoing an update deletes the entry rather than reverting it. `kno value --help` mentions
    `--kb`, `--embed-model`, and `--injection-mode`. Snapshot-tested.
29. `make typecheck-proto` passes: the two event members are additive and `buf breaking` is green.
30. *(F3)* Undo is idempotent and honest about deletion. A second `kno export --undo <id>` on an
    already-undone run exits 0, issues `Remove` only for identifiers that are already gone, errors on
    none of them, and rewrites no manifest. A golden pins the `--undo` prompt and asserts it contains
    the literal sentence that undo **deletes** entries that existed before the run rather than
    reverting them, with the count of such entries. A test drives update-then-undo and asserts the
    pre-existing entry is **absent** afterwards — the loss is asserted, not assumed.
31. *(F2)* Mode selection, all three arms: (a) `kno value --kb ... --embed-model ...` produces
    `INJECTION_MODE_KNOWLEDGE` Valuations for every `KIND_KNOWLEDGE` Asset with no further flag, and
    those Assets then route to `DESTINATION_KNOWLEDGE_BASE`; (b) `--injection-mode knowledge` without
    `--kb` is refused with `errs.ErrInvalidInput` naming `--kb`, and no Case runs; (c) a run with no
    `--kb` produces context-mode Valuations and routes **zero** Assets to
    `DESTINATION_KNOWLEDGE_BASE`.
32. *(F4)* Negative space for the ownership predicate: a property test over a collection seeded with
    many foreign entries — random ids in both legal Qdrant id forms, foreign payload keys including
    near-misses such as `kno_asset_id` **without** `kno_managed`, and ids that collide with the
    `uuidv5` namespace — asserts **zero** writes, updates, or deletes touch any entry lacking
    `kno_managed: true`, across the whole generated space rather than one planted example. The same
    property is asserted for `--force` and for `--undo`. A mutation that drops the `kno_managed`
    check from `Inspect` must make it fail.

## Alternatives considered

**A docs platform first (Notion or Confluence).** Genuinely attractive: the cookbook already has
`notion.md` and `confluence.md`, so the audience exists and the shape is familiar. Rejected on the
definition in Step 1 — writing to Notion does not put an Asset in front of the agent's retriever, it
puts it in front of an ingest job that runs on a schedule Kno cannot see. Measurement mode against
it would measure the ingest schedule; export mode against it would ship a number produced by a
different mechanism than the one it acts through. It is a good *pool* source, which is exactly what
those cookbook entries already make it.

**Put `KnowledgeBase` in `adapters/` instead of Ring-0.** Avoids a core change, which is the
conservative instinct. Rejected: `cli`, `api`, and `tui` must construct one and hand it to the
engine — the same shape as `resolvePool` returning a `core.Pool` — so an adapter-package interface
would force the shells to name concrete types and would make a second adapter a change to `core`'s
call sites rather than a registration. `Tuner` is the precedent, and the boundary test
(`TestCoreImportsNothingAbove`) stays green because the interface imports nothing.

**One flag for both consents (`--yes` covers the write).** Fewer flags, simpler help text.
Rejected in Step 3(b): `--yes` is the flag people paste into CI, and the two risks — spending money
and mutating a production index — have different blast radii and different people who should be
asked.

**Real rollback: snapshot the prior entries and restore them on undo.** The honest-sounding answer.
Rejected as a promise Kno cannot keep: restoring a vector requires having stored the vector, which
means Kno holding a copy of the user's index content on local disk — customer data outside the
purge story, in a file nobody expected. And it would still be wrong under a concurrent writer. The
narrower promise (remove what we wrote, never touch what we did not) is one that can actually be
tested, and criteria 7 and 13 test it.

**Make `--apply` the default and gate on a confirmation alone.** Matches how most write-capable CLIs
behave. Rejected: `core.Export`'s contract today is that it never touches the Destination, and a
default that inverts it means every existing script and cookbook line changes meaning silently at
upgrade. A new flag changes meaning only for people who type it.

**Skip measurement mode; ship only the writable Destination.** Half the work, and it is what the
task's framing of "v0.2 makes it writable" most directly asks for. Rejected: the Assets would be
routed to the knowledge base on the strength of a **context-mode upper bound**, applied through a
retriever that was never measured. `what-the-numbers-mean.md` would then be wrong on the page where
it is most careful. Shipping the write half alone is shipping the dishonest half.

## Affected packages

`core/` (`KnowledgeBase` + supporting types on `ring0.go`; `destinationFor` routing rule in
`select.go`; the knowledge-base branch of `Export`; `max_knowledge_base_bytes` in the Select budget);
`adapters/knowledge/qdrant` and `adapters/knowledge/http` (new); `adapters/agent/knowledge` (the
`Wrap` injector); `adapters/internal/endpointsec` (reused unchanged); `adapters/agent/openaicompat`
(embeddings path — a new method, not a new adapter); `store/` (unchanged — the undo manifest is a
file, deliberately, so it survives a lost database); `proto/kno/v1/event.proto` (two additive oneof
members); `cli/` (`--kb`, `--apply`, `--yes-write`, `--undo`, `--embed-model`, `--injection-mode`,
`--max-knowledge-base-bytes`, the write-confirm prompt, the undo prompt, `--json` shapes, help
snapshots); `tui/`
(two renderers); `docs/` (mental model, what-the-numbers-mean, a "Write a portfolio into your
knowledge base" cookbook entry, the retention page for the ephemeral-sweep behavior, the adapters
matrix); `docs/debt.md` (#78 repaid via a linked plan, one new entry); `CHANGELOG.md`.

## Proto / schema impact

Verified against `proto/kno/v1/`. **Additive only.**

| Change | File | Note |
|---|---|---|
| `KnowledgeBaseWritten knowledge_base_written = 26` | `event.proto` | oneof currently ends at `export_written = 25` |
| `KnowledgeWriteRolledBack knowledge_write_rolled_back = 27` | `event.proto` | |

Nothing else. `InjectionMode.INJECTION_MODE_KNOWLEDGE`, `Destination.DESTINATION_KNOWLEDGE_BASE`,
`Capabilities.knowledge_write`, and `Budget.max_knowledge_base_bytes = 6` all already exist — this
plan is the first code to **populate** them, which is a behavior change with a docs obligation but
not a schema one. `core.KnowledgeBase` and its `Scope`/`WriteResult`/`KnowledgeState`/
`KnowledgeTarget` types are Go, not proto: they are an adapter contract, never on the wire, and
putting them in the schema would commit a plugin protocol to a surface that has one implementation.
`buf breaking --against main` passes. **A conflict to note against the bridge plan**: both propose
`event.proto` member 26, and the bridge plan now claims four consecutive members (26–29) rather than
three, so the collision is wider than when this section was written. Whichever plan merges first
takes the numbers it names and the other continues from the next free tag — proto-first is the
coordination rule and the numbers are assigned at the proto PR, not in these documents. Neither plan
may assume its own numbering survived; both consume generated code.

## Edge cases

| Case | Behavior |
|---|---|
| KB rejects a write (403) | Actionable refusal naming the host and the variable; nothing further is attempted; the undo manifest already on disk covers whatever landed |
| KB rejects a write (409 / schema or dimension mismatch) | Refused at `Describe` time where possible; at write time the provider's error is quoted verbatim and the run fails — never retried into a different collection |
| KB rate-limits | `Retry-After` honored through the existing limiter; batches resume; a write batch is idempotent so a retried batch is safe |
| Partial write (batch 2 of 5 fails) | Run is `RUN_STATUS_FAILED`, exit 1, the message states how many entries landed and names `kno export --undo`. Never "completed with warnings" |
| Crash mid-batch | The manifest, written before the batch, is a superset of what landed. `--undo` removes the superset — removing an entry that was never written is a no-op |
| Concurrent writer changes an entry Kno owns | Last-writer-wins; detected on the next dry-run as `changed`. Accepted risk with a ledger entry |
| Concurrent Kno process, same Portfolio | Same identifiers, same content hash ⇒ converges. Both runs write their own undo manifest; either can undo |
| Concurrent Kno process, different Portfolio | The `select_run_id` refusal (Step 4) fires for whichever runs second, without `--force` |
| Re-export, nothing changed | Zero writes; plan says `0 to write, N unchanged`; exit 0 |
| Re-export after Assets were removed from the Portfolio | Entries Kno wrote and no longer selects are reported as `orphaned` in the plan and are **not** deleted implicitly; `--prune` opts into removing them, and it is refused without `--apply` |
| Embedding endpoint fails mid-write | The batch is not written; spend already settled for successful embeddings stays settled; run fails resumable |
| Embedding model unpriced | Refused unless `--price-input-per-mtok`, exactly as the agent path already behaves |
| Vector dimension mismatch | Refused at `Describe`, before anything is written or embedded |
| Collection absent | Refused; Kno never creates one |
| Key revoked mid-write | Refusal naming the variable; the manifest covers what landed; the message names `--undo` |
| Key missing, private-address target | Allowed (an unauthenticated local Qdrant is legitimate), the same asymmetry `anthropic.resolveKey` implements |
| Measurement-mode rollback fails | Run fails, event emitted, error names scope + Asset ID. Never swallowed, never `_ = rollback()` |
| Measurement-mode crash before rollback | Ephemeral entries survive; swept by the next process of the same run; `kno doctor` reports leftovers from other runs |
| Measurement mode against a KB the agent does not actually read | Undetectable by Kno, and the honest answer is documentation: knowledge mode measures the index you point it at, and pointing it at the wrong one produces a delta of ~0 with a wide interval. Named in `what-the-numbers-mean.md` |
| Asset content is not UTF-8 | Refused before embedding, naming the Asset — the same rule `injectable()` already applies for context mode |
| Zero Assets destined for `KNOWLEDGE_BASE` | "Nothing to write" — legal, reported, exit 0, no prompt, no manifest |
| `--undo` for a run whose manifest is missing | Refused with `errs.ErrInvalidInput` naming the expected path. Kno does not reconstruct the set from the KB — that would delete by inference |
| `--undo` run twice | Idempotent no-op, exit 0. Identifiers already gone are removed without error; no manifest is rewritten; nothing is inferred from the KB |
| `--undo` of a run that **updated** pre-existing entries | Those entries are **deleted, not reverted** — the manifest holds their prior hash, not their prior content. The prompt says so in its own words before anything is removed |
| `--injection-mode knowledge` without `--kb` | Refused with `errs.ErrInvalidInput` naming `--kb`. Never silently downgraded to context mode: that would relabel a deployment prediction as an upper bound |
| Self-hosted Qdrant with no `--embed-model` | Refused. Server-side inference is Qdrant-Cloud-managed only, so there is no mode in which the OSS deployment embeds for Kno |
| Holdout | Unchanged: Select and Export read dev-side Valuations, the seal applies, and the canary tests extend to the KB path |

## Test plan

- **A fake `KnowledgeBase`** with programmable failures (reject, partial, rate-limit, panic
  mid-batch) drives every row of the edge-case table. It fails the test on any `Write`/`Remove`
  during a dry-run — that is criterion 1's mechanism, not an assertion after the fact.
- **Qdrant adapter against recorded fixtures**, in the repo's existing shape: an allowlisted
  fixture directory (`request.json`, `response.json`, `status`, `note.txt`), **no headers recorded in
  either direction**, and a key-material scan. Recording is cheap here — a local Qdrant costs
  nothing — so a `docker`-gated integration test is added to `make test-live` and, unlike the
  bridge, it *is* eligible for the nightly matrix.
- **Idempotence and determinism**: two applies, content-change detection, identifier stability across
  processes and across map-iteration order, golden plan renderings.
- **Undo**: manifest-before-batch ordering (asserted from inside the fake's `Write`); undo removes
  exactly the manifest set; a planted foreign entry survives; undo of a partial run; **double undo**
  as an idempotent no-op; and update-then-undo asserting the pre-existing entry is deleted rather
  than reverted, with the prompt golden that says so *(F3)*.
- **Ownership, as negative space rather than one example** *(F4)*: a property test over a collection
  full of generated foreign entries (both legal id forms, near-miss payload keys, namespace
  collisions) asserting zero false-positive writes, updates, or deletes — the single planted entry
  stays as a readable regression case, but the property is what carries the stakes. Plus the
  `kno_managed` refusal including with `--force`; the cross-`select_run_id` refusal; the payload-key
  allowlist with a Portfolio full of measurement numbers.
- **Measurement mode**: pointer identity of the returned agent; rollback exactness; rollback-failure
  propagation (verified failing when the propagation is removed); the sweep; the
  `Capabilities`/interface agreement test extended to every adapter.
- **Routing and mode selection** *(F2)*: the mode→Destination rule as a table test; the three arms of
  criterion 31 (`--kb` implies knowledge mode; `--injection-mode knowledge` without `--kb` refused;
  no `--kb` routes nothing to `KNOWLEDGE_BASE`); explicit `Asset.destination` still wins; a
  context-mode knowledge Asset never reaches `KNOWLEDGE_BASE`.
- **Budget**: embedding spend through the Guard; the cap refusal before any write;
  `--max-knowledge-base-bytes` binding with `COST_DOMINATED` rejections.
- **Security**: `endpointsec` matrix; credential-refusal matrix; the secrets scan over
  `adapters/knowledge/` including the undo manifest; a sentinel-content test asserting Asset content
  appears in no event, log, span, or manifest.
- **Holdout canary** extended to the KB export path.
- **CLI**: help snapshots; `--json` equivalence golden; exit codes 0/1/2 per the table.

## Rollback

Delete `adapters/knowledge/*`, `adapters/agent/knowledge`, the CLI flags, and the knowledge-base
branch of `Export`. Three things do **not** roll back cleanly and are named: the `core.KnowledgeBase`
Ring-0 addition (removable, but it is a public-surface removal and needs a CHANGELOG note under the
pre-1.0 allowance); the `destinationFor` routing rule (reverting it re-strands the
`KNOWLEDGE_BASE` Destination, so it should be reverted only together with the export branch); and
anything already written into a user's KB, which is the entire reason `--undo` exists — the rollback
story for a shipped write is the user's command, not ours. The two proto members are additive and
inert once the code is gone.

## Docs impact

`docs/mental-model.md` — "Injection modes: what a number is a claim about" gains the sentence that
the Destination now follows the mode, and the write half gets a paragraph on dry-run-by-default.
`docs/what-the-numbers-mean.md` — the existing "Context injection is an upper bound" section already
promises knowledge mode in the present tense; it becomes true and gains what a knowledge-mode number
requires (that the index Kno writes is the index the agent reads) and what it cannot detect (a KB
the agent does not actually query). New cookbook entry "Write a portfolio into your knowledge base",
whose first step is the dry run and which states the undo promise in the same words as the CLI,
including that undoing an update deletes rather than reverts. The cookbook and the CLI help both
state that `--embed-model` is required for self-hosted Qdrant, without a conditional.
`docs/cookbook/retention.md` gains the ephemeral-sweep behavior and what `kno doctor` reports.
Adapters matrix gains a KnowledgeBase column and flips `KnowledgeWrite` for the wrapper. CLI help
snapshots. CHANGELOG under Unreleased. `docs/debt.md`: #78 repaid via the linked trial-splitting
plan and carrying its 2026-09-14 go/no-go date, one new entry with a non-self-satisfying trigger.

## Accepted risks

- **Last-writer-wins.** Kno takes no lock because it owns none. Detected on the next dry-run,
  disclosed, and tracked as a new ledger entry with a non-self-satisfying trigger.
- **Undo is compensation, not restoration — and undoing an *update* is a deletion.** *(F3)* The
  manifest holds a prior entry's hash, not its content, so `--undo` on a run that updated a
  pre-existing Kno-managed entry removes it rather than restoring what it held. That is a stronger
  loss than "cannot restore" and it is said in those words in the undo prompt, the CLI help, the
  docs, and the cookbook. The alternative (snapshotting the user's index content to local disk) was
  rejected as a worse promise, not as a smaller one.
- **`--yes-write` is one flag away from the muscle memory `--yes` already has.** *(F6)* Step 3(b)
  argues the split and the argument holds — two risks, two words — but the honest statement belongs
  here too: the moment `--yes-write` exists, it can be pasted into CI beside `--yes`, and the second
  gate becomes as automatic as the first for anyone who does. What the design keeps is that it takes
  a *deliberate second act* to get there, that `--apply` is still required alongside it, and that the
  ownership predicate and the cross-`select_run_id` refusal still stand in front of the blast radius
  even when both consents are waived. Mitigation is naming, help text, and the fact that the flag
  appears in no example, cookbook line, or vhs tape in this repo — never a claim that the risk is
  gone.
- **Kno cannot verify the agent reads the index it wrote.** A misconfigured target produces a delta
  near zero with a wide interval — which is the correct output for "nothing happened", but is
  indistinguishable from a genuinely worthless Asset. Named in the epistemics page.
- **Embeddings are a second provider.** The plan makes it a priced, guarded, explicitly-flagged spend
  path rather than an implicit one, but it is still a dependency the context-injection path did not
  have.
- **The Ring-0 surface grows.** `KnowledgeBase` is a pre-1.0 addition to the public Go API. Justified
  in Step 1, and pre-1.0 the public surface is `core`, the adapter interfaces, and the generated
  proto — this is the first of those, deliberately.
- **The Qdrant API facts this plan leans on are now checked; the remainder are still
  ***(verify)***.** Phase 1 settled the four endpoints, the id restriction (unsigned 64-bit int or
  UUID, arbitrary strings rejected), the discoverable vector size, and the Cloud-only scope of
  server-side inference. What is left is tagged, and the design is arranged so a wrong one costs an
  adapter method and a fixture, not the consent model or the undo model.

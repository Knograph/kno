# Generated status data: `docs/status.json`

One artifact, after review: a **generated file** that reports what a release shipped. The website
needs it. The temptation is to build a `kno status` command and point the site at *that*, and it is
the bug this plan exists to avoid — a command reports the binary in front of you, which is a
different document. The first draft proposed both; the command is **cut** (§2), and the argument for
why the two are different is kept, because it is the argument that has to be quoted the next time
somebody proposes merging them.

**Phase-1 re-reviewed 2026-08-30 — verdict: amend; amendments applied.** The P0 the first draft
missed is an ordering hazard inside the release process itself: release-please bumps
`.release-please-manifest.json` in an **ordinary PR against `main`**, and that PR runs `make check`
— so a `released_version` derived from that file makes `status-check` fail on **every** release PR,
and the new gate blocks the release it exists to describe *(F1)*. Resolved by removing the field:
the git ref the site fetches at already carries the version, and a second copy of that fact inside
the file is the exact rot this plan is about. Second, no consumer of `kno status --json` exists
anywhere in the repo, and "nearly free to add" is precisely the trap §5's own SemVer paragraph
warns about — pre-1.0 CLI surface becomes a post-1.0 covenant — so the command is cut and deferred
behind a trigger *(F2)*. The merge-conflict tax of a committed generated file *(F3)* and the
cross-repo `schema_version` protocol *(F4)* are now specified rather than assumed, and every
***(verify)*** about the website is resolved against `uknoAI/kno-www` *(F5)*.

The facts the first draft asserted were re-checked and stand: `identity()`, `doctorVersion()` and
`String()` in `cli/root.go`; `.release-please-manifest.json` is `{".":"0.1.1"}`; the nine registered
commands; `docs/debt.md`'s 84 `id="…"` anchors; the filename-scoped `encoding/json` depguard
exemption; `scripts/ledger-check.py`'s row scan supporting an additive `--json` without becoming a
second parser; and `make docs` (`Makefile:398`) / `make generate-check` (`Makefile:337`) as the
precedents `status-check` copies. The `Stage` enum was confirmed to hold exactly `BASELINE`,
`VALUE`, `SELECT`, `VALIDATE`, `EXPORT` and **no `REPORT` member** — §4's cross-checks already
tolerate a shipped command with no enum member, and §4 now makes that tolerance structural in the
README rather than implicit in a test.

## Problem

The project's status is asserted in five kinds of place — **seven files across two repositories** —
none of which can disagree with the others *mechanically*:

| Where | What it claims | How it stays true |
|---|---|---|
| `README.md` "## Status" | A six-row table: Baseline / Value / Select / Export / Report **Shipped**, Validate **Planned** | Hand-edited |
| `cli/root.go:113-116` (root `Long`) | "Today baseline, value, select, and export run; validate arrives next" | Hand-edited prose in help text |
| `DESIGN.md:397-400` | Milestones v0.1–v0.4 | Hand-edited |
| `proto/kno/v1/run.proto:13-33` | `Stage` enum: `BASELINE`, `VALUE`, `SELECT`, `VALIDATE`, `EXPORT` | Generated from proto — **and it already contains `VALIDATE`, which has not shipped** |
| The website's roadmap/status section, in `uknoAI/kno-www` (Astro) | Hand-maintained in **three** places *(F5)*: `src/content/docs/roadmap.md`, `src/content/home/home.yaml`, and a literal `Status · v0.1.1` label in `src/pages/index.astro` | Hand-edited, three times, in a repo the person shipping the stage is not working in |

Three of these are already inconsistent in ways a reader can find today:

- The README table lists **Report** as a stage. `Stage` in `run.proto` does not have a `REPORT`
  member — `report` is a command that composes recorded stages, not a pipeline stage. So "the
  stage list" means two different things depending on which document you read.
- The README table omits `init`, `mine`, `doctor` and `purge`, all of which are real commands
  registered in `NewRootCmd` (`cli/root.go:126-134`).
- `DESIGN.md:398` puts `validate` in **v0.2**; `DESIGN.md:399` puts post-fine-tune validation
  (`validate --agent`) in v0.3; the README says "Planned" without a version. Released version is
  `0.1.1` (`.release-please-manifest.json`).

The day `validate` ships, every one of these needs a human edit, and the website's copy is the one
furthest from the person making the change. That is the rot.

There is also a second, quieter problem worth naming: **`docs/debt.md` already has a machine
consumer.** `scripts/ledger-check.py` parses it with `re.search(r'id="(\d+)"', line)` and
`line.split("|")`, and its module docstring is an argument for staying narrow — "It does NOT parse
prose dates or evaluate arbitrary conditions. A checker that guessed at 'when a second Tuner lands'
would be wrong often enough to get switched off, and a gate people switch off is worse than no gate
--- which is what `docs/debt.md#70` is about." Any status artifact that wants a ledger count must
not become a second, more ambitious parser of the same table.

## Design

### 1. What "status" means, and where each field's truth lives

The document answers one question: **what does this release of Kno do, and how honest is it being
about the parts it does not do yet?** Every field is either derived or explicitly declared, and the
declared ones are visible as declared.

| Field | Truth source | Derived or declared |
|---|---|---|
| ~~`version`, `commit`, `built_from`~~ | The build stamp — `identity()` (`cli/root.go:52-85`), preferring goreleaser's `-X` ldflags, falling back to `debug.ReadBuildInfo()`'s `Main.Version` and `vcs.*`. **Not in this artifact.** These were the *command's* fields, and the command is cut (§2); a build stamp in a committed file also fails the drift gate on every dirty tree (§7) | **Cut** *(F2)* |
| ~~`released_version`~~ | Was `.release-please-manifest.json`. **Removed** *(F1)*: release-please bumps that file in a PR that runs `make check`, so deriving from it breaks the gate on every release PR (§7). The version anchor is the **git ref the site fetches at** — a fact the consumer already holds and that cannot go stale | **Cut** *(F1)* |
| `stages[].name` | `knov1.Stage`'s enum values, `proto/kno/v1/run.proto:13-33` — the schema is already the vocabulary's source of truth (ADR-0001) | **Derived** |
| `stages[].command` | Whether `NewRootCmd()` registers a command of that name. Walkable: `root.Commands()` | **Derived** |
| `stages[].shipped` | See §4 — a declared list, checked against the command tree | **Declared, then verified** |
| `stages[].milestone` | `DESIGN.md`'s milestone the stage belongs to | **Declared** |
| `commands[]` | `NewRootCmd()` walked: `init`, `baseline`, `mine`, `value`, `select`, `export`, `report`, `doctor`, `purge` | **Derived** |
| `adapters[]` | `cli/doctor.go`'s `adapterFacts()` — already the repo's answer to "what can this build do", already hand-written with a drift test, already shaped for a `--json` contract | **Derived (reused)** |
| `goals[]` | `runDoctor`'s `Goals: []string{"exact-match"}` (`cli/doctor.go:154`) | **Derived (reused)** |
| `price_table` | `pricing.Version` | **Derived (reused)** |
| `debt.open`, `debt.total` | `docs/debt.md`, via **`scripts/ledger-check.py`**, extended with a `--json` mode. Not a new parser | **Derived** |

**The ledger decision is the load-bearing one.** `scripts/ledger-check.py` gains an optional
`--json` output that emits `{"total": N, "open": M, "entries": [...]}` using the *same* row scan
and the *same* `DISPOSITIONS` tuple it already uses for the release gate. Nothing else parses
`docs/debt.md`. The release gate's exit-code behaviour is unchanged, so the thing CI depends on
keeps working exactly as it does at `release.yml:124`.

The rejected shape — a Go parser in the status generator — would be a second reader of a 137 KB
hand-written table with 84 rows, and it would drift from the Python one the first time a row is
formatted unusually. It would also drift *silently*, because the two consumers ask different
questions and would disagree without either failing.

### 2. Generator only — the command is cut, and why the distinction still matters *(F2)*

The first draft shipped two artifacts. The distinction between them is correct and is kept, because
it is the reason the site cannot be pointed at a binary:

> **A `kno status` command would report the state of the binary you are holding. `docs/status.json`
> reports the state of a release. Conflating them is a bug, because they disagree in exactly the
> cases that matter.**

A user running `kno status` on a `go install`ed dev build gets `version: "dev"` (or a pseudo-version
from `ReadBuildInfo`), possibly `commit: "…-dirty"`, and whatever stages *that tree* has. The
website must never render that. Conversely `docs/status.json` cannot report the user's build,
because it is a file in a repo. That argument survives review intact.

**What does not survive is the case for building the command at all.** The first draft justified it
on two grounds, and both fail:

- *"It answers a real user question — does my installed `kno` do `validate` yet?"* `kno --version`
  plus the published site already answers that for a human, and `kno doctor --json` already answers
  the adjacent question about adapters and models. The stage list a user cares about is a property
  of the *release* they installed, which is what the website renders.
- *"It is nearly free once the renderer exists."* This is the trap §5's own SemVer paragraph names.
  A CLI command and its JSON keys are surface: pre-1.0 they may break with a CHANGELOG notice, and
  post-1.0 CLAUDE.md makes exit codes and the CLI contract covenants. "Nearly free" measures the
  cost of writing it, not the cost of keeping it. A command with no named consumer is a covenant
  bought on credit.

**No consumer exists.** `grep -rn 'kno status\|status --json\|status\.json'` across the repo —
Go, workflows, `Makefile`, docs — returns nothing but an unrelated `SourceStatus` proto field. No CI
check, no plugin, no script, no cookbook entry wants it. The website wants the *file*, at a tag.

**So the deliverable is `make status` → `docs/status.json`, and nothing else.** The generator is a
Go program in `internal/cmd/` (alongside `godoccheck`) that imports the `cli` package to reuse
`adapterFacts()`, the `Goals` list and `pricing.Version` rather than re-deriving them — the reuse
argument was always the real one, and it does not require a cobra command to hold it. The derived
fields, the declaration table of §4 and its cross-checking tests all stay exactly as planned; they
were never the command's, they were the *data model's*.

**Deferred, not deleted.** `kno status` is recorded in `docs/debt.md` with a trigger that cannot
self-satisfy: *when a named consumer appears — a CI check, a Ring-2 plugin, or a user asking for it
in an issue.* At that point the command is a thin printer over a struct that already exists and a
renderer that is already tested, so deferring costs almost nothing and buys the one thing that
matters — the surface is added when somebody needs it, on evidence, rather than because it was
cheap.

### 3. Why the ledger counts belong in a generated file and never in a binary

With the command cut *(F2)* this is no longer a decision between two shipping artifacts, but the
argument is retained deliberately, because "just embed the count" is the obvious shortcut and it is
the first thing that will be proposed if `kno status` is ever revived.

A binary does not ship `docs/debt.md`, and embedding the ledger — or a generated count — into the
binary freezes the number at build time, where it drifts from `main` immediately. It would make a
debt figure a function of *when the user's binary was built*: a number that looks authoritative and
means almost nothing. `docs/status.json` is regenerated from the tree it describes, so its count is
true of that ref by construction.

### 4. Keeping "shipped vs planned" honest without a human editing a list

This is the hard part and the plan must not pretend otherwise.

**Full derivation is impossible and it is worth saying why.** `Stage` in `run.proto` already
contains `STAGE_VALIDATE`. The schema is written ahead of the implementation on purpose — CLAUDE.md's
"proto first" rule *requires* it. So enum membership cannot mean "shipped". Neither can command
registration alone: `kno report` exists and is not a `Stage`; a stage could plausibly land as
library-only before it gets a verb. And "the tests pass" cannot mean shipped either — a stage can be
half-implemented behind passing tests for the half that exists.

**So `shipped` is declared, in exactly one place, and the declaration is cross-checked.** The single
place is a Go table in `cli/` — not the README, not YAML, not a Markdown table — because Go is where
the cross-checks can run. Something like:

```
// stageFacts declares which Stage values this build implements. Declared,
// not derived: the enum carries STAGE_VALIDATE before validate ships,
// because proto leads implementation (CLAUDE.md, "proto first").
```

Three mechanical checks make the declaration hard to get wrong, and they are the actual deliverable:

1. **Exhaustiveness.** A test asserts the declared table names every `knov1.Stage` value except
   `STAGE_UNSPECIFIED`. Add a stage to the proto without deciding its status and the test fails.
   This is the check that makes the enum's growth visible.
2. **Command agreement.** For each stage declared `shipped: true`, a test asserts a command of that
   name is registered by `NewRootCmd()`. Claim `validate` ships without a `kno validate` and the
   test fails. The converse is *not* asserted — a command without a shipped stage is legal
   (`doctor`, `purge`, `init`, `mine`, `report`).
3. **README agreement, and the `Report` problem it forces us to fix.** A test parses `README.md`'s
   `## Status` table and asserts its rows and `Shipped`/`Planned` words match the declaration. As
   written today that test **cannot pass**: the README table has a `Report` row and the `Stage`
   enum has no `REPORT` member (re-confirmed in review — the enum is exactly `BASELINE`, `VALUE`,
   `SELECT`, `VALIDATE`, `EXPORT`). Check 2 already tolerates a command with no enum member in the
   *code* direction; the README is where the tolerance has to become visible. So this PR
   **restructures the table into two**: a **Stages** table, asserted row-for-row against the
   declaration, and a **Commands** table, asserted against `NewRootCmd()`'s registration — which
   also fixes the omission of `init`, `mine`, `doctor` and `purge` noted in §Problem. Both keep
   their prose "What it does" column. Conflating the two meanings in one table is *how* the
   inconsistency arose; a checker that papered over it with an exception list would preserve the
   confusion and add a list to maintain.

**What was gained, stated honestly.** A human still writes `shipped: true` once. What changed is
that they write it in one place, and three documents — `README.md`, `docs/status.json` and the
website — become consequences of that one edit rather than three independent edits, two of which get
forgotten. (The first draft counted four, the fourth being `kno status`; the command is cut *(F2)*,
which removes a consumer without weakening the argument.) The README audit in §Problem (Report listed as a stage; `init`/`mine`/`doctor`/
`purge` missing; DESIGN and README disagreeing on `validate`'s milestone) is the evidence that the
four-independent-edits model has already failed at n=1 release.

### 5. The JSON schema and its stability promise

**Hand-written Go structs, `encoding/json`, in `cli/`, under ADR-0001's existing exemption.** The
structs and the renderer live in a new `cli/status.go` — a file, not a command *(F2)* — and
`internal/cmd`'s generator calls into it. Keeping them in `cli/` is what preserves the exemption
story below; moving them to `internal/cmd` would need a second depguard entry for no gain.
`cli/jsonreport.go`'s file header states the exemption's terms precisely: the ban exists because
"proto3 JSON encodes int64 as quoted strings and enums as names, so using it on a `kno.v1` type
silently diverges from the generated OpenAPI spec … That reasoning is about `kno.v1` types. This
file encodes a hand-written struct aimed at somebody's jq pipeline." Status is exactly that case,
so it reuses `writeJSON` (`cli/jsonreport.go`) and lives in the same file or a sibling covered by
the same depguard exemption. **The exemption is filename-scoped**, so extending it is a deliberate
config edit and a review item, not an accident.

Two consequences the schema must respect, both borrowed from precedent already in the tree:

- **Stage names are the enum's short names, lowercased** (`baseline`, `value`, `select`, `validate`,
  `export`) — not `STAGE_BASELINE`, and not the proto's integer. The CLI already does this kind of
  mapping deliberately (`statusName`, `destinationName`, `rejectReasonName` in `cli/`).
- **There is no version key at all** *(F1)*. Not `version`, not `commit`, not `released_version`.
  The artifact's version anchor is the git ref the site fetches it at (§6), which the consumer
  already has and which cannot go stale. If a version key is ever reinstated, `cli/root.go`'s
  comment on `String()` states the constraint it must satisfy — "appending a commit hash to a value
  consumers parse as a version is a breaking change wearing a cosmetic disguise" — so it would be
  the bare version, matching `doctorVersion()` and the test that pins it,
  `TestDoctorReportsTheBareVersionNotTheHumanString`, with `commit` as a separate key. Reinstating
  it also re-opens §7's release-PR hazard, which is the reason it is gone.

**Stability promise: the same one `doctor --json` carries, and no stronger.** Pre-1.0, per
CLAUDE.md's SemVer section, a JSON contract may break on a minor with a CHANGELOG notice. Post-1.0
it is a covenant. Concretely: **keys are added, never renamed or removed within a major; absent
means absent, not zero** (the `omitempty` discipline `jsonreport.go` already argues for at length in
its `concurrencyFields` comment).

**`schema_version`, and the cross-repo protocol it needs** *(F4)*. The first draft carried
`"schema_version": 1` and said only that "a site build can refuse a shape it does not understand".
A version number with no bump rule and no notification path is a number, not a mechanism — the
version would drift, or worse, never move because nobody was sure when it should. The protocol,
specified:

- **When it bumps.** Only on a **breaking** shape change: a key renamed, removed, or retyped, or a
  value's meaning changed (for example `shipped` ceasing to be the tri-state of §Edge cases).
  Adding a key does **not** bump — that is the additive half of the promise above, and a consumer
  that breaks on an unknown key is broken.
- **How the consumer enforces it.** `kno-www` declares the schema it understands in its content
  collection schema (`src/content.config.ts`), which is where every other content shape in that
  repo is already validated. A `schema_version` it does not recognise **fails the Astro build** —
  the same posture as §6's fetch failure, and it surfaces in the `website` workflow and the
  Cloudflare Pages preview rather than as a silently-empty roadmap section.
- **How the bump is notified.** A bump is not a merge-and-hope. The Kno-side PR that bumps
  `schema_version` must, in the same PR: record it under `Unreleased` in `CHANGELOG.md`, and file
  an issue on `uknoAI/kno-www` linking the PR and naming the changed keys. Notification is a
  deliverable of the PR, not a follow-up, for the same reason CLAUDE.md puts docs in the PR that
  changes behaviour.
- **Why this is not a hard cutover.** §6 has the site fetching at a **release tag**, so the site
  keeps rendering the last shape it understands until it is updated and re-pointed. The bump is
  therefore never an outage; it is a deadline with a working fallback, which is the only kind of
  cross-repo contract change worth signing up for.
- **Bumping is expected to be rare.** If it happens twice in a release cycle, that is the signal
  that the shape was not designed and the schema should be settled in one deliberate pass rather
  than three reactive ones. Recorded as a ledger trigger.

### 6. How the site consumes it

**Committed file in the repo, fetched at site-build time from a git ref — a release tag, not
`main`.** Three properties this buys:

- The site can render the state of the **latest release**, which is what a visitor deciding whether
  to install cares about, rather than the state of `main`, which they cannot install.
- The file is reviewable in a PR diff like any other artifact, so a status change is something a
  reviewer sees.
- It needs no release asset, no API, and no runtime service. The consumer is **`uknoAI/kno-www`, an
  Astro site** *(F5)*, whose content already lives in file-backed collections under `src/content/`
  with a schema in `src/content.config.ts`. A build-time fetch from `raw.githubusercontent.com` at
  a tag — or a checkout — lands the JSON as one more entry in that pipeline rather than as a new
  mechanism, and the version anchor is the ref itself (§5, *(F1)*): the site renders "as of
  `<tag>`" from the ref it fetched, not from a field inside the file.

**What this replaces on the site.** Three hand-maintained copies *(F5)*: `src/content/docs/roadmap.md`,
`src/content/home/home.yaml`, and the literal `Status · v0.1.1` label in `src/pages/index.astro`.
The third is the one that dates fastest and is least likely to be remembered, because it is a string
in a page component rather than content. Migrating all three is site-repo work and out of scope for
this PR; this plan defines the contract they consume, and the ordering constraint is that the JSON
ships first so the site change has something to build against.

**When the fetch fails, the site build fails.** Not a fallback to a stale committed copy, and not a
silently-empty section. A roadmap page that renders yesterday's truth because a fetch 404'd is
precisely the rot this plan is about, and it would be *invisible*. The site keeps a last-known-good
copy for local development convenience only, clearly marked, and site CI must not use it. `kno-www`
has the machinery to enforce this *(F5)*: a `website` workflow, a Cloudflare Pages preview per PR,
and Playwright e2e including a broken-link crawl — a failed fetch fails the Astro build before the
crawl ever runs, and a roadmap that renders but links nowhere is caught by the crawl. Neither check
is new; the status section simply falls under them.

**Rejected: a release asset.** goreleaser could attach `status.json` to each release. It is a
cleaner ownership story, but it makes the file invisible in the repo (no PR diff, no drift gate, no
link checker) and adds a publishing dependency to a document whose entire value is being auditable.
The committed file can *also* be attached later; the reverse is not true.

### 7. The enforcement question — does CI fail on drift?

**Yes, in `make check`, and this is the point of the whole plan.**

`make docs` (`Makefile:398-419`) already owns the class: it runs `godoccheck`, reports a `PEND` for
OpenAPI, and fails on a broken internal link. A `status`-drift check belongs there, because it is
the same kind of check — a generated artifact that must agree with the tree.

The gate is `make status-check`, wired into `make docs`: **regenerate `docs/status.json` into a
temp file and fail if it differs from the committed one.** This is the exact idiom
`make generate-check` (`Makefile:337`) already uses for proto codegen, so it is a pattern the repo
has and reviewers recognise, not a new mechanism. The fix is always `make status` plus commit.

Two deliberate exclusions from the drift gate, and they matter:

- **No build-stamp field exists to compare.** They are per-build values; a contributor's tree is
  `dev`/`-dirty` and a committed copy would fail the gate on every uncommitted edit. `docs/status.json`
  carries no `version`, `commit` or `built_from` — this is §2's distinction enforced by what the
  artifact is *allowed to contain*, rather than by an exclusion list the gate has to remember.

- **`released_version` is gone, and this is the P0 the first draft missed** *(F1)*. The first draft
  read it from `.release-please-manifest.json`, on the reasoning that a committed file is stable in
  any tree. It is not stable in the one tree that matters. **release-please bumps that manifest
  before the tag exists, in a normal PR against `main`** — `.github/workflows/release-please.yml`
  maintains exactly such a PR on every push — and that PR runs `make check` like any other. The
  moment the bump lands in the release branch, regenerating `docs/status.json` picks up the new
  `released_version` while the committed file still holds the old one, `status-check` fails, and
  the release PR cannot merge without a human intervening. On **every release**. A gate that blocks
  the release it exists to describe is not a gate, it is a tax, and taxes get switched off — which
  is `docs/debt.md#70` again.

  **The fix is to delete the field, not to automate around it.** The version of a release is the
  tag; the site fetches at the tag (§6); a copy of the tag inside the file is a second source of
  truth for a fact the consumer already holds — the precise failure this whole plan exists to
  remove. Deleting it also makes the hazard structurally impossible rather than handled: with the
  field gone, nothing in `docs/status.json` derives from anything a release PR touches (it changes
  `CHANGELOG.md` and the manifest; the artifact derives from the `Stage` enum, the command tree,
  `adapterFacts()`, `pricing.Version` and the ledger), so a release PR produces no status diff at
  all.

  **The rejected automation, recorded because it is the obvious alternative.** A step in
  `.github/workflows/release-please.yml`, after the `googleapis/release-please-action` step, that
  checks out the release branch, runs `make status`, and pushes an update commit onto the release
  PR when the file changes. It works, and it is strictly worse: it needs a Go toolchain and
  `python3` in a workflow that currently needs neither; the push must use `RELEASE_PLEASE_TOKEN`
  rather than the default token, because — as that file already documents at length for the tag
  push — GitHub creates no workflow runs from events triggered by `GITHUB_TOKEN`, so a
  default-token push would leave the release PR's checks stale rather than re-run; and the pushed
  commit needs a DCO trailer matching the token's identity, which is the same manual-`--signoff`
  problem `docs/debt.md#82` already tracks for this action. Three hazards, all documented in that
  one file, in exchange for keeping a redundant field. **If `released_version` is ever reinstated,
  this step is mandatory and this paragraph is its specification.**
- **Ledger counts are regenerated but compared loosely** — see the "ledger row does not parse" edge
  case. A count that changes with every ledger edit would put `docs/status.json` in the diff of
  every debt-touching PR. That is arguably correct (the count *did* change) and is accepted:
  the file is small and `make status` is one command. The alternative — omitting counts from the
  gate — creates a field nothing checks, which is a field that lies.

**The merge-conflict tax, and the procedure for it** *(F3)*. Committing a generated file means any
two concurrent PRs that touch its inputs — `docs/debt.md` (the counts), the `README.md` status
tables, `adapterFacts()`, `pricing.Version`, or the §4 declaration — both regenerate it and both
land a diff on the same lines. The second to merge conflicts. Accepted risk 2 discussed only the
*churn* of ledger counts, not what a contributor does when git stops them. The resolution is a
one-liner and belongs in `CONTRIBUTING.md` next to the `make status-check` row, not in a
reviewer's head:

> `docs/status.json` is generated. **Never hand-resolve a conflict in it.** Take either side —
> `git checkout --ours docs/status.json` — then run `make status` and commit the result.
> `make status-check` will tell you if you got it wrong.

Two supporting notes. First, the backstop is real: a botched resolution cannot survive `make check`,
because the gate regenerates and compares — which is why hand-resolving is not merely discouraged
but pointless. Second, the blast radius is bounded by what the file contains, and *(F1)*'s deletion
of `released_version` already removes one whole class of concurrent writer (every release PR).
`.gitattributes` merge drivers were considered and rejected: a custom driver is repo-global
machinery that a contributor cannot see working, for a file whose correct resolution is "run one
command".

The three consistency tests in §4 are **unit tests**, not part of the drift gate. They run under
`make test`, fail fast, and give a better message than a JSON diff.

## Acceptance criteria

Every criterion is an observable on **one** artifact now, which is itself a result of the review
*(F2)*.

1. `jq 'has("version") or has("commit") or has("built_from") or has("released_version")'
   docs/status.json` is **`false`**. The artifact carries no build stamp and no copy of the release
   version; its version anchor is the git ref it is fetched at (§5, §6) *(F1, F2)*.
2. `jq '.stages | length' docs/status.json` equals the number of `knov1.Stage` values excluding
   `STAGE_UNSPECIFIED` — **5** on `main` today (`BASELINE`, `VALUE`, `SELECT`, `VALIDATE`,
   `EXPORT`; there is no `REPORT`).
3. `jq -r '.stages[] | select(.shipped == "shipped") | .name' docs/status.json` prints exactly
   `baseline`, `value`, `select`, `export` — and **not** `validate` — on `main` at `0.1.1`.
4. `jq -r '.commands[]' docs/status.json` equals the nine commands `NewRootCmd()` registers
   (`init`, `baseline`, `mine`, `value`, `select`, `export`, `report`, `doctor`, `purge`), and
   **`status` is not among them** — the command is cut, and the artifact says so by omission
   *(F2)*.
5. `jq 'has("debt")' docs/status.json` is `true` and `.debt` carries `total`, `open` and `skipped`.
6. `jq '.debt.total' docs/status.json` equals the number of `id="…"` anchors in `docs/debt.md`
   (**84** today), and `.debt.open` equals what `scripts/ledger-check.py --json` reports open
   using its existing `DISPOSITIONS` tuple. **`grep -rn 'debt.md' --include='*.go' .` returns no
   parser** — the only reader of the ledger is `scripts/ledger-check.py`.
7. `make status-check` exits 0 on a clean tree and **non-zero** after `sed`-ing a `shipped` value in
   the declaration without regenerating. Demonstrated in the PR, per `docs/debt.md#16`'s principle
   that a gate is only real once it has been seen to fail.
8. **The release-PR regression test** *(F1)*: on a scratch branch, bump `.release-please-manifest.json`
   from `0.1.1` to `0.1.2` — exactly what release-please's PR does — and run `make check`. It is
   **green**. This is the P0's proof and it is demonstrated in the PR body, because the failure it
   guards against would otherwise surface for the first time on a release day.
9. `make status-check` runs as part of `make docs`, which runs as part of `make check`
   (`Makefile:151`).
10. A unit test fails if a new `Stage` enum value is added to `proto/kno/v1/run.proto` without a
    corresponding row in the declaration (§4 check 1). Demonstrated by adding a scratch value locally.
11. Two unit tests over `README.md`: its **Stages** table matches the declaration row-for-row on
    name and `Shipped`/`Partial`/`Planned` word, and its **Commands** table matches `NewRootCmd()`'s
    registration (§4 check 3). Both fail if either table is edited by hand into disagreement.
12. `make status` contacts no network, resolves no credential, and succeeds with `HOME` and every
    provider variable unset — the generator has the same posture `kno doctor` does, asserted the
    same way `cli/main_test.go`'s environment allowlist asserts it.
13. `docs/status.json` carries `"schema_version": 1`, and the bump protocol of §5 is written into
    `CONTRIBUTING.md` so the next person to change a key finds the rule before they change it
    *(F4)*.
14. `docs/status.json` is regenerated, not hand-edited, after a merge conflict — the procedure is in
    `CONTRIBUTING.md` and `make status-check` is the backstop *(F3)*.
15. `make check` is green, and `make bench-diff` shows no regression — `status` is off every hot
    path by construction.

## Alternatives considered

**A. Keep hand-editing it.** Edit `README.md`, `cli/root.go`'s `Long` string, `DESIGN.md`, and the
site's YAML each time a stage ships. Rejected on evidence, not principle: at **one** minor release
the four copies have already diverged — `README.md` lists `Report` as a stage where `run.proto` has
no `REPORT` member; `README.md` omits four registered commands; `DESIGN.md:398` puts `validate` in
v0.2 while `DESIGN.md:399` puts `validate --agent` in v0.3 and `README.md` says only "Planned". The
honest case *for* this option is real and should be recorded: the total edit is maybe ten minutes
per release, and this plan costs a day. The case against is that the ten minutes is not the cost —
the cost is the reader who trusts the stalest copy, and there is no mechanism that tells them which
one that is.

**B. Fully derive `shipped` — no human list at all.** Infer from the command tree, or from whether
`core` exports a stage entry point — and that signal is not even uniform today: `Baseline` is a free
function (`core/baseline.go:336`) while the other three are methods on their options types
(`ValueOptions.Value` at `core/value_loop.go:157`, `SelectOptions.Select` at `core/select.go:96`,
`ExportOptions.Export` at `core/export.go:81`) — or from test presence. Rejected: every available signal is wrong in a case that
already exists. `Stage` contains `STAGE_VALIDATE` because proto leads implementation — that is
CLAUDE.md's rule, not an oversight. `kno report` is a command and not a stage. A derived answer
would have to encode "shipped means a registered command whose name matches an enum value", which
would silently mark `validate` shipped on the day a stub command lands. **A declaration that is
cross-checked is more honest than a derivation that is subtly wrong**, and the cross-checks in §4
are where the real value is.

**C. `kno status` only, no generated file; the site shells out or scrapes.** Rejected in §2 — a
command reports the binary, and the site cannot run a release binary at build time without
downloading and executing one, which is a supply-chain surface for a roadmap widget.

**D. Generated file only, no command. — CHOSEN, on review** *(F2)*. The first draft rejected this
because "the command is nearly free once the renderer exists" and because `kno status` "answers a
real user question (`does my installed kno do validate yet?`)". Phase-1 killed both arguments. The
question is already answered by `kno --version` plus the published site, and by `kno doctor --json`
for the adapter half; and *nearly free to add* prices only the writing, not the keeping — pre-1.0
CLI surface is a post-1.0 covenant under CLAUDE.md's SemVer section, and a covenant with no named
consumer is a liability bought for a convenience nobody asked for. A repo-wide search finds no CI
check, workflow, script, plugin or doc that would call it. The field derivations do live in Go and
the generator is a Go program regardless — which is why the *renderer* survives in `cli/status.go`
and only the cobra command is dropped. Deferred with a trigger (§Accepted risks), not deleted: when
a consumer is named, the command is a printer over a struct that already exists.

**E. Put the status data in proto and serve it from `kno serve`.** Rejected for v0.1: `serve` is v0.3
(`DESIGN.md:399`), a runtime service is explicitly what the brief rules out, and a `kno.v1` message
would drag the output under protojson — where int64s become quoted strings and enums become full
names, which is precisely the shape ADR-0001 says a jq contract must not have.

## Affected packages / repos

- **`cli/`** — new `status.go`: the stage declaration table, the JSON structs, and the renderer.
  **No cobra command** *(F2)*. Reuses `writeJSON`, `adapterFacts()` and `pricing.Version`; the file
  is added to the depguard `encoding/json` exemption, which is filename-scoped and therefore a
  deliberate config edit and a review item.
- **`cli/root.go`** — no `AddCommand`. The hand-written `Long` prose about which stages run becomes
  a pointer to the README's Stages table rather than a fifth copy of the list.
- **`internal/cmd/`** — the `make status` generator, alongside the existing `internal/cmd/godoccheck`.
- **`scripts/ledger-check.py`** — additive `--json` mode. Exit-code behaviour and the release gate at
  `.github/workflows/release.yml:124` unchanged.
- **`Makefile`** — `status`, `status-check`; `status-check` added to the `docs` target.
- **`docs/status.json`** — new, committed.
- **`README.md`** — `## Status` is split into a **Stages** table and a **Commands** table (§4
  check 3), both keeping their prose column, both becoming test-checked, with a footnote naming
  `docs/status.json` and `make status` as the generated sources.
- **`.golangci.yml`** — depguard `encoding/json` allowance if the structs land in a new file.
- **`uknoAI/kno-www`** (Astro) *(F5)* — build-time fetch at a release tag, landing as an entry in
  the existing `src/content/` collections with its `schema_version` pinned in `src/content.config.ts`;
  it retires the hand-maintained copies in `src/content/docs/roadmap.md`, `src/content/home/home.yaml`
  and the `Status · v0.1.1` label in `src/pages/index.astro`. **Out of scope for this PR** — this
  plan defines the contract it consumes, and the JSON must ship first.
- **`docs/debt.md`** — the accepted-risk rows below, including the deferral of `kno status` *(F2)*.
- **Not touched:** `core/`, `stats/`, `bridge/`, `plugin/`, `adapters/`, `store/`, `api/`, `gen/`.

## Proto / schema impact

**None.** Verified against `proto/kno/v1/`: no message is added, no field is added, no enum value
changes. The plan *reads* `knov1.Stage` (`proto/kno/v1/run.proto:13-33`) and adds a test that fires
when it grows. `buf lint` and `buf breaking --against main` are unaffected; `make generate-check`
sees no diff because nothing regenerates.

The one schema-adjacent commitment: the status JSON is **not** a `kno.v1` type and must never
become one. If a future `kno serve` wants to expose status over the wire, that is a new proto
message with its own protojson encoding, and the CLI's hand-written struct stays as the jq contract
— exactly the split ADR-0001 already draws for `jsonReport`, `valueReport`, `selectReport`,
`exportReport` and `reportJSON`.

## Edge cases

| Case | What happens without care | Mitigation |
|---|---|---|
| **A dev build** (`go install`, no ldflags) | If a status artifact carried `identity()`'s output, a site rendering it would claim a version nobody can install | `docs/status.json` carries **no** build stamp and no version key at all (§5, §7). With the command cut *(F2)* there is no second artifact to confuse it with |
| **A dirty tree** | `identity()` appends `-dirty` (`cli/root.go:79-81`). A drift gate comparing a committed file against a dirty-tree regeneration would fail on every uncommitted edit | No build-stamp field exists in the artifact, so there is nothing to exclude from the comparison — the property is structural, not a rule the gate has to remember |
| **A release PR bumps `.release-please-manifest.json`** *(F1)* | If `released_version` were derived from that file, regenerating on the release branch would disagree with the committed copy and `status-check` would fail — **on every release**, blocking the release PR the gate is supposed to describe | The field is deleted (§7). Nothing in the artifact derives from anything a release PR touches, so a release PR produces no status diff. Acceptance criterion 8 is the regression test: bump the manifest on a scratch branch, `make check` stays green |
| **Two concurrent PRs both regenerate `docs/status.json`** *(F3)* — one edits `docs/debt.md`, the other edits `adapterFacts()` | The second to merge hits a conflict in a generated file, and a contributor hand-resolves it into a state that matches neither tree | Documented procedure (§7): take either side, run `make status`, commit. Never hand-resolve. `make status-check` inside `make check` makes a botched resolution unmergeable, so the wrong answer cannot survive review |
| **A ledger row does not parse** — a row missing its `id="…"` anchor, or with a `\|` inside a cell | `scripts/ledger-check.py`'s scan silently skips it (`if not m: continue`, `if len(cells) < 6: continue`). A skipped row means an undercount presented as fact | `--json` mode emits a `skipped` count alongside `total`/`open`, and `make status-check` **fails** when `skipped > 0`. The release gate's behaviour is deliberately left unchanged — it is narrow on purpose and this plan does not widen it. This is the one place the plan makes the ledger parser stricter, and it does so in the new consumer only |
| **The site builds against an older release** | The roadmap shows a stage as Planned that has since shipped, or vice versa | The site renders the **git ref it fetched at**, visibly — "as of v0.1.1" *(F1)*. That is a build input `kno-www` already holds, and unlike a field inside the file it cannot disagree with the content it labels. A status page that does not say *as of when* is the failure mode, not the staleness itself |
| **The fetch fails at site build** | Empty or stale roadmap section, silently | Build fails (§6). No fallback in CI |
| **A stage is half-shipped** — `kno validate` exists but only handles one Destination | Boolean `shipped` forces a lie in one direction or the other | `shipped` is a **tri-state**: `shipped` / `partial` / `planned`, with a required `note` when `partial`. §4's command-agreement check requires a registered command for `shipped` **or** `partial`; the README-agreement check maps `partial` to a distinct word so the table cannot flatten it back to "Shipped". This is the same discipline as `not_measured` in `valuation.proto` — a state that says *why*, rather than a number that pretends |
| **A `Stage` is added to proto in a PR that ships nothing** | Exhaustiveness test fails, blocking a legitimate proto-first PR | Correct behaviour, and cheap to satisfy: the PR adds one row declaring it `planned`. That is a *feature* — proto-first changes now announce themselves in the status document |
| **A command is registered whose name is not a `Stage`** (`doctor`, `purge`, `init`, `mine`, `report`) | A naive check would flag five real commands | Only the `shipped → command exists` direction is asserted, never the converse (§4). `commands[]` in the JSON lists all of them separately |
| **`docs/status.json` is edited by hand** | The file and the tree disagree; someone "fixes" the site by editing the artifact | `make status-check` fails in `make check`. The file gets a `"_generated"` note key naming `make status` |
| **`scripts/ledger-check.py` is absent or python3 is missing** | `make status` breaks on a machine without python3 | Already a hard dependency — `make ledger-check` (`Makefile:460`) invokes `python3` directly and the release workflow relies on it. No new dependency; the generator fails with the actionable message the CLI grammar requires |

## Test plan

- **A golden file** for `docs/status.json`, regenerated by `make update-golden` and reviewed like
  code — the repo's existing convention for CLI output. One golden, not three *(F2)*: there is no
  human renderer and no `--json` variant to pin, and no build-stamp fields to elide because the
  artifact has none *(F1)*.
- **The three consistency unit tests** of §4, each named for the scenario in vocabulary terms:
  `TestEveryStageIsDeclared`, `TestShippedStagesHaveACommand`, `TestReadmeStatusTableMatchesTheDeclaration`.
  Table-driven, `t.Parallel()`.
- **A no-version-key test** *(F1)*: decode `docs/status.json` and assert the absence of `version`,
  `commit`, `built_from` and `released_version`. Reinstating any of them is then a deliberate,
  failing-test decision that sends the author to §7's release-PR hazard rather than past it.
- **A release-PR simulation test** *(F1)*: in a temp tree, rewrite `.release-please-manifest.json`
  to a higher version and assert `status-check` still exits 0. This is acceptance criterion 8 as a
  test rather than a demo, so the P0 stays fixed.
- **jq-contract test**: decode into `map[string]any` and assert the exact key set, the way
  `decodeRaw` (`cli/jsonreport.go`) already lets `jsonReport`'s tests do. This is what makes a
  renamed key a failing test rather than a silent break for someone's pipeline.
- **`scripts/ledger-check.py --json`**: Python unit tests over small fixture ledgers — a clean row,
  a `REPAID` row, a row with no anchor (asserts `skipped` increments), a row with too few cells.
  Plus a regression test that the existing exit-code path is byte-identical for the release gate's
  inputs.
- **Gate-fails-when-it-should** (`docs/debt.md#16`'s principle, applied preemptively): a test that
  mutates the declaration in a temp tree and asserts `status-check` exits non-zero. A gate nobody
  has watched fail is not a gate.
- **No network, no credentials, no spend** anywhere in the test path. `status` constructs no
  transport, which is asserted the same way `doctor`'s posture is.
- **Coverage:** all new code is in `cli/` and `internal/cmd/`, which sit under the 70% repo-wide
  floor rather than the 85% `core`/`stats`/`bridge`/`plugin` floor. The `.coverage-baseline` ratchet
  must not decrease; goldens plus the consistency tests should clear it comfortably.

## Rollback

1. **Site first**: point the roadmap section back at its hand-edited source. Independent of this
   repo, one revert, minutes.
2. **Un-gate**: remove `status-check` from `make docs`. `make check` goes green regardless of the
   file's state; nothing else depends on it.
3. **Remove the artifact**: delete `docs/status.json` and the `make status` target. `make docs`'s
   link checker does not reference it; nothing breaks.
4. **No command to remove** *(F2)* — which is itself a rollback property worth naming: nothing here
   is on the CLI surface, so no step of this rollback is a SemVer event, needs a CHANGELOG
   deprecation notice, or can break somebody's script.
5. **`scripts/ledger-check.py`**: the `--json` mode is additive and the release gate never calls it.
   It can stay dead rather than be reverted, which keeps the release path untouched by the rollback.

Rollback order matters: un-gate before deleting, or `make check` fails on a missing generator.

## Docs impact

- **`README.md`** — `## Status` becomes two tables, **Stages** and **Commands** (§4 check 3), both
  keeping their prose column, both test-pinned, with a line naming `docs/status.json` and
  `make status` as the generated sources. Splitting them is what fixes `Report`-listed-as-a-stage
  and the four missing commands in one edit.
- **`cli/root.go`'s `Long`** — "Today baseline, value, select, and export run; validate arrives
  next" is a fifth hand-maintained copy of the list. Replaced with a pointer to the README's Stages
  table (not to a `kno status` command, which is cut — *(F2)*).
- **CLI help** — **unchanged**. No command is added, so there is no new help text to snapshot
  *(F2)*.
- **godoc** — every exported symbol; `make docs` runs `godoccheck`.
- **`CONTRIBUTING.md`** — three additions: the quality-gates table gains a `make status-check` row
  in the same style as the existing entries; the **merge-conflict procedure** for `docs/status.json`
  sits next to it (§7, *(F3)*); and the **`schema_version` bump protocol** of §5 is written down
  where the next person to rename a key will find it before they rename it *(F4)*.
- **`docs/mental-model.md` / `docs/what-the-numbers-mean.md`** — unchanged. `status` reports no
  measurement and makes no claim about a number's meaning.
- **OpenAPI** — unaffected; no proto service.
- **vhs tape** — the README quickstart does not run `status`; no re-record.
- **CHANGELOG** — an `Unreleased` entry. The PR title is `feat:` (a new published artifact and a new
  gate), so `changelog.yml` requires it — and it is required on its own merits: `docs/status.json`
  becomes a consumed contract the moment the site points at it.

## Accepted risks

Each mirrors to `docs/debt.md` with a trigger that names a date or a condition that cannot
self-satisfy.

1. **`shipped` is still a human judgement.** §4's cross-checks make the declaration hard to get
   *inconsistent*, not hard to get *wrong*: a maintainer who declares `validate` shipped the day a
   stub command lands passes every check. The mitigation is social (review) plus the tri-state
   `partial`, and that is the honest ceiling of a mechanical approach. *Trigger: revisit when the
   first stage ships `partial`, or at 1.0, whichever is first.*
2. **`docs/status.json` appears in the diff of every ledger-touching PR**, because the debt counts
   move. Accepted over the alternative (a field nothing checks). *Trigger: if a PR is ever merged
   with a stale count because the regeneration was treated as noise, drop the counts from the file
   and expose them only in a nightly report.*
3. **The site contract is designed from the Kno side and the consumer has not been built against
   it.** The consumer is no longer unverified — it is `uknoAI/kno-www`, an Astro site with
   file-backed content collections, a `website` workflow, Cloudflare Pages previews and Playwright
   e2e including a broken-link crawl *(F5)* — but "Astro can read this JSON" is not the same as
   "this JSON is the shape the roadmap page wants". The three copies it replaces
   (`roadmap.md`, `home.yaml`, the `Status · v0.1.1` label in `index.astro`) carry prose and
   ordering this artifact does not. *Trigger: before the `kno-www` PR is written — build the
   roadmap section against the real file and amend this plan if the shape does not fit, rather
   than bending the JSON in review.*
4. **`scripts/ledger-check.py` gains a second caller with different strictness.** The release gate
   tolerates unparseable rows; `make status-check` fails on them. One script, two contracts, and the
   docstring's argument for narrowness now has an exception living next to it. Accepted because the
   alternative is a second parser, which is strictly worse. *Trigger: when a third consumer of
   `docs/debt.md` appears, the ledger stops being a Markdown table parsed by regex and becomes a
   data file with a rendered view — that is the real fix and this defers it deliberately.*
5. **`kno status` is deferred, and the deferral may be wrong** *(F2)*. Cutting the command is the
   right call on today's evidence — no consumer exists in the repo — but the evidence is an absence,
   and absences are weak. If users do start asking "does my install do `validate`", they will ask by
   filing an issue, not by appearing in a `grep`. Accepted because the asymmetry is decisive: adding
   a command later is cheap and additive, while removing one post-1.0 is a major bump under
   CLAUDE.md's SemVer covenants. *Trigger: when a consumer is named — a CI check, a Ring-2 plugin,
   or a user request in an issue — `kno status` ships as a printer over `cli/status.go`'s existing
   struct and renderer. Reviewed at each minor release alongside the ledger.*
6. **The §2 distinction will be re-litigated.** Somebody will propose making the site call a binary,
   or making the file carry a build stamp, and it will look like a simplification right up until a
   dev build's `version: "dev"` reaches the website. §2's blockquote survives the command's removal
   precisely so it can be quoted in that review. *Trigger: none needed; recorded so the argument does
   not have to be re-derived.*
7. **`schema_version` depends on a human filing an issue in another repo** *(F4)*. §5 makes the
   cross-repo notification a deliverable of the bumping PR, but nothing mechanical enforces it —
   the Kno repo cannot fail its own CI on the state of `kno-www`. The mitigation is that the failure
   is loud on the other side (the site build refuses an unrecognised `schema_version`) and never an
   outage (the site keeps rendering the last tag it understands). *Trigger: the first time a
   `schema_version` bump reaches `main` without the `kno-www` issue, the protocol moves from a PR
   convention to a check — a CI step asserting that a changed `schema_version` is accompanied by a
   linked cross-repo issue.*
8. **The merge-conflict procedure is a documented convention, not a mechanism** *(F3)*. A
   contributor can still hand-resolve `docs/status.json` into nonsense; `make status-check` catches
   it, but only after they have wasted the effort. *Trigger: if conflicts in this file become
   routine — more than once in a release cycle — drop the debt counts from the artifact (Accepted
   risk 2's trigger already contemplates this), which removes the highest-churn input.*
6. **The drift gate adds a step to `make check`, which CLAUDE.md wants fail-fast-cheapest-first.**
   It is a Go program plus a `python3` invocation, so it is cheap — but it is in `make docs`, which
   is late in the chain, so a status drift is discovered after lint and test have run.
   *Trigger: if `make docs` wall-clock grows past a point where contributors start skipping
   `make check`, re-order — the same concern `docs/debt.md#72` records for the release gates.*

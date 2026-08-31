# `uknoAI/kno-examples`: committed scenarios, and a gate that makes the cookbook true

A sibling repository holding end-to-end-verified **scenarios** — committed Cases, a committed
Pool, committed expected outputs — plus the cookbook, migrated. The website quickstart gets a
copy-paste target; a future `kno demo` gets a fixture source; and, for the first time, something
executes the commands the docs tell people to run.

**Phase 0. Not implemented. No repository created.** Phase-1 adversarial review is next and this
plan is not approved until its objections are folded or explicitly accepted here and mirrored to
`docs/debt.md`.

**Phase-1 re-reviewed 2026-08-30 — verdict: amend; amendments applied.** Four findings survived
and are folded in and tagged inline. The dominant one: **the website consumer is not
hypothetical.** `uknoAI/kno-www` is live today — an Astro site with content collections under
`src/content/`, a `website` GitHub Actions workflow, Cloudflare Pages previews, and its own
Playwright end-to-end broken-link crawl — so the first draft's hedge that the website "does not
exist yet" was simply wrong, `kno-www` was missing from the affected-repos list entirely, and a
third repository's CI was about to be reddened by a deletion nobody had coordinated with it
*(F1)*. The other three: Tier B's honesty becomes a **rendering** rule rather than a wording one
*(F2)*; the 24 deleted paths get **tombstone stubs** rather than 404s, chosen over ledgering the
breakage *(F3)*; and the four recipes promoted to `executed` must say on the page that they are
executed as stages of a shared-store scenario, not standalone *(F4)*.

## Problem

Four facts, each verified against `main` at 0.1.1:

1. **There is no committed scenario data anywhere in the repo.** Every `*.jsonl` outside a
   `testdata/` directory is an adapter fixture (`adapters/evals/mine/testdata/`). No
   `cases.jsonl`, no `pool.jsonl`, no `assets.jsonl`. Every cookbook recipe either has the
   reader `curl` a vendor API, `git clone` a live third-party repo (`stripe.md` clones
   `stripe/docs`), or hand-type a JSONL example from prose.

2. **The same scenario is maintained by hand in three places and has already drifted.**
   `README.md` and `tapes/quickstart.tape` both carry a support/refund `cases.jsonl`. They
   disagree:

   | Source | `refund-01` expected |
   |---|---|
   | `README.md` | `Refunds are processed within 5 business days.` |
   | `tapes/quickstart.tape` | `Refunds are issued within 5 business days.` |

   Neither is wrong; nothing can tell. `docs/cookbook/first-baseline.md` carries a third,
   different inline set. This is the failure mode the whole plan exists to close.

3. **`make docs` does not check that any documented command runs.** Read the target: it is
   `go run ./internal/cmd/godoccheck`, a `pending` marker for OpenAPI generation, and a shell
   loop that resolves *internal relative links* in `*.md` (it `continue`s on any `http://` or
   `https://` target). That is godoc coverage plus link integrity. It is a real gate and it
   catches real rot — but `CLAUDE.md`'s claim that "a PR that changes behavior without a docs
   diff fails this gate" is enforced by a **label check**, not by execution. **No CI job
   anywhere in this repository has ever run a cookbook command.** Any plan premised on "we are
   giving up an executable in-repo gate" is premised on something that does not exist. What is
   actually being given up is the relative-link checker; what is being gained never existed.

4. **Most of the cookbook cannot be executed by anyone, anywhere.** Of 25 entries, **7** run
   with no network and no credentials as literally written — `first-baseline`, `value-a-pool`,
   `ci-gate`, `select-a-portfolio`, `export-a-tuning-set`, `read-the-whole-story` (the `--json`
   form; `--watch` wants a TTY), `retention` — because they use no `--agent` (so the default
   `fake:` applies) or call no model at all. `your-own-provider` is partial (its local-server
   example needs a loopback server). The other **17** require a vendor credential, an LLM
   credential, or both: `ZENDESK_API_KEY`, `HUBSPOT_TOKEN`, `SF_TOKEN`, `CONFLUENCE_API_TOKEN`,
   `JIRA_API_TOKEN`, `NOTION_TOKEN`, `SHOPIFY_TOKEN`, `LANGSMITH_API_KEY`,
   `LANGFUSE_PUBLIC_KEY`/`LANGFUSE_SECRET_KEY`, `BRAINTRUST_API_KEY`, `HF_TOKEN`,
   `ANTHROPIC_API_KEY`, AWS SigV4 credentials, GCP application-default credentials, and an
   implied `OPENAI_API_KEY` that **the vendor recipes never name** — it is defined once, in
   `your-own-provider.md`'s scheme table, and thereafter merely implied by `--agent
   openai:gpt-4.1`. A reader following `zendesk.md` from the top learns about Zendesk's token
   and is never told the recipe also bills OpenAI.

So: the cookbook is 25 pages of instructions, four of the five stages deep, none of it executed,
some of it already contradicting itself, and one of its most common prerequisites undocumented.

### The conflict this plan must flag

`CLAUDE.md` says: *"Read `DESIGN.md` first — it is the source of truth... Where they conflict,
stop and flag it; do not silently pick one."*

`DESIGN.md`'s Repository layout places examples **inside** this repo:

```
└── examples/           # toy agent + pool + calibration set; gum-polished scripts; vhs tapes
```

and line 242 says *"`examples/` ships a small human-labeled calibration set"* for judge
calibration. **This plan contradicts `DESIGN.md`.** It is flagged, not silently overridden. The
resolution proposed below is a partial split, not a wholesale move, and it lands as a `DESIGN.md`
diff in the same PR — see *Design, §1*. If the reviewer prefers `DESIGN.md` as written, the
correct outcome is to reject this plan, not to amend `DESIGN.md` quietly.

## Design

### 1. What moves, what stays, and the `DESIGN.md` resolution

**Moves to `uknoAI/kno-examples`:**

- All 25 files in `docs/cookbook/` except `README.md`, becoming `recipes/<name>.md`.
- New: `scenarios/` — the committed data that has never existed anywhere.
- New: the runner (`cmd/verify`) and its CI.

**Stays in `uknoAI/kno`, and this is deliberate:**

- **The README quickstart, in full, self-contained.** The front door must not depend on a second
  repository being reachable, renamed, or unforked. Its four steps keep their inline JSONL. It
  gains a copy-check (§6), not a link.
- `docs/cookbook/README.md`, rewritten as a **pointer page**: the same two tables (Core /
  Vendor), the same one-line descriptions, `https://` links out. Keeping the index in-repo means
  a reader who cloned the repo still gets the map; and because the `make docs` link checker
  `continue`s on `https://`, the page cannot break the build — which is exactly the property
  that makes it also *unverified*. That asymmetry is real debt and is ledgered, not hidden.
- `docs/mental-model.md`, `docs/what-the-numbers-mean.md`, `docs/evaluation-design.md`,
  `docs/plans/`, `docs/adr/`, `docs/debt.md`, `DESIGN.md`, `CONTRIBUTING.md`, `tapes/`. These
  are claims about the engine's epistemics and its process. They belong with the code that has
  to honor them.
- **The judge calibration set stays in `uknoAI/kno`** when it lands. `DESIGN.md` puts it in
  `examples/`; this plan moves recipes, not test data that a CI gate consumes. `CLAUDE.md`
  requires that "a judge prompt change that drops agreement below threshold fails CI" — that
  gate cannot live across a repo boundary without making `uknoAI/kno`'s CI depend on a network
  fetch. The calibration set goes to `judge/testdata/` ***(verify: `judge/` exists but the
  calibration harness does not yet; this is a constraint on a future plan, not a change here.)***
- **A one-line tombstone stub at each of the 24 deleted `docs/cookbook/<name>.md` paths**
  *(F3)*. Inbound links are the surface the first draft missed; see §8.

**The `DESIGN.md` diff** (same PR as the migration): the `examples/` row is replaced with a note
that scenarios and recipes live in `uknoAI/kno-examples`, and line 242's calibration-set sentence
is re-pointed at `judge/`. The `gum` row in the tooling table (line 261, "used in `examples/`
shell scripts") is re-pointed too, or `gum` is dropped — `scenarios/*/run.sh` must be plain POSIX
`sh` so CI and a reader's machine run the identical bytes, and `gum` is a dependency a
copy-paste target should not have. **Decision: drop `gum` from the scenario scripts.** Polish is
worth less than "the script in the docs is the script CI ran".

### 2. The enforcement mechanism (this is the plan)

An external repo has no `make check`. So it builds one, and the design principle is: **every
recipe carries a machine-set verification tier, and no recipe may omit it.**

Front matter, on every `recipes/*.md`:

```yaml
---
verification: executed | flags-only | manual
scenario: support-refunds        # required when verification: executed
last-verified: 2026-08-30        # WRITTEN BY CI, never by a human
verified-against: kno v0.1.1     # WRITTEN BY CI
owner: "@handle"                 # required when verification != executed
---
```

A lint refuses a recipe with missing or hand-edited machine fields (the generator recomputes and
diffs — same shape as `make generate-check` in `uknoAI/kno`).

**Tier A — `executed`.** Commands run end-to-end against the released binary with `fake:` and
committed data. Assertions are on `--json` documents, never on rendered text (§5). The rendered
page shows: *"Verified end-to-end against kno v0.1.1 on 2026-08-30."*
Eligible today: the 7 keyless recipes, plus every recipe that only *seemed* unrunnable because it
needs "a prior recorded run" — `select-a-portfolio`, `export-a-tuning-set`,
`read-the-whole-story`, `retention` all read a SQLite store that an earlier stage wrote. **The
scenario, not the recipe, is the unit of execution**, so `run.sh` performs baseline → value →
select → export → report → purge in one store and each recipe asserts against its own stage.
That single decision moves four recipes from "cannot be checked" to "checked".

**But `executed` must not be read as `standalone`** *(F4)*. Those four are executed only as part
of one shared-store `run.sh`: `select-a-portfolio` reads a SQLite store that `baseline` and
`value` wrote earlier in the same script. A reader who copy-pastes just the excerpt shown on the
recipe page, without the scenario, gets an empty store and a confusing failure — which is exactly
the false confidence this plan exists to destroy, re-created underneath a green tick. So the
rendered page for any `executed` recipe whose scenario has prior stages carries, adjacent to the
verification line and not in a footnote: *"Verified as stage 3 of the `support-refunds` scenario.
These commands read a store the earlier stages wrote — run `scenarios/support-refunds/run.sh`
first, or they will find nothing."* The front matter gains `requires-stages: [baseline, value]`
so the sentence is generated from a declared field rather than remembered, and the lint fails any
`executed` recipe that quotes a `run.sh` marker below the scenario's first stage without it.

**Tier B — `flags-only`.** Recipes needing credentials. CI extracts every `kno` invocation and
validates it against the *released binary's own flag surface*: the subcommand must exist, every
long flag must appear in `kno <subcommand> --help`, and every `--agent`/`--evals`/`--pool`
scheme prefix must appear in `kno doctor --json` (verified: `doctor` has `--json` and "contacts
nothing"). This catches the overwhelmingly most common rot — a renamed or removed flag — with no
key and no network. It proves nothing about the vendor call. The page says exactly that:
*"Command shapes checked against kno v0.1.1. The vendor steps are not machine-verified; last
checked by hand on <date>."*

**Tier B's honesty is a rendering rule, not a wording one** *(F2)*. Wording is not a control: a
skimming reader sees a badge shape and a colour and has finished forming a belief before reading
the sentence underneath it. So the renderer carries the load, not the prose. **Tier A is the only
tier in the system with a positive affordance** — the only tick, the only green, the only word
"Verified". **Tier B renders in the same neutral register as Tier C**: same icon, same colour
token, same weight, differing only in its text. `flags-only` therefore *looks* exactly as
unverified as `manual`, because with respect to the question a reader is actually asking — does
this recipe work — it is. The difference between them is a claim about what was checked, which is
a sentence, not a colour. Asserted by a renderer test (AC 18), not by review.

**Tier C — `manual`.** Prose about a vendor's UI or console ("Admin → Apps and integrations →
API"). Not machine-checkable by anything, ever. Page says: *"Not machine-verified. Vendor UIs
change; last checked by hand on <date> by @owner."* No badge, no green tick, no implication.

The honest label for the second class is therefore **not** "unverified" as a single word — it is
two different words, because "the flags are right and the vendor steps are not checked" and "none
of this is checked" are different claims, and collapsing them would be exactly the kind of
statistical-honesty violation `CLAUDE.md` forbids for measurements. The same discipline, applied
to documentation.

**Staleness is automatic.** The renderer injects a visible banner when `last-verified` (Tier A,
CI-written) is older than 30 days, or when `last-manual-verification` (Tier B/C, human-written)
is older than 180 days. Nobody has to remember; the page tells on itself. This is `docs/debt.md`'s
lapsing-trigger philosophy applied to documentation: a claim with no expiry is a claim nobody
revisits.

**Blocks are opt-in, not opt-out.** Only fenced blocks tagged ` ```bash kno-run ` are executed.
A block showing `rm -rf`, a vendor `curl`, or illustrative output is inert by default. An
untagged block containing a bare `kno ` invocation is still *parsed* for Tier B flag checking —
extraction for checking and extraction for execution are separate passes, because the failure
modes are opposite (missing a check is silent rot; running something unintended is destructive).

### 3. Nightly CI against the latest released binary

**Install.** Via the project's own `install.sh`, so CI exercises the path users exercise:

```
https://github.com/uknoAI/kno/releases/download/<tag>/kno_<tag-without-v>_<os>_<arch>.tar.gz
```

(verified from `install.sh` and `.goreleaser.yaml`: `name_template: "kno_{{ .Version }}_{{ .Os }}_{{ .Arch }}"`,
`bare=${version#v}`, plus `checksums.txt` from the same base URL). `install.sh` verifies the
cosign bundle **only when `cosign` is present**, so the workflow installs
`sigstore/cosign-installer` (SHA-pinned, the same action and pin `release.yml` uses) *before*
running it. A CI job that silently skips signature verification while appearing to test the
signed install path would be worse than not testing it.

**Two jobs, two different meanings:**

| Job | Binary | On failure |
|---|---|---|
| `released` | latest release tag via `install.sh` | The **docs are wrong today**. Red job + issue on `kno-examples`, P0. |
| `main` | built from `uknoAI/kno@main` source | A **merged change will break a recipe at the next release**. Red job + issue on `uknoAI/kno` labelled `docs-drift`. Advisory, not release-blocking. |

The `main` job is why this repo is worth having: it turns "the docs rotted three weeks ago" into
"the PR that will rot the docs merged yesterday", without putting scenario runtime on the
critical path of every `uknoAI/kno` PR.

**Failing loudly** reuses `pricing-check.yml`'s issue lifecycle verbatim, because that mechanism
is already proven in this org and inventing a second one would be debt: normalize each finding
to a signature, intersect against open issues labelled `docs-drift`, refresh the body of any
issue whose signature set intersects, **close** any whose set is now disjoint with a dated
comment, and create **at most one** new issue per run titled after the specific finding — never
a generic summary. Close-on-green falls out for free (an empty finding set is disjoint from
everything). Cross-repo issue creation needs a fine-grained PAT scoped to `issues: write` on
`uknoAI/kno` only, held in `kno-examples`, mirroring the posture of `HOMEBREW_TAP_TOKEN`.

**Commands that need real API keys are not run nightly. Ever.** They are Tier B (flag shapes,
free). What actually verifies them is a separate, deliberately inconvenient job:

- `vendor-smoke`, `workflow_dispatch` only, gated on a GitHub **environment** with required
  reviewers (the pattern `release.yml` already establishes with `environment: release`).
- Budget-capped the way `nightly.yml`'s `live` job is: `KNO_MAX_COST_USD` set in the job env,
  plus `--max-cost-usd` **and** `--max-calls` on each invocation (both flags verified present on
  `baseline` and `value`). Two independent ceilings, because a wrong price in the table makes the
  dollar cap wrong while the call cap stays right.
- On success it writes `last-manual-verification: <date>` and the tested kno version into the
  recipe's front matter and opens a PR with that one-line diff. The date is therefore evidence
  of an actual run, not a memory.
- Quarterly cadence by convention, enforced only by the 180-day staleness banner. **Nothing
  automatically spends money.** Prime directive 4 applies to our own CI.

Vendor recipes are, in the end, verified by a human with credentials, on a schedule, with the
result date-stamped by machine. That is the honest ceiling. Claiming otherwise would be the
documentation equivalent of reporting a delta without its interval.

### 4. Scenario structure and naming

Vocabulary is law, so the directory names are the vocabulary — `evals/`, `pool/`, `expected/`.
No `data/`, no `input/`, no `fixtures/`.

```
scenarios/support-refunds/
  README.md            the story: what this agent does, what the Assets are, what to look for
  DATA-PROVENANCE.md   who wrote this data and the assertion that it is synthetic
  evals/cases.jsonl    the Cases (9, matching tapes/quickstart.tape's set)
  pool/pool.jsonl      the Pool of candidate Assets (3)
  run.sh               POSIX sh; the single source every recipe quotes
  expected/            one projected JSON document per stage (§5)
```

`support-refunds` is the first and, at launch, the only scenario. It is the Zendesk-shaped one:
refunds, shipping, account, billing — the same four tags the tape already uses. The vendor
recipes then read as "your Zendesk data, arranged the way `support-refunds` is arranged", which
is what `docs/cookbook/README.md` already promises ("Every vendor recipe is the same shape").

**Recipes never re-type commands.** A recipe includes a line range of `run.sh` by marker, and a
lint asserts the included text is byte-identical to the source. This is the mechanism that stops
`processed`/`issued` from happening again — not a review convention, a failing build.

Scenario slugs are `<domain>-<task>`, lowercase, hyphenated. A scenario is added, never renamed
(a renamed scenario breaks every external link to it); superseding one means adding the successor
and marking the predecessor `deprecated: true`, which the renderer surfaces.

### 5. Keeping expected outputs stable when kno's output changes

Three rules, and the first two are the load-bearing ones.

**Assert on `--json`, and only on a committed allowlist of fields.** `--json` exists on
`baseline`, `value`, `select`, `export`, `report`, and `doctor` (verified). A full-document
golden would churn on every run — run ids, timestamps, durations, versions — so it would be
regenerated reflexively and rubber-stamped, which is a golden file that has stopped being a test.
Instead `expected/<stage>.json` holds a **projection**: the fields the recipe's prose actually
makes a claim about. Additive proto/CLI changes then pass (correct — the recipe's claim is
untouched); a removed or renamed field fails (correct — the recipe's claim just became false).

**Determinism is bought with flags that exist, not with luck.** `--run-id`, `--seed`,
`--split-seed`, `--holdout-frac`, `--goal`, and `--concurrency` are all real flags (verified in
`cli/baseline.go` / `cli/value.go`). `run.sh` pins every one of them and uses a fresh `KNO_DB`
under a temp dir. `fake:` is deterministic by design. Anything still varying between two runs of
the same scenario on the same binary is a **kno bug** and the scenario surfaces it — a
flakiness detector we get for free.

**Rendered text is asserted by quotation, not by byte.** Where a recipe's prose quotes CLI output
— `first-baseline.md` quotes `6 held back`, and the README quotes the same — the assertion is
that the exact quoted phrase appears in the actual output. If the CLI reformats, the assertion
fails, and the thing that must change is the prose. That is the correct direction of blame.

Regeneration is `make update-expected` (mirroring `make update-golden`'s `go test ./... -update`
convention), run by a human, diff reviewed like code. Expectations record the kno version that
produced them. **Nothing pins a floating "latest" inside a committed expectation.**

Consequence, stated up front: a kno release that changes a projected field turns `kno-examples`
red on release day. That is the design working. It also means `kno-examples` **must not be a
required check on `uknoAI/kno` PRs** — a cross-repo gate that reddens on someone else's schedule
becomes a gate people learn to override.

### 6. Relationship to `kno demo` — duplicate, deliberately, with a detector

**`kno demo` does not exist** ***(verify: no `demo` subcommand in `cli/`, no mention in
`DESIGN.md`, `README.md`, or `CONTRIBUTING.md`; the closest artifact is `tapes/quickstart.tape`)***.
This section is therefore a constraint on a future plan.

**Decision: the fixtures are duplicated — `go:embed`ed in `uknoAI/kno`, canonical in
`kno-examples` — and a nightly job byte-compares them.** Defended:

- A `kno demo` that fetches a sibling repo at runtime fails on a plane, on an air-gapped box, and
  behind a corporate proxy — and it makes a shipped binary depend on a repo name we could rename.
  `install.sh` already goes to great lengths to make the binary self-sufficient and verifiable;
  a network-dependent demo would undo that.
- Sharing via a Go module dependency (`uknoAI/kno` importing `kno-examples`) inverts the
  dependency: the engine would depend on the docs. `CLAUDE.md`'s layering rule is about `core/`,
  but the spirit is the same, and `go.mod` churn on `uknoAI/kno` for a docs change is a bad
  trade.
- So: duplication is accepted, and the cost of duplication — divergence — is paid by a detector
  rather than by vigilance. The nightly job fetches the embedded fixture (via `kno demo
  --print-fixtures`, or by reading the file from `uknoAI/kno@main`), byte-compares against
  `scenarios/support-refunds/evals/cases.jsonl`, and files a `docs-drift` issue on divergence.
- **The same detector covers `README.md` and `tapes/quickstart.tape` on day one**, before
  `kno demo` exists — which is how the `processed`/`issued` drift found above gets caught and
  stays caught. This is the highest-value single job in the plan and it ships first.

### 7. Licensing, data provenance, and contribution flow

- **Apache-2.0**, matching `uknoAI/kno` (verified: `Copyright 2026 uknoAI`). Scenario data is
  under the same license, which requires that we have the right to license it.
- **All scenario data must be synthetic.** Hard rule, in `CONTRIBUTING.md` and enforced by a
  required `DATA-PROVENANCE.md` per scenario naming the author and asserting synthesis. Real
  support tickets are end-user conversation content; `CLAUDE.md` treats traces as customer data,
  and a public repo of "example support tickets" is precisely where that rule gets broken by
  accident.
- **`gitleaks` runs over the working tree and history**, same as `uknoAI/kno`'s `secrets-scan`.
  A scenario repo full of `export ZENDESK_API_KEY=` lines is the single most likely place in the
  organization for a real key to be pasted.
- **DCO sign-off** (`git commit -s`), same `dco.yml`, no CLA — matching `CONTRIBUTING.md`'s
  stated position. `CODE_OF_CONDUCT.md` and `SECURITY.md` by reference to `uknoAI/kno`.
- **Contribution flow:** a new recipe declares its tier. Tier A must ship committed expectations
  and pass the runner in PR CI (cheap: `fake:` only, ubuntu-latest, no keys, no self-hosted
  runner). Tier B/C must name an owner. A PR that adds a recipe with no tier fails the lint.
  Vendor recipes are the designed on-ramp for external contributors, exactly as `CONTRIBUTING.md`
  designates Ring-1 adapters and judge prompts.

### 8. `uknoAI/kno-www` is a live consumer, and the old paths get tombstones *(F1, F3)*

**The website exists.** `uknoAI/kno-www` is an Astro site with content collections under
`src/content/`, a `website` GitHub Actions workflow, Cloudflare Pages preview deployments, and a
Playwright end-to-end crawl that asserts links resolve. It is a third repository this migration
can turn red, and the first draft did not list it at all. Two consequences, the first of which is
an open question that must be **answered, not guessed, before Phase 2 begins**:

**Open question — ANSWERED 2026-08-31, by reading the repository.** `kno-www`'s build sources
content ONLY from its own tree: every collection in `src/content.config.ts` is an Astro
`glob({ base: './src/content/...' })` over `site`, `home`, `blog`, `use-cases`, `pages`, and
`docs`. Nothing reads `uknoAI/kno`'s `docs/cookbook/*` — no submodule, no build-time fetch, no
sync script, no transclusion. **Deleting those files cannot break the site build.** The
migration is a docs PR, not a coordinated release.

**And the Playwright crawl does not catch it either — which is worse, not better.**
`tests/e2e/links.spec.ts:31` skips every href beginning with `http`, so external links are never
asserted. The site holds **22 hard links** to `github.com/uknoAI/kno/blob/main/docs/cookbook/*.md`
across nine entries (`anthropic`, `ci-gate`, `export-a-tuning-set`, `first-baseline`,
`read-the-whole-story`, `retention`, `select-a-portfolio`, `your-own-provider`, `zendesk`), all
branch-pinned to `main`. Every one 404s the moment `main` moves past the deletion, and **no gate
in either repository would report it** — not `make docs`, which checks only relative links, and
not the crawl, which skips external ones. Silent rot, discovered by a reader.

This raises rather than lowers the value of the tombstone decision in *(F3)*: the stubs are what
keep all 22 links alive, and they are load-bearing precisely because nothing else is watching.
The coordinating `kno-www` PR remains worth doing — a link to a stub is worse prose than a link
to the real page — but it is now a quality step, not a build-breakage mitigation, and it may
land after the migration rather than before it.

**Sequencing decision, relaxed by the answer above.** The original ordering assumed a possible
build break; there is none, and the tombstones keep every link working, so the `kno-www` PR may
follow the migration. What must NOT happen is the migration landing without the stubs — that is
the step nothing else would catch. In order: (1) answered; (2) open
the `kno-www` PR that re-points every cookbook reference at `kno-examples` — and, if the content
collection sources prose, re-points the source; (3) merge the `uknoAI/kno` migration only once
(2) is green on a Cloudflare Pages preview. If the answer is "the collection sources prose", the
two merge in one window and `kno-examples` must already be public — which makes the new repo a
**prerequisite** of the migration rather than a consequence of it.

**Tombstones, not 404s** *(F3)*. Deleting the 24 files also removes them from `make docs`' link
check, and the first draft covered only *outbound* links. The *inbound* ones are the larger
surface: `blob/main/docs/cookbook/stripe.md`-style links live in merged PR bodies, in issues, in
`kno-www`, and in anything anyone has bookmarked. Those are branch-pinned and 404 the instant
`main` moves past the deletion.

**Decision: leave a one-line stub at each of the 24 old paths**, chosen over ledgering the 404s
as accepted debt. Three reasons. (i) The debt version's repayment trigger would be "when someone
reports a dead link" — a trigger that fires only *after* the damage and cannot lapse observably,
which is precisely the kind of entry `docs/debt.md`'s rules reject. (ii) A stub costs 24 lines
once; the 404 costs every reader of every old link, forever. (iii) It de-risks the `kno-www`
crawl above *independently of when that coordinating PR lands* — belt and braces on the one
failure mode this plan can cheaply make impossible.

The stub is exactly one line and carries no prose:

```markdown
Moved to <https://github.com/uknoAI/kno-examples/blob/main/recipes/stripe.md>.
```

A lint in `uknoAI/kno` asserts every stub is a single line whose only content is one
`https://github.com/uknoAI/kno-examples/blob/main/recipes/<name>.md` link, and that the set of
stub paths equals the set of pre-migration recipe paths. A stub therefore cannot quietly grow
back into a second copy of a recipe — which would reintroduce the exact drift this plan exists to
kill — and a missing stub is a 404 that fails the build. Stubs are never removed; they are the
permanent redirect layer in a system that has no redirect layer.

Residual, stated rather than glossed: with stubs in place, `make docs`' link checker no longer
fails on the deletion, so the first draft's "the link checker failing is itself the acceptance
evidence" disappears. It is replaced by the stub lint (AC 1) and the `git grep` criterion (AC 2),
which assert a property rather than observing a transient failure — a strictly better gate.

## Acceptance criteria

Numbered, each naming something observable.

1. `docs/cookbook/` in `uknoAI/kno` contains `README.md` plus exactly 24 one-line tombstone
   stubs, one per migrated recipe, and `make docs` passes *(F3)*. A lint asserts each stub is a
   single line whose only content is one
   `https://github.com/uknoAI/kno-examples/blob/main/recipes/<name>.md` link, and that the set of
   stub paths equals the set of pre-migration recipe paths — a missing stub is an inbound 404 and
   fails the build. Every relative link that pointed at a recipe from live prose is rewritten to
   an `https://` link in the same PR.
2. `git grep -n 'docs/cookbook/[a-z-]*\.md'` in `uknoAI/kno` returns no hits outside
   `docs/cookbook/README.md`, the stub files themselves, `CHANGELOG.md`, and `docs/plans/` — no
   live prose anywhere routes a reader through a tombstone *(F3)*.
3. `DESIGN.md`'s repository-layout block no longer contains an `examples/` row, and line 242's
   calibration-set sentence names `judge/` rather than `examples/`. The conflict is resolved in
   writing, in the same PR, not carried.
4. `scenarios/support-refunds/run.sh` executes to exit 0 on a clean machine with only a released
   `kno` on `PATH`, no network after install, and no environment variable other than `KNO_DB`.
5. Running `run.sh` twice against the same released binary produces byte-identical projected
   `expected/*.json` for all six stages. A diff is a kno determinism bug and is reported as one.
6. Every file in `recipes/` has front matter whose `verification` value is one of exactly
   `executed`, `flags-only`, `manual`. The lint exits non-zero if any file lacks it; a test in
   the runner's own suite proves the lint fails on a fixture recipe missing the field.
7. At least the 7 keyless recipes plus `select-a-portfolio`, `export-a-tuning-set`,
   `read-the-whole-story`, and `retention` are `verification: executed` — i.e. no recipe is
   demoted to `flags-only` merely because it needs a prior recorded run.
8. Every recipe requiring a credential names **every** credential it requires, including
   `OPENAI_API_KEY` where `--agent openai:...` appears. A lint cross-references each `--agent
   <scheme>:` in a recipe against a scheme→required-env table and fails on an unnamed one. The
   17 vendor recipes pass this lint; today at least 10 would fail it.
9. The nightly `released` job installs via `install.sh` with `cosign` present, and the job log
   contains the cosign verification line. A run where verification was skipped fails the job.
10. Injecting a deliberately broken recipe (a renamed flag) into the runner's `testdata/` causes
    the runner to exit non-zero, and a CI self-test asserts exactly that. Injecting a recipe whose
    `expected/` disagrees with actual output does the same.
11. The issue lifecycle is observable: a synthetic finding creates exactly one issue labelled
    `docs-drift`; a second run with the same finding edits that issue rather than creating a
    second; a run without the finding closes it with a dated comment. Proven in a dry-run test
    against a mocked issue list, not by waiting for reality.
12. A recipe with `last-verified` back-dated 45 days renders a visible staleness banner; one at 29
    days does not. Asserted by a renderer test.
13. The fixture-drift detector fails when `README.md`'s quickstart JSONL differs from
    `scenarios/support-refunds/evals/cases.jsonl` — demonstrated by running it against today's
    `main`, where it must report the `processed`/`issued` divergence.
14. `gitleaks` passes over the working tree and full history of `kno-examples` at first commit.
15. No workflow in `kno-examples` can spend money without a GitHub environment approval;
    `grep -L 'environment:' ` over every workflow referencing a provider key returns nothing.
16. The `kno-www` open question in §8 is answered in writing — as a dated amendment to this plan
    naming the `kno-www` files that were read — **before** any file is deleted from
    `docs/cookbook/`. A migration PR opened while the answer is still unknown is rejected on
    process, not on content *(F1)*.
17. `kno-www`'s `website` workflow and Playwright crawl are green on a Cloudflare Pages preview of
    the coordinating `kno-www` PR **before** the `uknoAI/kno` migration merges, and the migration
    PR body links that preview *(F1)*.
18. A renderer test asserts that Tier B and Tier C pages emit the identical icon and the identical
    colour token and differ only in their text, and that no `flags-only` or `manual` page emits
    the tick or the green token reserved for `executed`. The test fails if a future stylesheet
    gives Tier B a colour of its own *(F2)*.
19. Every `executed` recipe with a non-empty `requires-stages` renders the prior-stage sentence
    adjacent to its verification line. A golden-file test over the four shared-store recipes
    (`select-a-portfolio`, `export-a-tuning-set`, `read-the-whole-story`, `retention`) asserts the
    sentence is present and names the scenario and the script *(F4)*.

## Alternatives considered

**A. Keep everything in `uknoAI/kno` and build the runner in-repo (`examples/` + `make
examples-check`).** This is what `DESIGN.md` already prescribes, and the case for it is genuinely
strong: one repository means a behavior change and its docs change land in one reviewed diff,
which *is* the docs-gate philosophy; the existing relative-link checker keeps working with no
change; contributors have one place, one `CONTRIBUTING.md`, one DCO check; there is no cross-repo
token, no cross-repo issue filing, no version-skew matrix, and no second CI to maintain. It is
also cheaper by a wide margin.

Rejected for two reasons that are not stylistic. **(i) Circularity.** The verification that
matters is against the binary users can actually download; a job inside the producing repo can
only test HEAD (not what anyone has) or the previous release (which cannot validate the change
under review). The `released` and `main` jobs above are two different questions and only one of
them is answerable in-repo. **(ii) Cadence.** `make check` is already a 25-minute job on two
runner OSes; scenario execution is release-keyed and daily, not commit-keyed, and taxing every
Go PR with docs-scenario runtime is how a gate becomes something people route around. The
`DESIGN.md` conflict is real and is resolved explicitly rather than by fiat (§1).

**B. A documentation *website* repo that renders both prose and examples (Docusaurus/MkDocs),
with scenarios as a subfolder.** Rejected: a site is a rendering concern, and coupling the
verification cadence to a site build makes the data harder to `git clone && sh run.sh`, which is
the entire point of a copy-paste target. It also inverts the dependency — the executable artifact
would live inside the presentation layer. *(F1: corrected. The site is not hypothetical — `uknoAI/kno-www` exists and is live. It is simply
not referenced from `README.md`, `DESIGN.md`, or `CONTRIBUTING.md`, which is how the first draft
mistook "no in-repo reference" for "no consumer". The rejection above stands and is in fact
strengthened: `kno-www` already **is** the rendering layer, so building a second one here would
duplicate a repository we own. See §8 for what it costs us instead — coordination.)*

**C. Move nothing; add a cheap `make cookbook-check` in `uknoAI/kno` that flag-shape-checks
fenced blocks against `kno --help`.** Rejected **as a complete answer**, adopted **as a
component**: this is exactly Tier B, and it is genuinely the highest value-per-line idea in the
alternatives. What it cannot do is execute anything end-to-end, which means the four recipes that
need a prior recorded run stay unverified, no committed scenario exists, the README/tape drift
stays undetected, and there is still nothing for a website to copy-paste. Taking the cheap half
and rejecting the expensive half would leave the plan's headline problem — nothing runs — exactly
where it is.

**D. Verify vendor recipes nightly with real credentials in CI.** Rejected on cost and on
principle: it violates prime directive 4's spirit (automatic, unattended spend), it requires
long-lived credentials for eight third-party SaaS products in a public repo's CI, and it would
redden nightly on every vendor's outage — training everyone to ignore the signal. The
`workflow_dispatch` + environment-approval + date-stamp design (§3) buys most of the truth at
none of that risk.

## Affected repos and packages

**New: `uknoAI/kno-examples`** — `recipes/`, `scenarios/`, `cmd/verify` (the runner),
`.github/workflows/{pr,nightly,vendor-smoke}.yml`, `METHODOLOGY`-equivalent lint config,
`CONTRIBUTING.md`, `LICENSE` (Apache-2.0), `DATA-PROVENANCE` template.

**`uknoAI/kno`** — `docs/cookbook/` (24 recipes replaced by one-line tombstone stubs *(F3)*,
`README.md` rewritten as a pointer page);
`README.md` (the five cookbook links in the Quickstart's closing paragraph and the "Full
walkthrough" line become `https://` links); `DESIGN.md` (repository-layout `examples/` row, line
242 calibration sentence, line 261 `gum` row); `CONTRIBUTING.md` ("Where to start" gains recipes
as an on-ramp); `docs/debt.md` (new entries); `CHANGELOG.md` under `Unreleased`. **No Go package
is touched.** Later, when `kno demo` lands: `cli/` and an embedded fixture — a separate plan.

**`uknoAI/kno-www`** *(F1)* — **live today**, and absent from the first draft's list. An Astro
site with content collections under `src/content/`, a `website` workflow, Cloudflare Pages
previews, and a Playwright broken-link crawl. It consumes the paths this migration moves and is
the third repository this change can redden. Its coordinating PR is sequenced ahead of — or
atomic with — the migration per §8. **Exactly which files that PR touches cannot be stated until
§8's open question is answered**, and this plan does not guess: if the content collection sources
cookbook prose, the PR changes the build; if it only links, the PR changes links and the crawl's
expectations.

## Proto / schema impact

**None.** `kno-examples` generates nothing from proto, imports no Go package from `uknoAI/kno`,
and adds no message or field. It consumes the CLI's `--json` documents and its exit codes as an
external observer.

Worth stating precisely, because it is not quite "no impact": this makes the `--json` document
shape and the exit codes into **observed** surfaces. `CLAUDE.md` already declares both covenants
post-1.0 ("proto, plugin protocol, exit codes, `kno.yaml` schema, and the public Go API are all
covenants"). `kno-examples` does not change that promise; it becomes the first thing that would
notice it being broken. That is a benefit, and it is also the reason `kno-examples` must not gate
`uknoAI/kno` merges — noticing and blocking are different jobs.

## Edge cases

| Case | Behavior |
|---|---|
| CI has no API keys | Tier A runs (`fake:`, offline). Tier B runs flag-shape checks against `--help` and `doctor --json`. Tier C is not executed and is not claimed to be. No job silently degrades to "passed". |
| Released binary drifts from the docs | The `released` job reddens and files a `docs-drift` issue naming the specific finding. Since the release already shipped, the fix is a `kno-examples` PR (docs are wrong), not a kno revert. |
| A merged kno change will break a recipe | The `main` job reddens first and files the issue on `uknoAI/kno`. Advisory: it never blocks a release. Discovered before users, not after. |
| A scenario's output legitimately changes | The projection diff is reviewed like code, `make update-expected` regenerates, the recipe's prose changes in the same PR. If the prose quoted a phrase that moved, the quotation assertion fails and forces that edit. |
| An output change is additive (new `--json` field) | Passes. The allowlist projection ignores fields no recipe claims. This is the whole reason for a projection rather than a full golden. |
| `install.sh` cannot resolve the latest release (API rate limit, outage) | The job fails with a distinct exit reason and files **no** issue — an infrastructure failure is not a docs finding. Mirrors `pricing-check.yml`'s refusal to file when the report is unparseable. |
| Contributor PR from a fork | PR CI runs Tier A + lints on `ubuntu-latest` with zero secrets. Issue-filing and `vendor-smoke` are unreachable from a fork PR by construction (no `pull_request_target`, no secrets in the PR workflow). |
| Contributor adds a recipe with no tier / no owner | Lint fails. Not a review convention. |
| Contributor commits a real API key in scenario data | `gitleaks` fails the PR; the key is treated as compromised regardless, per `SECURITY.md`. |
| Contributor commits real customer support tickets | `DATA-PROVENANCE.md` is required and reviewed; a scenario without a synthesis assertion cannot merge. This is a human gate and is stated as one. |
| Stale vendor recipe (nobody has run it in a year) | The 180-day banner renders on the page itself. It degrades honestly rather than lying quietly. |
| A vendor recipe's product is discontinued | The recipe is marked `deprecated: true`; the renderer says so; the file is not deleted (external links). |
| `vendor-smoke` overspends | Three independent limits: `--max-cost-usd`, `--max-calls`, and `KNO_MAX_COST_USD` in the job env. The environment approval means a human authorized the run in the first place. |
| Scenario grows large enough to be slow | Scenario runtime is budgeted; a scenario exceeding it is split, not silently allowed to make the nightly a 40-minute job nobody watches. |
| `kno-examples` is renamed or archived | `uknoAI/kno`'s README quickstart is self-contained and still works. `docs/cookbook/README.md`'s outbound links and the 24 tombstone stubs break together — the exact cost of the link-checker gap ledgered in §1. |
| `kno-www` links or sources a migrated cookbook path *(F1)* | Answered before Phase 2 (§8, AC 16) and fixed by a coordinating `kno-www` PR that lands before or with the migration (AC 17). Backstopped by the tombstones, so a reference missed by both humans degrades to a redirect instead of a 404. |
| An old `blob/main/docs/cookbook/<name>.md` link is followed from a merged PR body or an issue *(F3)* | The tombstone stub renders one line pointing at the `kno-examples` recipe. SHA-pinned links keep resolving to the historical file, which is correct — they asked for that commit. |
| A reader copy-pastes an `executed` recipe's excerpt without running the scenario *(F4)* | The page says, next to the verification line, that these commands read a store the earlier stages wrote and names the script to run first. `requires-stages` makes the sentence generated, not remembered. |

## Test plan — what verifies the verifier

The runner is the only new code, so it is the only thing that can silently lie.

- **A deliberately-broken corpus in `cmd/verify/testdata/`**, mirroring `CLAUDE.md`'s
  "deliberately-misbehaving plugin in `testdata/`" convention: a recipe with a flag that does not
  exist; a recipe whose `expected/` disagrees; a recipe quoting a phrase absent from output; a
  recipe with no `verification:` field; a recipe with a hand-edited `last-verified`. Each has a
  table-driven test asserting the runner exits non-zero **and** names the right file. A runner
  that passes everything is the failure mode; these tests are the defense.
- **Extraction tests**: a ` ```bash ` block without `kno-run` is not executed; a `curl` inside a
  Tier B recipe is never executed; a block containing `rm -rf` in prose is inert.
- **Issue-lifecycle dry run**: given a mocked open-issue list and a synthetic signature set,
  assert create-once / edit-on-intersect / close-on-disjoint. No network.
- **Staleness renderer**: table-driven over back-dated front matter, boundary at 30 and 180 days.
- **Determinism**: `run.sh` twice, byte-compare projections. Also serves as a kno flakiness canary.
- **Drift detector**: run against the current `uknoAI/kno@main` and assert it reports the known
  `README.md` vs `tapes/quickstart.tape` divergence. A detector that cannot find a bug we already
  know exists is not a detector.
- **`gitleaks`** over tree and history, in PR CI.
- In `uknoAI/kno`: no new test. `make docs` already fails on the dangling relative links the
  deletion creates, which is the migration's own gate.

## Rollback

Additive and cheap. `kno-examples` is archived read-only; the `uknoAI/kno` migration commit is
reverted, restoring `docs/cookbook/*` from history verbatim and re-pointing `README.md` and
`DESIGN.md`. No Go code, no `go.mod`, no proto, no store, no released artifact is involved, so
rollback is a markdown revert with a passing `make docs`. Recipes authored in `kno-examples` after
the split would need porting back by hand — a real cost, bounded by the number of recipes added,
and the reason to decide this before the repo accumulates history rather than after.

## Docs impact

`uknoAI/kno`: `docs/cookbook/README.md` (rewritten), `README.md` (five links), `DESIGN.md` (three
places), `CONTRIBUTING.md` ("Where to start"), `CHANGELOG.md` (`Unreleased`), `docs/debt.md`.
Proposed ledger entries — the highest number in use today is **130**, so these take the next free
numbers, each with a trigger that can lapse, per the ledger rules:

- *Outbound cookbook links are not link-checked.* `make docs` skips `https://` targets by
  construction, so `docs/cookbook/README.md` can point at recipes that no longer exist.
  **Trigger:** when a recipe is first renamed or removed in `kno-examples`, or before 1.0,
  whichever is first — repay with an external-link checker in `kno-examples`' nightly that
  validates the pointer page from the other side.
- *The judge calibration set has no home.* `DESIGN.md` said `examples/`; this plan says
  `judge/testdata/`; neither exists. **Trigger:** the PR that lands `kno judge calibrate`.
- *Vendor recipes are verified by a human on an unenforced cadence.* The 180-day banner reports
  staleness; nothing prevents it. **Trigger:** before 1.0, or when any vendor recipe's banner has
  been rendered stale for two consecutive quarters.

`kno-examples`: `README.md` (what a scenario is, what the three tiers mean),
`CONTRIBUTING.md`, `LICENSE`, `DATA-PROVENANCE` template, and a `VERIFICATION.md` stating in one
page exactly what each badge does and does not claim — the epistemics page for documentation,
written in the register of `docs/what-the-numbers-mean.md`.

## Accepted risks

- **A cross-repo split costs velocity.** A behavior change and its recipe change are now two PRs
  in two repos, and nothing forces the second. The `main` job is the mitigation, and it is a
  detector, not a gate — it finds the gap after the merge, not before. Accepted knowingly; the
  alternative (a gate) would block kno merges on a docs repo's CI, which is worse.
- **Tier B proves less than it looks like it proves.** "Flags check out" reads, to a skimming
  reader, like "this works". The first draft's answer was label wording, and wording is not a
  control *(F2)*. The answer is now structural: Tier B is denied any positive affordance and
  renders identically to Tier C, so the badge cannot imply what the sentence does not say (§2,
  AC 18). What remains accepted is much smaller — a reader who ignores both the colour and the
  text is unreachable by any mechanism a document has.
- **Nightly reddens on kno release day when a projected field changes.** By design, but it means
  release day has a predictable red build in a second repo, and predictable red builds get
  ignored. Mitigated by the projection being small; if it churns more than once or twice a year,
  the projection is wrong and should shrink.
- **Duplicated fixtures can diverge between detector runs.** A window of up to 24 hours exists
  where `README.md` and the scenario disagree and nothing has said so. Accepted: the alternative
  is a runtime network dependency in a shipped binary.
- **All scenario data is synthetic, so scenarios cannot demonstrate messy real data.** A
  synthetic support corpus is cleaner than any real one, and a reader may over-generalize from
  how well it works. Stated on the scenario's `README.md` in the same register as the README's
  existing honesty about `fake:` scoring 1.000.
- **`kno-www` is a live consumer with its own CI, and this migration reaches into it** *(F1)*.
  The tier badges and `VERIFICATION.md` are a contract with a repository that has its own
  reviewers, its own cadence, and a Playwright crawl that can go red on our schedule. Mitigated
  by sequencing (§8, AC 16–17) rather than accepted blind. What is genuinely accepted is that two
  repositories must now merge in a coordinated window, which is more expensive than one, and that
  the contract's *content* (what a badge claims) is still only as binding as `kno-www`'s
  reviewers choose to make it.
- **Tombstone stubs are permanent** *(F3)*. `docs/cookbook/` never returns to a single file: 24
  one-line files stay forever, and the deletion no longer trips the link checker. Accepted
  deliberately — the alternative ledgers a 404 whose repayment trigger could only fire after a
  reader had already hit it, and a trigger that fires after the damage is not a trigger.

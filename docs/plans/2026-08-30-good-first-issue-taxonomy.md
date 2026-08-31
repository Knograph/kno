# The `good-first-issue` pipeline: audit criteria, taxonomy, and the mechanism that keeps it honest

`CLAUDE.md:124` promises that "`good-first-issue` is a curated pipeline, not a label of neglect:
each one has context, pointers, and a test to make pass. Ring-1 adapters and judge prompts are
the designed on-ramp." [`CONTRIBUTING.md:252`](../../CONTRIBUTING.md) repeats it to the audience
it is aimed at. [`docs/debt.md:4`](../debt.md) repeats it a third time.

Three documents make the promise. Zero issues carry the label. This plan makes the pipeline
visible without weakening the promise into a sticker.

**Phase-1 re-reviewed 2026-08-30 — verdict: amend; amendments applied.** The sharpest finding is
that the first draft **reproduced, in itself, the defect it diagnoses everywhere else**:
`CONTRIBUTING.md` asserts the judge-prompt on-ramp *twice*, and the Docs-impact section corrected
only the second occurrence *(F1)*. Second, acceptance criterion 3's "3–5 labelled issues" rested on
shaping PRs that Accepted risk 4 concedes may slip, plus an unnamed "adapter class" — the class is
now resolved into two concrete, verified gaps so the criterion is met on the day the PR merges
rather than aspirationally *(F2, §2d)*. Third, #14's busywork critique is stated and answered
instead of elided *(F3)*, and fourth, creating the `adapter` label is reconciled with §4's own
argument against area labels *(F4)*. The audit's factual claims were re-verified against
`uknoAI/kno` and stand as written: all fourteen labels exist as named (`good first issue` among
them, at zero open issues), `needs-triage` and `adapter` are declared by the templates and do not
exist, the #134/#129/#110 verdicts hold, every quoted ledger row matches, `Makefile:461` is
byte-exact (copy-pasted from `release-check` at `Makefile:443`, and it runs at `release.yml:124`),
`coretest.ConformIterator`/`CleanupProbe`/`FatalErrorStopsIteration`/`EvalsDuplicateIDs` all exist
and are usable as the G3 test, `judge/` is `doc.go`-only and `goal/` is `exactmatch`-only, and
`CODEOWNERS` routes every path — including `*` — to `@devarispbrown`. The ***(verify)*** tags that
remain are deliberate: each is a fact about GitHub's platform behaviour (the `/contribute` spelling
heuristic, whether a template's unknown label is dropped silently or errors) or about the
maintainer's own calendar (the weekly review budget behind "three to five"), and none of them is
settleable by a repo audit.

## Problem

**Measured 2026-08-30 against `uknoAI/kno`** (`gh api repos/uknoAI/kno/labels`,
`gh api 'repos/uknoAI/kno/issues?state=open&per_page=100'`):

- Fourteen labels exist: `accessibility`, `autorelease: pending`, `autorelease: tagged`, `bug`,
  `documentation`, `duplicate`, `enhancement`, `good first issue`, `help wanted`, `invalid`,
  `no-changelog`, `pricing-drift`, `question`, `wontfix`. Most are GitHub's stock set; three
  (`no-changelog`, `pricing-drift`, and the `autorelease:` pair) are ours.
- **Three open non-PR issues exist**: [#134](https://github.com/uknoAI/kno/issues/134)
  (`pricing-drift`, bot-filed), [#129](https://github.com/uknoAI/kno/issues/129) (pricing table
  cache slot order), [#110](https://github.com/uknoAI/kno/issues/110) (G404/G115 suppression
  anchor). **None carries `good first issue` or `help wanted`.**
- GitHub's `/contribute` page reads the `good first issue` label. With zero issues labelled, it
  renders empty. A visitor who follows `CONTRIBUTING.md`'s "Where to start" section arrives at
  nothing.

Four further defects fall out of the same audit:

1. **The label name in the docs does not exist.** All three documents write `good-first-issue`
   (hyphenated). The label is `good first issue` (spaced, GitHub's default). GitHub's
   `/contribute` heuristic accepts several spellings ***(verify: GitHub's exact accepted set —
   documented behaviour has changed before; confirm before choosing a name)***, but a document
   that names a label the repo does not have is wrong regardless of what the platform tolerates.
2. **Two issue templates apply labels that do not exist.** `.github/ISSUE_TEMPLATE/bug.yml` and
   `feature.yml` set `needs-triage`; `plugin.yml` sets `adapter` and `needs-triage`. Neither
   `needs-triage` nor `adapter` is in the label list. GitHub silently drops labels a form names
   but the repo lacks ***(verify: silently-drops vs. form-submission error — reproduce once on a
   throwaway issue)***, so every plugin/adapter report has been filing unrouted since the
   templates landed.
3. **Half the named on-ramp does not exist.** `CLAUDE.md` names "Ring-1 adapters **and judge
   prompts**". `judge/` contains exactly one file, `judge/doc.go`; `goal/` contains exactly one
   implementation, `goal/exactmatch/exactmatch.go`, and `cli/doctor.go`'s `runDoctor` reports
   `Goals: []string{"exact-match"}`. There is no judge, no prompt, and no calibration set to hold
   an agreement threshold against. Directing a newcomer at "judge prompts" today sends them at a
   package with a doc comment in it.
4. **Two ledger entries self-nominate as `good-first-issue`s, and one of them is wrong.**
   [`docs/debt.md#16`](../debt.md#16) ends "A `make selftest` target … is a well-scoped
   `good-first-issue`." [`#61`](../debt.md#61) ends "The canary is the cheaper half and is a good
   `good-first-issue`." #61's canary is *an assertion against the live Anthropic API*: it needs a
   credential, `KNO_LIVE_TESTS=1`, and a budget cap, none of which a drive-by contributor has.
   The ledger's own nomination fails the ledger's own audience test. Labelling on the strength of
   a self-nomination would ship that error to the front page.

The failure this plan prevents is the one CLAUDE.md's Debt Ledger section names explicitly: a
curated set that nobody re-reads is worse than none, because it advertises maintainer attention
that stopped.

## Design

### 1. The audit criteria — three gates, each with a stated verification

An issue may carry `good first issue` only if a reviewer can point at all three of CLAUDE.md's
own words. The rule is written so that "I think a beginner could do this" is not an argument.

| Gate | CLAUDE.md's word | How it is verified before the label goes on |
|---|---|---|
| **G1 — Context** | "context" | The issue body states the problem *and why it is a problem*, in the vocabulary, without requiring the reader to open the ledger. A link to a ledger row is a citation, not the context: `docs/debt.md` is 137 KB in one table and a newcomer will not find row 16 in it. Verification: a reader who has never seen the repo can restate the goal from the issue alone. |
| **G2 — Pointers** | "pointers" | At least one `path:line` citation to the code that must change, and one to the nearest existing example of the same shape. Verification: every cited path resolves on `main` at labelling time (mechanically checkable — see §5). "Somewhere in `adapters/`" fails. |
| **G3 — A test to make pass** | "a test to make pass" | A **named, existing** test function, `make` target, or `coretest` harness call that is red (or absent-and-specified) before the change and green after. Verification: the issue names it, and the name exists in the tree, or the issue explicitly ships the test in the maintainer's own preceding commit. |

**G3 is the gate that does the work, and it is the one that will disqualify most candidates.**
It is also the gate the repo is best equipped to pass in exactly one area, which is §2's finding.

Two clarifications, both deliberate:

- **A credential is a disqualifier.** If the test cannot be made green by `make test` on a laptop
  with no API keys, it is not a first issue, whatever else is true of it. This is what kills
  [`#61`](../debt.md#61)'s self-nomination, and it is a rule rather than a judgement so that the
  next live-API candidate does not have to re-argue it.
- **A `CODEOWNERS` path that routes to a design decision is a disqualifier.** `CODEOWNERS` routes
  `/proto/`, `/core/`, `/stats/`, `/bridge/`, `/plugin/`, `/judge/`, `/docs/adr/` and
  `/docs/debt.md` to `@devarispbrown` — but so does `*`, so the routing is currently uniform and
  carries no signal (§4). The disqualifier is the *nature* of the change, not the path: an issue
  whose resolution requires picking between two designs is a plan (Phase 0), and CLAUDE.md
  requires a plan for it. Handing a newcomer a task that starts with "write a Phase-0 plan and
  get it adversarially reviewed" is not an on-ramp.

### 2. The candidates, audited — this is the substance

Every open issue and every open ledger entry, with a verdict. Ledger status read from
`docs/debt.md` on 2026-08-30: 84 rows, 46 without a disposition marker plus 6 marked `RE-DATED`
and therefore still open.

#### 2a. Existing issues

| Issue | G1 context | G2 pointers | G3 test | Verdict |
|---|---|---|---|---|
| [#134](https://github.com/uknoAI/kno/issues/134) — bedrock regional multiplier not checked against AWS's price list | yes (bot report) | yes | no | **No label.** Bot-filed by `pricing-check.yml`, and CONTRIBUTING's pricing section says each drift issue "closes itself with a verification comment on the first run whose report no longer carries its finding". A human taking it would fight the bot. Leave to the weekly job. |
| [#129](https://github.com/uknoAI/kno/issues/129) — pricing table cache read/write slots swapped vs. published column order | yes | yes (`adapters/agent/pricing/table.go`, `price()`) | no | **`help wanted`, not `good first issue`.** It is a spend-path change, and CONTRIBUTING is explicit: "A price edit is a spend-path change and is reviewed like one; there is no 'it's just a number' lane." The issue also says "pinned as-is … Track until the table next moves" — it is a *tracking* issue with a deliberate no-op today. |
| [#110](https://github.com/uknoAI/kno/issues/110) — G404/G115 suppression anchor | yes | yes | n/a | **No label.** This issue exists *to be linked*, not to be closed: the lint policy forbids a naked `//nolint`, `docs/debt.md#75` is repaid, and this is the live anchor that replaced it. There is no work in it. Labelling it would be the exact "label of neglect" the promise disowns. |

**Net: zero of three existing issues qualify.** The pipeline cannot be made visible by labelling
what exists; it has to be *stocked*.

#### 2b. Ledger entries

| Entry | What | Verdict and reasoning |
|---|---|---|
| [#16](../debt.md#16) | Nothing verifies a gate actually *fails* when it should; `make selftest` proposed | **Qualifies after shaping — the flagship.** G1 and G2 are already in the row, including the concrete precedent (the `.SHELLFLAGS` defect: "eleven recipes reported success while the commands inside them failed"). G3 is missing *by construction* — the deliverable **is** the test. Resolution: the maintainer lands `make selftest` covering **one** gate as the worked pattern, then the issue is "extend it to the remaining ones", with the existing target as both pointer and the test to make pass. Split into one issue per gate so several people can take it. |
| [#14](../debt.md#14) | Actions pinned to mutable tags (`@v4`, `@v5`), not SHAs | **Qualifies after shaping.** Mechanical, bounded to `.github/workflows/*.yml`, needs no domain knowledge, and Dependabot already understands SHA pins. G3 is missing: nothing checks the pins. Same resolution — maintainer lands the checker (a `uses:` scan asserting a 40-hex ref) *failing*, contributor makes it green. It starts with at least one passing case: `.github/workflows/release-please.yml` already pins `googleapis/release-please-action@45996ed1…`, deliberately, and the checker must not regress that. **The busywork critique, stated rather than elided *(F3)*:** once the checker exists, the contributor's remaining work is looking up commit SHAs and pasting them. Calling this "the cleanest case" without saying so oversells it — there is no judgement left in the task. It is kept anyway, and defended on the grounds it actually earns: a first PR's real content is DCO sign-off, a Conventional Commit title, a CHANGELOG entry and a red-to-green CI run, and a diff where *the change itself* cannot be got wrong is the best available vehicle for learning those four things. It is written up as what it is — a mechanical chore that teaches the workflow — never as a design contribution. If a candidate with comparable G3 clarity and more substance appears, #14 is demoted rather than defended. |
| [#12](../debt.md#12) + [#71](../debt.md#71) | `make vuln` scans only the shipping module; `tools/` and `tools/goreleaser` (~500 packages, in the job holding `id-token: write`) are unscanned | **`help wanted`, not `good first issue`.** #71's trigger says "**With [#12](#12)** — the same fix covers both". But #12 records the trap in its own Why column: "`govulncheck ./...` in a module with no Go packages errors rather than scanning its dependencies." Getting past that means knowing about binary-mode scanning and tool-module layout. That is a good second contribution, not a first. |
| [#72](../debt.md#72) | `make release-check` / `release-stamp` outside `make check` | **`help wanted`.** The row calls it "a one-line change once the compile is reliably cache-warm in CI" — but the judgement is whether a ~500-package compile belongs in a gate "whose entire design is fail-fast-cheapest-first". That is a design call, and a contributor cannot make it. |
| [#30](../debt.md#30) | `backfillScoreValues` buffers every scored row (215 MB `TotalAlloc` at 200k rows) | **`help wanted`.** Well-specified — the row even names the fix, "keyset-paging on `(run_id, case_id)` inside the same transaction" — and a measured allocation figure makes an assertable test obvious. But it is a migration path over a live database; a wrong page boundary silently loses backfilled rows. Not first-issue risk. |
| [#61](../debt.md#61) | Run-fatal escalation on an English prose prefix; ledger nominates a canary as a GFI | **Rejected, and the row is amended.** The canary asserts against the live Anthropic API — a credential, `KNO_LIVE_TESTS=1`, and a budget cap, per CONTRIBUTING's test rules. It cannot be made green by `make test`. The nomination sentence is struck from the ledger row in this plan's PR; leaving it would make the ledger contradict the criteria. |
| [#24](../debt.md#24), [#17](../debt.md#17), [#5](../debt.md#5), [#7](../debt.md#7), [#9](../debt.md#9), [#34](../debt.md#34), [#35](../debt.md#35), [#82](../debt.md#82), [#60](../debt.md#60) | conditional triggers ("when a second `Store` backend lands", "when the first `//go:build` platform fork lands", "when the release-please action SHA is next upgraded", …) | **No label.** The trigger has not fired: there is no work to do. Labelling an unfired trigger is how a curated set fills with issues nobody can start. |
| [#6](../debt.md#6), [#10](../debt.md#10), [#62](../debt.md#62), [#36](../debt.md#36) | field-naming policy; the proto-alias design; the `kno.yaml` config layer; token-count approximation | **No label.** Each is a Phase-0 plan with an adversarial review, some of them (#10, #36) with statistical or spend-path consequences. Explicitly out of scope per §1's second disqualifier. |

#### 2c. Candidates found by inspection, not in the ledger

| Candidate | Verdict |
|---|---|
| **`make ledger-check` prints the wrong success line.** `Makefile:461` is `@printf '\033[32m  OK  \033[0m .goreleaser.yaml is valid\n'` — copy-pasted from `release-check` two targets above. `scripts/ledger-check.py` already prints its own correct OK line, so a passing run reports the ledger gate as a goreleaser gate. It runs in `release.yml:124` on every tag. | **`good first issue`, and it qualifies today.** G1: a release gate that misreports which gate ran. G2: `Makefile:447-461`, with `release-check` at `Makefile:443-446` as the mistaken source. G3 needs one line — a Make-output assertion in the same style the repo already uses. Small, real, visibly a *bug* rather than a chore, and it teaches the CHANGELOG/DCO/PR-title workflow on a diff nobody can get wrong. **The best available first issue.** |
| **Issue templates name labels the repo does not have** (`needs-triage`, `adapter`). | **Not a contributor issue.** Creating a label is a repo-settings action a contributor has no permission for. Maintainer chore, folded into this plan's deliverables (§4). |
| **A new Ring-1 pool or evals adapter.** `adapters/pool/` has `csv`, `hf`, `jsonl`, `markdown`; `adapters/evals/` has `braintrust`, `hf`, `jsonl`, `langfuse`, `langsmith`, `mine`, `split`. | **The strongest GFI *class* in the repo, and the only one where G3 is satisfied by code that already exists.** `coretest.ConformIterator` (`coretest/coretest.go:162`), `coretest.CleanupProbe` (`:263`), `coretest.FatalErrorStopsIteration` (`:211`) and `coretest.EvalsDuplicateIDs` (`:250`) *are* the test to make pass — CONTRIBUTING already instructs adapter authors to call them. Seven worked examples exist to copy. This is what CLAUDE.md meant by "Ring-1 adapters are the designed on-ramp", and it needs no shaping work beyond writing the issue. **Stock the pipeline here first — and §2d names which gaps, because "a class" is not a countable deliverable.** *(F2)* |
| **Judge prompts** — the other half of CLAUDE.md's named on-ramp. | **Cannot be labelled; the docs are corrected instead.** `judge/` is `doc.go` only; `goal/` is `exactmatch` only. There is no calibration set for a prompt change to be measured against, so CLAUDE.md's "a judge prompt change that drops agreement below threshold fails CI" has nothing to fail. The claim appears **twice** in `CONTRIBUTING.md`, not once *(F1)*: at `:252-255` in "Where to start", and — one screen earlier, in the **opening paragraph** at `:3-4` — "Ring-1 adapters and judge prompts are the designed on-ramp", which is the first sentence a contributor reads. The first draft of this plan corrected only the second site, which would have left the front door asserting the fiction while the back door disowned it: the same two-sources-of-truth failure this plan exists to fix, self-inflicted. Both are amended to say adapters are the on-ramp *today* and judge prompts become one when `judge` lands (v0.2, per `DESIGN.md:398`). |

#### 2d. The day-one issue set — three named issues, not a class *(F2)*

The first draft's acceptance criterion 3 ("3–5 labelled issues") was not guaranteed by its own PR.
It leaned on the §2b flagships, which Accepted risk 4 concedes are unfunded and may slip, plus an
uncounted "adapter class"; and Accepted risk 2's "re-argue if the count is below three at two
consecutive reviews" is a *reaction*, not a gate. If the shaping PRs slipped, the deliverable was
one `Makefile` fix and a hand-wave. So the class is resolved into named gaps here.

The two adapter gaps were chosen on a stricter test than "a newcomer could do it": in each case
**the repo's own documentation already claims the adapter exists**, so the issue is a *defect
report* rather than a wishlist entry — which is what makes G1 self-evident and what stops the issue
from being make-work invented to fill a page.

1. **`make ledger-check` prints the wrong success line** (`Makefile:461`, §2c). Shipped *as* the
   labelled issue rather than fixed in this PR — criterion 6's second branch, and the PR body says
   so.
2. **`adapters/pool/parquet` does not exist.** `adapters/pool/doc.go` states "Adapters: jsonl, csv,
   **parquet**, markdown-dir, and MCP", and `DESIGN.md:178`, `:223`, `:344` and `:397` all list
   parquet among the **v0.1** Ring-1 pools. `adapters/pool/` contains `csv`, `hf`, `jsonl`,
   `markdown` — no parquet. **G1:** a package doc that is false on `main`, in the package CLAUDE.md
   names as the on-ramp. **G2:** `adapters/pool/csv` is the nearest worked example, one directory
   over. **G3:** `coretest.ConformIterator` (`coretest/coretest.go:162`), `CleanupProbe` (`:263`)
   and `FatalErrorStopsIteration` (`:211`) — the calls CONTRIBUTING already instructs adapter
   authors to make.
   **Scope fence, and it is load-bearing:** a Parquet reader needs a new dependency, and CLAUDE.md
   requires new dependencies to be justified in the PR body (what it does, why stdlib or an
   existing dep cannot, license, maintenance signal). That is a supply-chain decision and it is
   **not a contributor's to make**. The maintainer names the module in the issue body, pre-decided;
   the contributor's PR quotes the justification rather than authoring it. Without that fence this
   issue trips §1's second disqualifier — a task whose first step is choosing between two designs —
   and must not be labelled.
3. **`adapters/evals/csv` does not exist.** `adapters/evals/doc.go` states "Adapters: jsonl, **csv**,
   and transcripts (via mine)". `adapters/evals/` contains `braintrust`, `hf`, `jsonl`, `langfuse`,
   `langsmith`, `mine`, `split` — no csv. **G1:** the same defect shape. **G2:** two worked
   examples — `adapters/pool/csv` for the parsing, `adapters/evals/jsonl` for the `core.Evals`
   surface (`Cases`, `CountSplits`, `ContentHash`, the dev/holdout split). **G3:** the same
   `coretest` calls plus `coretest.EvalsDuplicateIDs` (`:250`), which is `Evals`-specific and is
   precisely the trap a first CSV implementation falls into. **No new dependency** — `encoding/csv`
   is stdlib — which makes this the cleanest of the three and the one to write first.

Three issues, none of which depends on the #16/#14 shaping work, so criterion 3's floor is met by
this PR alone; #16 and #14 are upside that takes the count toward five. One consequence worth
stating: both adapter issues also retire a documentation defect, and if a contributor concludes the
right fix is to **correct the `doc.go` line** instead of writing the adapter, that is a legitimate
outcome and the issue body says so. An on-ramp that accepts exactly one answer is a homework
assignment.

### 3. What to do with a good candidate that lacks the test

Three options were live. The plan picks the first and refuses the third.

**Chosen: the maintainer writes the failing test, then labels.** For #16 and #14 the deliverable
is inverted — a small maintainer PR lands the check in its failing (or one-case) state, and the
issue becomes "make it green / extend it". This costs the maintainer one PR per issue and buys
the promise literally: the contributor really does have a test to make pass, and their PR is
verifiable by CI rather than by taste. It also means the issue cannot rot into vagueness, because
a red check is not a prose description.

**Rejected: `help wanted` as the parking lot.** `help wanted` *is* used (§4) — but as a statement
about **scope and appetite**, not as a holding pen for under-specified work. An issue that is
genuinely well-shaped but merely larger than a first contribution is `help wanted`. An issue that
is *not yet shaped* is neither: it stays unlabelled, because a label on an unshaped issue is the
neglect the promise disowns.

**Rejected: a `needs-shaping` staging label.** It sounds tidy and is a trap. A staging label is
publicly visible and reads to an outsider as "the maintainer knows this needs work and has not
done it" — which is an accurate but demoralising signal, and it accumulates. Worse, it creates a
third state that the cadence in §5 would have to review, doubling the review surface to track
work that has not started. The ledger is already the staging area: an entry that is a future GFI
sits in `docs/debt.md` with its trigger, exactly as designed, and becomes an issue when the
maintainer shapes it. **Adding a label to model "not ready" duplicates the ledger.**

### 4. The label taxonomy

| Label | Meaning | Who applies |
|---|---|---|
| `good first issue` | Passes G1+G2+G3. First contribution to this repo. | Maintainer only, at creation time. |
| `help wanted` | Shaped, scoped, and the maintainer is not working on it. Larger than a first issue or needing domain judgement. | Maintainer. |
| `bug`, `enhancement`, `documentation`, `question` | Kind. Existing GitHub defaults; `bug` and `enhancement` are already applied by the issue templates. | Template or triager. |
| `adapter` | **Create it.** `.github/ISSUE_TEMPLATE/plugin.yml` already sets it and the label does not exist. | Template. |
| `needs-triage` | **Create it.** All three templates already set it. | Template. |
| `no-changelog`, `pricing-drift`, `autorelease: *` | Machine-read by `changelog.yml`, `pricing-check.yml`, release-please. Do not touch. | Workflows. |

**Keep `good first issue` under GitHub's default spelling; fix the three documents instead.**
The docs write `good-first-issue`; the label is `good first issue`. Renaming the label to match
the prose risks the `/contribute` heuristic (the exact accepted set is a platform behaviour we do
not control ***(verify)***) for the sake of a hyphen. Editing prose is free and cannot break a
platform feature. `CLAUDE.md:48`, `CLAUDE.md:124`, `CONTRIBUTING.md:252` and `docs/debt.md:4` all
change to the literal label name, in backticks.

**No area labels — decided against, and this is the arguable one.** GitHub labels and `CODEOWNERS`
would be two hand-maintained maps of the same package boundaries, and CLAUDE.md's DRY rule
("extract on the third occurrence") argues against the second copy before the first has proved
insufficient. The stronger argument is that `CODEOWNERS` today routes **every path to
`@devarispbrown`**, including the `*` catch-all — so an area label would partition a set of one
reviewer, which is decoration. Revisit when a second name appears in `CODEOWNERS`; that is the
trigger, and it is a condition rather than a date because it is genuinely event-driven. Until
then `bug`/`enhancement`/`adapter` carry all the routing signal there is.

**`adapter` is an area label, and creating it is a real exception to the paragraph above** *(F4)*.
"The template already sets it" explains how the label came to be declared, not why it should exist,
and it is not a taxonomy argument. The honest reconciliation is that the no-area-labels case is
about **routing work to reviewers** — a job `CODEOWNERS` already does, and which an area label
cannot improve while `CODEOWNERS` holds one name. `adapter` is not doing that job. It is doing the
job §2c and §2d identify as the repo's only well-stocked GFI class: marking the one area where a
contributor can find a starting point from the label alone, without a maintainer shaping anything.
It is therefore justified **by the pipeline, not by the template**, and it inherits the same
trigger as the rest of §4 — if a second area label is ever proposed, that is the third occurrence,
and the taxonomy re-opens as a whole rather than one label at a time.

### 5. Who maintains it, and on what cadence

CLAUDE.md's own rule — "a trigger without a date is not a trigger" — applies to this pipeline as
much as to a ledger row. So the cadence is a date attached to an event the repo already has.

- **Owner: `@devarispbrown`**, the sole `CODEOWNERS` entry. Named in the label descriptions.
- **Cadence: every minor release, in the same pass as the Debt Ledger review** that CLAUDE.md
  already mandates ("The ledger is reviewed at every minor release"). Piggybacking is deliberate:
  a separate cadence is a separate thing to forget, and the ledger review is already the moment
  the maintainer is reading every open entry with repayment in mind — which is exactly when "is
  this now a first issue?" is cheapest to answer.
- **Staleness rule with a date, not a vibe:** an issue carrying `good first issue` for **90 days**
  with no assignee is re-audited against G1–G3 at the next ledger review. It is either re-shaped,
  demoted to `help wanted`, or closed with a written reason. Silent carryover is not an option —
  the same sentence the ledger rules use, because it is the same failure.
- **Enforcement, deliberately weak:** extend `scripts/` with a checker that, for each open issue
  carrying `good first issue`, asserts (a) every `path:line` citation in the body resolves on
  `main`, and (b) the issue is under 90 days old or has an assignee. Run it in `nightly.yml`,
  **reporting**, not gating. A gate that fails `main` because a *GitHub issue* went stale would
  be switched off within a month, and `docs/debt.md#70` is the entry about exactly that failure.
  This checker's honest job is to make rot visible at the next review, not to stop a merge.

### 6. How an issue gets created from a ledger entry

The ledger is a prose table row; an issue is a different artifact with a different reader. The
translation is a maintainer act and is not automated. Auto-filing an issue per ledger entry would
produce 52 issues, most of them unstartable, which is the "label everything" failure (§Alternatives)
wearing a script.

**Author: the maintainer.** **Template: a new
`.github/ISSUE_TEMPLATE/good-first-issue.yml`**, maintainer-facing rather than reporter-facing —
it exists so the three gates are checked at the keyboard rather than remembered. Required fields:

- **Context** (G1) — the problem in the vocabulary, self-contained. Ledger link as citation.
- **Pointers** (G2) — `path:line` for what changes, and `path:line` for the nearest worked example.
- **The test to make pass** (G3) — a named target, function, or `coretest` call, plus the exact
  command a contributor runs to see it red.
- **Definition of done** — pointing at `CONTRIBUTING.md`'s DoD, plus the two things a first-timer
  reliably misses: `git commit -s` (DCO) and a Conventional Commit PR title.
- **Scope fence** — what is explicitly *not* in this issue, so a well-meaning contributor does not
  arrive with a 600-line PR that needs a Phase-0 plan.

The ledger row then gains an issue link in its Why column. The row remains the debt record; the
issue is the work record. They are not merged, because the ledger's release-time review reads
rows and `scripts/ledger-check.py` parses them positionally (`line.split("|")`, `id="(\d+)"`) —
restructuring rows to carry issue state would break that parser for no gain.

### 7. Interaction with the 48-hour response promise

`CLAUDE.md` and `CONTRIBUTING.md:3` both promise "every external PR gets a first response within
48h, even if it's 'reviewing this week.'" Publishing a `/contribute` page is the first thing this
repo has done that actively *solicits* those PRs, so the promise stops being theoretical on the
day the first label lands.

Three consequences, all accepted deliberately:

1. **Stock conservatively.** Ship **three to five** labelled issues, not thirty. The binding
   constraint is review throughput, and a queue of unanswered PRs from people the docs invited is
   a worse outcome than an empty `/contribute` page. Five is roughly one week's review capacity
   for a single maintainer ***(verify: the maintainer's actual weekly review budget — five is an
   estimate, and the number should be set from the real figure)***.
2. **The 48 hours starts at the PR, not the issue.** A comment on an issue ("I'll take this") is
   not a PR and does not start the clock. Say so in the template, so the promise is not silently
   widened by the act of publishing issues.
3. **No auto-assignment.** GitHub's assignment is a lock; a contributor who assigns themselves and
   vanishes blocks the issue invisibly. Instead: a contributor comments to claim, and the issue is
   released after **14 days** of no PR with a friendly comment. The 14 days is a date, which is
   what the repo's own rule requires of a trigger.

## Acceptance criteria

Each is stated as an observable a reviewer can check without trusting a summary.

1. `gh api repos/uknoAI/kno/labels --jq '.[].name'` includes `adapter` and `needs-triage`, so that
   `.github/ISSUE_TEMPLATE/plugin.yml`'s and `bug.yml`'s declared labels are applied rather than
   dropped.
2. `grep -rn 'good-first-issue' CLAUDE.md CONTRIBUTING.md docs/debt.md` returns **no hit outside
   this plan and `docs/plans/`** — every prose reference names the literal label `good first issue`.
3. `gh issue list --repo uknoAI/kno --label 'good first issue' --state open` returns **between 3
   and 5 issues**, and the count is non-zero — i.e. `https://github.com/uknoAI/kno/contribute`
   renders a non-empty list. ***(F2)*** The floor of three is met by this PR alone and does **not**
   depend on the unfunded shaping work in Accepted risk 4: the three issues are named in §2d
   (`Makefile:461`, `adapters/pool/parquet`, `adapters/evals/csv`), each with its G1/G2/G3 already
   identified. #16 and #14 take the count toward five if their shaping PRs land.
4. For **every** issue returned by criterion 3, the body contains: at least one `path:line`
   citation that resolves on `main` (G2), and a line naming a `make` target or Go test function
   that exists in the tree (G3). Checkable by the nightly reporter in §5.
5. `.github/ISSUE_TEMPLATE/good-first-issue.yml` exists and its required fields are exactly
   Context, Pointers, The test to make pass, Definition of done, Scope fence.
6. `Makefile:461` no longer claims `.goreleaser.yaml is valid` for the `ledger-check` target, and
   an assertion on that target's output exists — **or** that fix is *deliberately withheld* and
   filed as the labelled first issue. Exactly one of the two is true, and the PR says which.
7. `docs/debt.md#61`'s row no longer describes its live-API canary as a `good-first-issue`, and
   `docs/debt.md#16`'s row links the issue(s) created from it.
8. `grep -n 'judge prompt' CONTRIBUTING.md` returns no claim that judge prompts are an on-ramp
   available today — checked at **both** sites, the opening paragraph (`CONTRIBUTING.md:3-4`) and
   "Where to start" (`:252-255`) ***(F1)***. Both instead name Ring-1 pool/evals adapters with the
   `coretest` harness calls (`ConformIterator`, `CleanupProbe`) as the test to make pass, and
   "Where to start" points at §2d's named gaps.
9. A `scripts/` checker exists that, given the open `good first issue` set, reports stale
   (>90 days, unassigned) issues and unresolvable `path:line` citations; it is wired into
   `.github/workflows/nightly.yml` as **report-only** and does not appear in `make check`.
10. `docs/debt.md` gains a row for the deferred half of this plan (§Accepted risks) with a
    repayment trigger that names a date or a condition that cannot self-satisfy.

## Alternatives considered

**A. Just add the label to everything plausible.** Take the 46 open ledger entries and the three
open issues, label the ones that look small, and let `/contribute` fill up. Rejected, and it is
worth saying why at length because it is the tempting option: the audit in §2b found that **zero**
open issues and **zero** un-shaped ledger entries pass G3 as written. Labelling them would put
issues on the front page whose triggers have not fired (#24, #17, #82 — literally no work to do
yet), whose resolution is a Phase-0 plan (#6, #62), or which need an API credential (#61). The
first contributor to pick one and discover this learns that the repo's most prominent promise is
decorative, and CONTRIBUTING's "not a label of neglect" becomes a sentence they can quote back.
The promise is more valuable than the page.

**B. Automate: generate one issue per open ledger entry from `docs/debt.md`.** A script parses the
table the way `scripts/ledger-check.py` already does and files an issue per row. Rejected on two
counts. Mechanically, it duplicates a parser — `ledger-check.py`'s docstring explains why it is
"deliberately narrowly" scoped and refuses to evaluate prose triggers, and a second consumer with
richer ambitions is exactly the drift that argument warns about. Substantively, it inverts the
promise: curation is the *whole* claim, and a generator produces 52 issues of which the audit says
about four are startable. Automation belongs on the *decay* side (§5's staleness reporter), where
the machine is checking a human's work, not replacing it.

**C. Skip labels; keep the ledger as the only on-ramp.** Point contributors at `docs/debt.md` and
be done. Rejected because it does not fix the stated problem — `/contribute` stays empty, and the
ledger is a 137 KB single-table document that a newcomer cannot triage. It also leaves the
`CLAUDE.md`/`CONTRIBUTING.md` promise unfulfilled, which means the honest version of this option
is to *delete the promise*, and the promise is a good one.

**D. Rename the label to `good-first-issue` to match the prose.** Rejected in §4: it risks a
platform heuristic we do not control in order to fix a hyphen, when editing four lines of prose is
free and cannot break anything.

## Affected packages / repos

- `.github/ISSUE_TEMPLATE/good-first-issue.yml` — new.
- `.github/ISSUE_TEMPLATE/{bug,feature,plugin}.yml` — unchanged; their labels start working once
  the labels exist.
- `.github/workflows/nightly.yml` — one report-only step.
- `scripts/` — one new checker (staleness + citation resolution). **Does not touch
  `scripts/ledger-check.py`.**
- `CLAUDE.md`, `CONTRIBUTING.md`, `docs/debt.md` — prose corrections (label spelling, the judge-
  prompt on-ramp claim, #16's and #61's rows).
- `Makefile:461` — the `ledger-check` success line, if it is fixed here rather than filed.
- **Repo settings (not a file):** two new labels. Recorded in the PR body since it is a change
  nobody can see in the diff.
- **Not touched:** `core/`, `stats/`, `adapters/`, `proto/`, `gen/`, `cli/`, `api/`.

## Proto / schema impact

**None.** Verified against `proto/kno/v1/` (`asset`, `case`, `common`, `error`, `event`,
`portfolio`, `report`, `run`, `tuner`, `valuation`): nothing here touches a wire type, so
`buf breaking` has nothing to compare and `make typecheck-proto` is unaffected. No generated code
in `gen/` changes.

## Edge cases

| Case | What happens | Mitigation |
|---|---|---|
| A labelled issue goes stale — the code it points at moves, the ledger entry is repaid, the pointer rots | Contributor follows a `path:line` into nothing; the repo looks abandoned at its most public surface | §5's nightly reporter resolves every citation and flags the misses; the 90-day re-audit catches the rest. Report-only, never a `main`-blocking gate |
| A ledger entry is **repaid while its issue is labelled** | The issue is now a lie, and a contributor may open a PR re-doing repaid work | The ledger row carries the issue link (§6), so the repayment edit is looking straight at it. Add to the ledger-review checklist: repaying a row closes its issue in the same PR. The nightly reporter also flags a labelled issue whose ledger row now begins `REPAID` — mechanical, since that is the same marker `scripts/ledger-check.py` reads |
| A contributor claims an issue and vanishes | The issue looks taken and nobody else starts it | No assignment (§7). Claim by comment; released after **14 days** with a friendly comment. The reporter surfaces claimed-but-PR-less issues past that date |
| Two contributors open PRs for the same issue | One person's work is wasted, which is the fastest way to lose a contributor | The claim comment is the ordering, first comment wins, and the template says so. If both PRs are already open: the first-opened merges, the second gets a written thank-you, a specific pointer to another labelled issue, and — if the second is genuinely better — the first is closed *with that said out loud* |
| A duplicate issue is filed against a labelled one | Two curated issues describing one task; the `duplicate` label exists but says nothing about which survives | Close the newer, comment the link on both, and move any *better prose* from the duplicate into the survivor before closing — the newcomer's phrasing is frequently clearer than the maintainer's |
| A labelled issue turns out to need a Phase-0 plan once someone starts it | The contributor is now facing the full CLAUDE.md workflow they were not warned about | The maintainer says so immediately (48h), converts the issue to `help wanted`, writes the plan themselves or with the contributor, and **the misjudgement goes in the PR body** — the audit criteria failed and that is a finding, not an embarrassment |
| An external contributor files something excellent and unlabelled | The 48h clock is on the PR, not the issue, and a great unsolicited PR can sit | Unchanged from today; noted here so the label pipeline is not read as narrowing the promise |
| GitHub silently drops a template label that does not exist | Already happening for `adapter`/`needs-triage` | Fixed by creating them (criterion 1); a template naming a nonexistent label is now a review item |

## Test plan

Most of this plan is prose and repo settings, so the test plan is mostly assertion-by-observable
(§Acceptance criteria). What is genuinely testable:

- **`make docs` link checker** already walks every `*.md` and fails on an unresolvable relative
  link (`Makefile:398-419`). Every `docs/debt.md#N` and `CONTRIBUTING.md` link this plan edits is
  covered by it for free — that is the regression test on the prose edits.
- **The staleness/citation checker** ships with unit tests over recorded fixtures: an issue body
  with a resolving citation, one with a rotted citation, one with no citation at all, one 91 days
  old and unassigned, one 91 days old and assigned. No live GitHub calls in the test path — the
  fixture is the API response shape, consistent with the repo's determinism-first rule.
- **`Makefile` `ledger-check` output assertion** (if the fix lands here): run the target with a
  known-good `VERSION` and assert the emitted OK line names the ledger, not `.goreleaser.yaml`.
  This is the same class of check `make release-identity-check` already performs on hand-
  maintained strings.
- **No new coverage surface** in `core/`, `stats/`, `bridge/`, `plugin/`, so the 85% floors and the
  `.coverage-baseline` ratchet are unaffected.

## Rollback

Cheap and complete, in decreasing order of urgency:

1. **Remove the label from the issues** (`gh issue edit --remove-label 'good first issue'`).
   `/contribute` empties within a page load and the promise is un-made. Seconds.
2. **Revert the prose PR.** Documentation only; no code depends on it.
3. **Delete the nightly step.** Report-only, so nothing was gating on it and nothing breaks.
4. The two new labels (`adapter`, `needs-triage`) are **not** rolled back — they fix an existing
   defect (templates naming labels that do not exist) and are independent of the pipeline.

The one thing that cannot be rolled back is a contributor's experience of a bad first issue, which
is the whole argument for stocking conservatively (§7.1).

## Docs impact

Per CLAUDE.md's gate, the behaviour change and the docs change are the same PR:

- **`CONTRIBUTING.md`** — **two occurrences, not one** *(F1)*. The first draft of this plan listed
  only "Where to start" here, and thereby committed the exact fault it diagnoses: the **opening
  paragraph** at `:3-4` makes the same claim to the same audience one screen earlier — "Ring-1
  adapters and judge prompts are the designed on-ramp" — and it is the first sentence a contributor
  reads. Correcting the section while leaving the opener would have shipped a document that
  contradicts itself between its first screen and its middle. Both change in this PR: the opener
  names Ring-1 adapters alone (the 48-hour promise in the same sentence is untouched), and "Where
  to start" is rewritten with the literal label name, Ring-1 pool/evals adapters as *the* on-ramp,
  §2d's two named gaps as the concrete starting points, `coretest.ConformIterator`/`CleanupProbe`
  named as the test to make pass, and judge prompts moved to "when `judge` lands (v0.2)". The check
  is a `grep`, and it is criterion 8.
- **`CLAUDE.md:48` and `:124`** — label spelling; the "Ring-1 adapters and judge prompts" sentence
  gains the same tense correction. CLAUDE.md is process law, so this is a `CODEOWNERS`-routed edit.
- **`docs/debt.md`** — `:4` label spelling; #16 gains issue links; #61 loses its GFI nomination
  and gains a one-line reason.
- **`README.md`** — unchanged. It links `CONTRIBUTING.md` already; adding a "good first issue"
  section would be a fourth copy of a promise that already appears three times.
- **No godoc, no CLI help, no OpenAPI, no vhs tape** — nothing user-facing in the binary changes.
- **CHANGELOG** — an entry under `Unreleased`. The PR title will be `docs:`, which
  `changelog.yml` exempts, but the pipeline is a real change to how the project receives
  contributions and an exemption used for convenience is the habit that entry #49 is about.

## Accepted risks

Each mirrors to `docs/debt.md` with a trigger, per CLAUDE.md. None is "someday".

1. **The pipeline depends on one person's attention, and `CODEOWNERS` has one name.** Every
   mitigation here (the release-cadence review, the 90-day re-audit, the shaping PRs) is work for
   `@devarispbrown`. If that attention lapses, the set rots exactly as predicted, and the nightly
   reporter will faithfully report the rot to nobody. *Trigger: revisit when a second name appears
   in `CODEOWNERS`, or at 1.0, whichever is first.*
2. **Three to five issues is a thin pipeline.** `/contribute` with four entries is honest but not
   inviting, and the §2 audit says we cannot responsibly ship more. *Trigger: restock at each
   minor release; if the count is below three at two consecutive ledger reviews, the promise is
   re-argued rather than quietly under-delivered.*
3. **The staleness checker is report-only and can be ignored.** It is deliberately not a gate
   (§5), which means its output is a nightly log line nobody is obliged to read. This is the
   `docs/debt.md#70` shape — a check occupying the slot where a real one would go — accepted here
   because the alternative (failing `main` on a stale GitHub issue) is strictly worse and would be
   switched off. *Trigger: before 1.0, or the first time a labelled issue is found rotted that the
   reporter had already flagged.*
4. **The shaping PRs for #16 and #14 are unfunded work.** Both flagship candidates need a
   maintainer PR before they are labellable, and neither is written. **Criterion 3 no longer
   depends on them** *(F2)*: if they slip, the pipeline still ships §2d's three named issues, which
   is the floor. What is lost is *variety*, not count — three issues of which two are adapter gaps
   is a narrower invitation than intended, and that is the honest cost of the slip. *Trigger: a
   date — if the shaping PRs have not landed by the next minor release, #16 and #14 are demoted to
   `help wanted` and the shortfall is recorded rather than carried.*
5. **The `/contribute` label heuristic is a platform behaviour, unverified.** §4 chooses the
   default spelling on the assumption GitHub keys off it. If that assumption is wrong the page
   stays empty and the whole deliverable is invisible. *Trigger: verify on the first labelled
   issue by loading the page; this one is checkable in a minute and should simply be checked.*
6. **Zero of the three existing open issues qualify, so the pipeline is entirely newly-authored.**
   That is the honest finding, and it means this plan's real cost is issue-writing, not labelling.
   *Trigger: none needed — it is stated so the next reader does not re-derive it.*

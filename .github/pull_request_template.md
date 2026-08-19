## What and why

<!-- What changes, and what problem it solves. Link the issue if there is one. -->

## Definition of Done

<!-- These are checked by a HUMAN reviewer, not by CI — nothing in our workflows reads
     the PR body. Strike through with a reason if genuinely N/A; do not tick a box you
     have not actually done. What CI does enforce mechanically: `make check`, the DCO
     sign-off, and a Conventional Commit PR title. -->

- [ ] **Plan linked** — `docs/plans/YYYY-MM-DD-<slug>.md` (required for >~50 LOC or any
      schema/interface change)
- [ ] **Adversarial reviews recorded** — plan review (Phase 1) and code review (Phase 3), with
      findings fixed or explicitly accepted below
- [ ] `make check` green locally
- [ ] Docs updated — godoc, CLI help, OpenAPI, and the mental-model / cookbook page if user-visible
- [ ] CHANGELOG entry under `Unreleased`
- [ ] vhs tape re-recorded if CLI output changed
- [ ] Every bug fix ships with the test that would have caught it
- [ ] All commits signed off (`git commit -s`)

## The non-negotiables

- [ ] `core/` imports nothing above it
- [ ] No new LLM or fine-tuning spend path outside the budget guard (`stats/budget`)
- [ ] No reported delta without its confidence interval; no holdout access before Validate
- [ ] Vocabulary held — no synonyms for Case, Evals, Asset, Pool, Goal, Valuation, Portfolio,
      Destination, Bridge, Holdout
- [ ] No secrets or trace content in logs, errors, spans, fixtures, or telemetry
- [ ] Proto messages passed by pointer; no `encoding/json` on `kno.v1` types outside `api/`

## Proto / schema impact

<!-- Delete if untouched. Otherwise: is it breaking? `buf breaking` result? Migration notes? -->

## New dependencies

<!-- Delete if none. Otherwise, per dependency: what it does, why stdlib or an existing dep can't,
     its license, and its maintenance signal. -->

## Accepted findings

<!-- Review findings you are NOT fixing, and why. Anything that is debt must also land in
     docs/debt.md WITH a repayment trigger — "someday" is not a trigger. -->

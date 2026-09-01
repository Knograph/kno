# ADR-0006: `--json` is a hand-written contract, and this is what it promises

- **Status:** accepted
- **Date:** 2026-08-31
- **Context:** [Stage spend reporting plan](../plans/2026-08-31-stage-spend-reporting.md) §7, and its Phase-2 amendment.

## The problem

Until this record existed, the `--json` contract was stated in exactly one sentence, in a cookbook recipe:

> `--json` emits a stable, hand-written shape aimed at `jq` — not the internal schema, so it won't shift under you when the proto gains a field

That sentence lived at `docs/cookbook/ci-gate.md:35`. When the cookbook moved to [`uknoAI/kno-examples`](https://github.com/uknoAI/kno-examples), the page here became a one-line tombstone and **the sentence left this repository**. It survives at the `v0.1.3` tag and in a copy this repository cannot edit; `grep -rn "stable, hand-written"` over the tree matches nothing else.

So the situation this record addresses is not "the promise is filed in the wrong genre of document". It is that a contract every v0.1 `jq` pipeline is running against is honored by the code and stated nowhere — while the pipeline authors, being downstream, are the last people who would notice.

ADR-0001 does not cover this. It is *"generated proto messages are the domain types"*, and its money content is a **serialization-correctness** rule: `cost_usd_micros` marshals as `"1500000"` under protojson and `1500000` under `encoding/json`, so `depguard` bans `encoding/json` outside a short list of named files. That is about not diverging from the generated OpenAPI spec. It says nothing about whether a CLI key can be renamed.

## The decision

The `--json` output of every `kno` command is a public contract with the following rules.

**1. Hand-written structs, never `protojson` over a `kno.v1` type.** The shapes live in `cli/jsonreport.go`, the single file holding the CLI's `encoding/json` exemption, scoped by filename so it cannot spread. A contract aimed at a `jq` pipeline must not mirror proto field names or presence rules, and must not shift when the schema gains a field.

**2. Keys may be added. Removing or renaming one is a break.** Pre-1.0, a rename or removal requires a CHANGELOG migration note. Post-1.0 it is a covenant requiring a major version, alongside exit codes, the plugin protocol, `kno.yaml`, and the proto (CLAUDE.md, *SemVer with teeth*).

**3. Enum-valued keys carry names, not numbers.** A pipeline branching on `1` breaks the day a value is inserted ahead of it, and a reader should not need the proto to interpret the document ([`docs/debt.md#44`](../debt.md)).

**4. Money appears twice: a display string and an integer.** `*_usd` is a formatted string for eyes; `*_usd_micros` is integer micro-USD for arithmetic. Emitting only the display form pushes CI authors into parsing a currency-formatted float, which is exactly what the engine refuses to do internally — `stats/budget` warns that its formatter is "for error messages only. Never use it for arithmetic."

**5. Absence is meaningful, documented per key, and paired with a positive signal wherever a consumer could read it as a zero.** A key is omitted when its absence is a *fact about the stage*, never merely because a value happened to be zero. Where the omission is load-bearing, the document also states the fact positively.

`guarded` is the first such signal, and the reasoning generalizes. `kno select` runs no budget guard, makes no LLM call, and emits no spend keys at all — because a uniform `"spent_usd": "$0.00"` would be indistinguishable from *"this stage spent money and the meter is missing"*, which is the failure the whole spend-reporting change exists to remove. But bare absence is quieter than it looks: `jq`'s `.spent_usd` on a document without the key returns `null`, identical to an explicit null, so a pipeline degrades rather than breaks. Worse, the repair a consumer reaches for is:

```jq
.spent_usd // 0        # WRONG — reinstates the ambiguity at the consumer
```

which resurrects metered-zero-versus-unmetered on the reading side, where neither these docs nor our goldens can see it. So every document says which it is:

```jq
# Right: sum spend across the stages that actually had a meter.
jq -s 'map(select(.guarded) | .spent_usd_micros) | add' baseline.json value.json

# Right: gate CI on a stage's real cost.
jq -e 'select(.guarded) | .spent_usd_micros < 5000000' value.json
```

`guarded` is a fact about the **stage**, not the run: `true` for `baseline` and `value` on every agent including `fake:`, where the figure legitimately reads `$0.00` beside a non-zero `llm_calls`; `false` for `select`, `export` and `report`. The rule for the future: **a new load-bearing omission ships with its own positive signal**, or it is not a load-bearing omission.

**6. Human and `--json` renderings of one stage are pinned to identical content** by an equivalence test or golden. The two surfaces disagreeing about what a stage cost is worse than either being absent.

**6. A reported statistic is rounded to four decimal places where it is computed, not where it is printed.** A float64 rendered at full precision carries up to seventeen significant digits, and its tail digits are not ours: Go may fuse a multiply-add into a single FMA instruction, which arm64 does and amd64 does not, and anything reaching `math.Log`, `math.Exp` or `math.Sqrt` is architecture-specific outright. Two consequences, and the second is the one that matters.

The mechanical one: a golden cannot hold such a number. `kno eval inspect`'s `separable_effect` hit this first (`cli/evalinspect.go`, "a golden holding 0.19685978500032256 cannot pass on both"), and `kno judge calibrate` hit it again — `kappa_interval.high` recorded as `0.929508759876331` on darwin/arm64 and read `0.9295087598763309` on linux/amd64, one ULP apart, with the human rendering byte-identical. Re-recording only moves the failure to the other runner.

The substantive one: **emitting seventeen digits is a false precision claim.** A percentile bootstrap over thirty records does not carry them, and a document whose stated job is honest reporting must not imply otherwise. Rounding at the point of computation — rather than formatting at each renderer — is what makes the human line, the `--json` document and the golden carry *one* number, so a verdict can always be reproduced from the output that announced it.

Four places, not more: it is finer than any measurement in this product resolves and coarser than the noise. Integer-valued keys (counts, micro-USD, `n_pairs`) are unaffected; they are exact.

## What this record does not do

**It records a decision; it does not enforce one.** Nothing in `make check` reads a markdown file, and a rule that lives only in `docs/adr/` is obeyed exactly as long as the next contributor happens to have read it. The enforcement is three mechanical artifacts:

- the per-stage JSON goldens under `cli/testdata/json/` (rule 2 — a renamed or removed key is a golden diff, reviewed like code);
- `cli/testdata/json/v0.1-shape.json`, the frozen capture of what v0.1.0 emitted unconditionally, checked as a subset of current output (rule 2, with historical force);
- `TestSpendFieldsAreReadInOneFile` (rule 4's single formatter) and `TestGuardedMatchesTheStage` / `TestSelectExportReportEmitNoSpendBlock` (rules 4–5);
- `TestNoJudgeJSONFloatCarriesMoreThanFourPlaces` and `TestEveryReportedStatisticIsRoundedAtTheSource` (rule 6). Note the scope honestly: these walk **`kno judge calibrate`'s** document and statistics as a property, so a key added there later without the treatment fails. Every other command's compliance with rule 6 rests on its own goldens, which catch it only on the architecture CI happens to run — the judge property test is the shape the others should grow, not evidence that they already have it.

The value of this record is that a reviewer rejecting a future PR has something to cite. Its value is **not** that the PR would have been caught without those tests. **Any rule added here later that no test enforces must be labelled as guidance in this document**, rather than left to read as a constraint. A decision record that quietly accumulates unenforced rules is how a contract becomes folklore — which is the failure mode that produced this ADR in the first place.

## Consequences

- The contract now exists in this repository, is linked from README's documentation list, and is versioned with the code that honors it.
- `spent_usd` is frozen as a display string for the life of 0.x. It cannot be summed, which is why rule 4 exists; the ledger carries the deprecation trigger.
- `kno mine` has no `--json` flag and is therefore outside this contract until it grows one.
- The `kno-examples` CI-gate recipe still teaches the pre-`guarded` idiom and still carries the sentence quoted above. Updating it is tracked in [the Debt Ledger](../debt.md) with a repayment trigger; it cannot land in the same commit as this record, because it lives in a different repository.

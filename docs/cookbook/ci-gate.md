# Gate a deploy on Kno in CI

Goal: fail a build when your agent regresses, without failing it for the wrong reasons.

## Exit codes are the contract

```bash
kno baseline --evals cases.jsonl --json > report.json
```

| Code | Meaning | What CI should do |
|---|---|---|
| `0` | Completed | Continue |
| `1` | Failed | Fail the build — something is broken |
| `2` | Stopped at a budget cap | **Not a failure.** The run did what you told it |
| `3` | Validation failed | Fail the build — this is the deploy gate (reserved for `kno validate`) |

The distinction between `1` and `2` matters. A run that stopped at its spending limit did exactly what you configured; treating it as a broken build trains people to ignore the signal.

```bash
kno baseline --evals cases.jsonl --json > report.json
case $? in
  0) echo "baseline complete" ;;
  2) echo "stopped at budget cap; resume or raise it" ; exit 0 ;;
  *) echo "baseline failed" ; exit 1 ;;
esac
```

## Machine-readable output

`--json` emits a stable, hand-written shape aimed at `jq` — not the internal schema, so it won't shift under you when the proto gains a field:

```json
{
  "run_id": "20260821T083017-d2dfc5377255",
  "status": "completed",
  "dev_cases": 44,
  "holdout_cases": 6,
  "attempted": 44,
  "scored": 44,
  "errored": 0,
  "score": 1.0,
  "spent_usd": "$0.00",
  "warnings": ["the holdout has only 6 cases, ..."]
}
```

**Check `warnings` in CI.** They qualify the result, and a scripted consumer that ignores them is reading a number without the reason it might be wrong:

```bash
jq -e '.warnings | length == 0' report.json || echo "::warning::$(jq -r '.warnings[]' report.json)"
```

**Check `errored` too.** A run where a third of the Cases never got an answer isn't a baseline, and Kno marks it — but your gate should notice:

```bash
jq -e '.errored / .attempted < 0.05' report.json || exit 1
```

## Cap spend

```bash
kno baseline --evals cases.jsonl \
  --max-cost-usd 5.00 \
  --cost-per-call-usd 0.002 \
  --yes
```

`--cost-per-call-usd` is required alongside a cost cap, and Kno refuses the run without it. The guard can't refuse what it wasn't told about: a cap checked only at settlement is a cap discovered after the money is gone.

`--yes` skips the confirmation. In `--json` mode Kno **refuses to spend past the threshold without it**, because a machine-readable run has nobody to answer a prompt and proceeding would spend money with no one watching.

## Resume in a scheduled job

Rate limits and timeouts happen. A job that stopped can continue rather than starting over:

```bash
kno baseline --evals cases.jsonl --run-id "nightly-$(date +%F)" --max-calls 5000
kno baseline --evals cases.jsonl --run-id "nightly-$(date +%F)" --resume
```

Resume skips completed Cases and reconstructs prior spend from disk, so the cap holds across both invocations. A resume against changed evals is refused, naming which input changed — continuing would mix results measured over different Case sets into one run.

## What not to gate on yet

`kno baseline` gives you a reference point, not a verdict. The deploy gate proper is `kno validate`, which measures a selected portfolio against the untouched holdout and exits `3` when it doesn't hold up. That stage isn't built yet.

Until then, useful CI checks are: the baseline completed, the error rate is low, and the score hasn't moved unexpectedly against the run you recorded yesterday.

# Recorded fine-tuning fixtures

Each directory is one recorded submit-then-poll-to-terminal exchange. Unlike
`adapters/agent/anthropic`'s fixtures, a real recording here costs dollars,
not fractions of a cent — see the tuner-bridge plan's Step 6(4): live
recording is manual, double-gated (`KNO_MAX_COST_USD` plus
`KNO_ALLOW_TUNING_SPEND=1`), and not re-recorded on a schedule.

The fixtures currently checked in are **hand-authored from Together's
published API documentation**, per this package's own PROVENANCE WARNING —
marked `(verify)` throughout `together.go` — not a confirmed wire trace. They
pin this adapter's own request/response parsing so it is deterministic and
reviewable; they are not evidence that Together's real API answers exactly
this way.

## What a fixture may contain

An **allowlist**, enforced by `TestPollFixturesCarryNothingTheyShouldNot`. A
directory may contain exactly these files and nothing else:

| File | What it is |
|---|---|
| `request.json` | the submit body Kno sent, verbatim |
| `response.json` | the submit response the provider returned, verbatim |
| `status` | the submit HTTP status |
| `poll-NN.json` | one status-response body per successive poll, replayed in order — the state machine `VALIDATING_FILES -> QUEUED -> RUNNING -> DEPLOYING -> SUCCEEDED` (or a `FAILED` branch) is one fixture directory's `poll-01.json` through `poll-NN.json` |
| `training_data.jsonl` | reserved for when the upload step lands (not yet implemented — see `together.go`'s note on `submitRequest.TrainingFile`) |
| `note.txt` | why this fixture exists |

Same reasoning as `adapters/agent/anthropic`'s fixtures: **no headers are
recorded in either direction.** A denylist of header names can only remove
what someone anticipated; there is no field for a credential to land in here
at all.

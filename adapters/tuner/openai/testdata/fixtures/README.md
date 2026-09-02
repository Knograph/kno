# Recorded fine-tuning fixtures

Each directory is one recorded submit-then-poll-to-terminal exchange —
together/testdata/fixtures/README.md's own doc, mirrored here. A real
recording against OpenAI's API costs dollars, not fractions of a cent, so
live recording is manual, double-gated (`KNO_MAX_COST_USD` plus
`KNO_ALLOW_TUNING_SPEND=1`), and not re-recorded on a schedule.

The fixtures checked in are **hand-authored from OpenAI's published
fine-tuning API documentation**, per this package's own PROVENANCE WARNING
— marked `(verify)` throughout `openai.go` — not a confirmed wire trace.
They pin this adapter's own request/response parsing so it is deterministic
and reviewable; they are not evidence that OpenAI's real API answers
exactly this way.

## What a fixture may contain

An **allowlist**, enforced by `TestPollFixturesCarryNothingTheyShouldNot`.
A directory may contain exactly these files and nothing else:

| File | What it is |
|---|---|
| `request.json` | the submit body Kno sent, verbatim |
| `response.json` | the submit response the provider returned, verbatim |
| `status` | the submit HTTP status |
| `poll-NN.json` | one status-response body per successive poll, replayed in order |
| `probe.json` | the GET `/v1/models/{id}` readiness-probe response body, served after the terminal SUCCEEDED poll — see `openai.go`'s `Deploy`/`probeReady` doc |
| `probe-status` | the probe HTTP status: `200` (ready) or `404` (not yet) |
| `training_data.jsonl` | reserved for when the upload step lands (not yet implemented — see `openai.go`'s note on `submitRequest.TrainingFile`) |
| `note.txt` | why this fixture exists |

Same reasoning as `together/testdata/fixtures`: **no headers are recorded
in either direction.** A denylist of header names can only remove what
someone anticipated; there is no field for a credential to land in here at
all.

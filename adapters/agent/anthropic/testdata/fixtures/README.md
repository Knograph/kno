# Recorded Messages API fixtures

Each directory is one recorded exchange. `make record-fixtures` regenerates
them against the live API and spends real money; every other test in this
package replays them and touches no network.

## What a fixture may contain

An **allowlist**, enforced by `TestFixturesCarryNothingTheyShouldNot`. A
directory may contain exactly these files and nothing else:

| File | What it is |
|---|---|
| `case.txt` | the synthetic Case input this exchange was recorded against |
| `request.json` | the body Kno sent, verbatim |
| `response.json` | the body the provider returned, verbatim |
| `status` | the HTTP status |
| `note.txt` | why this fixture exists |

A denylist of header names would be the wrong shape here — it can only remove
what someone anticipated, and `OpenAI-Organization`, `anthropic-beta`,
`request-id`, and `Set-Cookie` are all things gitleaks does not match. So **no
headers are recorded at all**, in either direction. There is no field for a
credential to land in.

The Cases are synthetic and checked in beside the fixtures. A user's evals are
customer data and are never recorded.

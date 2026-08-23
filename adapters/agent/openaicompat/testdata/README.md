# openaicompat fixtures

Recorded provider replies, replayed by `fixture_test.go`. No test in PR CI
touches the network; these are what stands in for it.

## The format is the allowlist

A fixture is a closed set of header lines, a blank line, and the response body:

```
# kno adapter fixture v1
name: answered
source: hand-authored from the documented Chat Completions shape
status: 200
content-type: application/json

{ ... the provider's reply ... }
```

`fixture_test.go` fails on any header name outside `permittedFixtureHeaders`.

This shape was chosen over JSON deliberately. A JSON envelope with a `request`
object invites recording request headers, and "scrub the sensitive ones" is a
denylist wearing another word — it can only remove what someone anticipated, and
`OpenAI-Organization`, `OpenAI-Project`, `anthropic-beta`, and `Set-Cookie` are
all things nobody listed the first time. Here there is **no slot** for a request
header at all, and the response header slot is a closed set of two.

**The request body is never recorded either.** It carries the Case input, which
is customer data (`CLAUDE.md`: "Traces are customer data"), and replay does not
need it — what a request should say is asserted against `httptest` in
`openaicompat_test.go`, where nothing is committed to the repository forever.

## Re-recording

```
KNO_MAX_COST_USD=0.50 make record-fixtures
```

That target refuses to run without a stated ceiling, and `live_test.go` reads
the same variable and authorizes every call against a `budget.Guard` built from
it — so the cap is enforced by code rather than asserted by a comment. See
`docs/debt.md#11`.

Recording uses `testdata/corpus.txt`, a synthetic Case corpus checked into the
repository. Never point it at a user's evals.

## Two classes of fixture, and the `source:` line tells them apart

**`recorded-NN.fixture`** are real replies from a live provider, written by
`TestRecord`, one per line of `corpus.txt`. Re-recording overwrites exactly
these and nothing else.

**Everything else is hand-authored** from the documented response and error
shapes, and says so. They exist because a corpus of ordinary questions cannot
provoke the outcomes whose mishandling produces a wrong *number* rather than a
visible failure: a content-filter refusal, a model-side refusal, a truncation, a
reply with no usage block at all, a 429 carrying a window, a context-length
400. Asking a provider to refuse on demand is not something a recording run can
do, so these are written by hand and marked as such. Re-recording does not touch
them.

`answered.fixture` is hand-authored even though `recorded-00` covers the same
outcome: it is the only fixture with a non-zero `cached_tokens` and a non-null
`system_fingerprint`, and both feed assertions that a live recording happened
not to exercise.

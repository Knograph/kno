# M2 — The first real provider adapter

**Status:** amended after two Phase-1 passes — ready for implementation
**Author:** @devarispbrown
**Date:** 2026-08-21 (two Phase-1 reviews, both BLOCK; amended after each)
**Depends on:** [M1 — Baseline](2026-08-19-m1-baseline.md), [Ring-0 surface](2026-08-19-ring0-surface.md)

Two Phase-1 passes, both **BLOCK**. The first found 11 HIGH / 13 MEDIUM. The second attacked the
amendments and found 9 HIGH / 15 MEDIUM — including that the largest amendment was built on
**arithmetic that is false against `stats/budget/budget.go`**. Finding IDs (H1, M7 from pass one;
H-A, M-j from pass two) are cited inline so any decision traces back to its argument. Pass-one
MEDIUM findings are written `P1-Mn` so they cannot be confused with milestone names or with pass
two's `M-a`…`M-o` — a collision the "M4" mistake below made concrete.

**The correction that matters most.** Pass one's H8 said pure-pessimistic reservation makes the
cost cap unusable — a $5.00 cap stopping at ~6% spent. I accepted it and designed an adaptive
quantile estimator around it. Pass two showed the premise was wrong: `Settle` calls `releaseLocked`
(`budget.go:316,350`), which **decrements** `g.reserved`, so reservations are only ever the
in-flight set. Measured against the real guard, same inputs:

    completed=2455  spent=$4.91  (98.2% of the $5.00 cap)
    plan claimed:   ~152 Cases,  ~$0.30 spent (6% of cap)

Off by 16×. §2.3 is rewritten to the simpler design that premise was hiding.

One finding — **H9**, an unguarded live-spend path in `make record-fixtures` that already existed
on `main` — was not a plan defect and was fixed immediately in PR #23 rather than carried here.

---

## 0. Scope

**M2 is the provider adapters and everything required to call them safely. Nothing else.**

Three things were bundled into this slot at various points; two are moving out:

- **`value`** moves to **M3.** M2 is the first milestone that spends real money. Every milestone
  so far could be wrong for free — a bug in the guard, the retry loop, or settlement cost nothing
  to find. That ends here. Landing a new pipeline stage and a new paid dependency together means
  debugging both at once, and when something overspends you will not know which one did it.
- **The TUI** moves out. The M1 plan (line 44) stated: *"The TUI dashboard. Baseline emits events;
  rendering them is M2."* The first draft dropped it without saying so (P1-M9) — silently cancelling a
  written commitment is the same failure as silent debt carryover.

  The second draft then said it moves to "M4." **"M4" is not a milestone** (H-I): DESIGN.md defines
  v0.1–v0.4, and the token `M4` appears exactly once in this repository — `2026-08-19-m1-baseline.md:218`,
  where it means the *Validate* stage. Re-dating a ledger entry to an undefined token is precisely
  the "someday" failure §0 had just accused the first draft of, committed one paragraph later.

  Corrected: the TUI is DESIGN.md **v0.1** scope and lands after `value`. And `docs/debt.md#29` is
  **not re-dated at all** — it is repaid in M2-0 (§5), because it is an additive `event.proto`
  change and M2-0 already opens `event.proto` and already takes a `buf breaking` review. That is
  verbatim the argument this plan makes for repaying entry 26; pass two caught it being applied to
  26 and withheld from 29.

Phase 1 accepted the split's conclusion while rejecting one of its supporting arguments: the first
draft claimed two adapters are needed to prove the Ring-0 `Agent` interface is provider-neutral.
That is weak — M2 exercises only `Invoke`, and the surface that actually stresses neutrality is
`ContextInjector`/`WithContext`, which M2 never touches. The second adapter is justified on its own
terms (Anthropic's Messages API differs enough that treating it as OpenAI-shaped produces subtly
wrong numbers rather than loud failures), and **M2-4 remains severable if scope needs cutting.**

## 1. Problem statement

`kno baseline` measures an agent. The only agent it can measure is a local fake that echoes the
expected answer. Every number it has ever produced is `1.000`.

To measure a real agent, Kno needs an adapter that:

1. Calls a provider over HTTP and returns a `core.Response`.
2. Reports what the call actually cost, in integer micro-dollars, so the guard settles against
   reality rather than a flag the user guessed at.
3. **Estimates** what a call will cost before making it, because a cap enforced at settlement is a
   cap discovered after the money is gone — the failure that already overshot a $0.10 cap in M1 and
   is currently papered over by `--cost-per-call-usd`, one flat number supplied by hand.
4. Survives what real transports do: rate limits, 5xx, mid-stream deadlines, partial reads,
   connection reuse, context-length refusals, content-filter refusals, truncation, and responses
   whose usage block is missing or wrong.
5. Never puts an API key or a Case's content anywhere it can be read — not a log line, not a trace
   above DEBUG, not a recorded fixture, not an error message, and **not a host the user did not
   name.**

## 2. Design

### 2.1 Two adapters, one transport, and the transport does not retry

**`adapters/agent/openaicompat`** — Chat Completions. Reaches OpenAI, Together, Groq, Fireworks,
OpenRouter, vLLM, llama.cpp, LM Studio, and Ollama's compat endpoint.

**`adapters/agent/anthropic`** — the Messages API. System prompt is top-level, `max_tokens` is
required, the usage block is shaped differently, `stop_reason` semantics differ.

Both sit on `adapters/agent/internal/transport`, which owns connection pooling, timeouts, the
per-host rate limiter, redirect policy, host validation, and redaction.

**The transport does not retry (H3).** The first draft gave retries to both the transport and
`core.invokeWithRetry`, which cannot both be true. `core/baseline.go:644` settles every Response as
exactly one call:

```go
func spendOf(r *knov1.Response) budget.Spend {
	return budget.Spend{
		Calls:         1,
```

A transport that retries internally makes N provider calls inside one reservation and settles them
as one. With `--max-calls 1000` and three transport attempts, a throttled run makes up to 3000
provider calls against a 1000-call cap — a prime-directive-4 breach the plan would have introduced.

So: the transport **classifies** and returns `errs.ErrRateLimited` carrying the parsed
`Retry-After` as a duration. `core` owns retry, reservation, and backoff, and consumes the hint.
Each attempt takes its own reservation, which M1 already implements and tests.

**"Does not retry" is not achieved by not writing retry code (H-G).** `net/http` replays a request
on a reused connection when nothing was written, provided `req.GetBody != nil` — which
`http.NewRequest` sets automatically for a `*bytes.Reader` body. Benign for money (the provider
never saw it), but it makes the claim false and untested. **And it becomes dangerous the moment
`docs/debt.md#20` is repaid:** `isReplayable()` returns true for a POST carrying an
`Idempotency-Key`, so the milestone's own remedy for the dark-spend window switches on stdlib
replay — including `transportReadFromServerError`, where bytes *were* written and the provider may
have processed them. Silently, with no visible code change.

So the claim is made enforceable rather than asserted: set `req.GetBody = nil` explicitly, and
wrap the `RoundTripper` in a counter that the misbehaving-server suite asserts against —
**one round trip observed at the server per reservation**, not per client call.

**And the converse half.** `core` retries *only* `ErrRateLimited`, and `core/errs` has no transient-
transport sentinel. So a stale pooled connection becomes a terminally errored Case labeled
`AGENT_ERROR`; at concurrency 8 against a provider with a 60s idle timeout, any pause in a long run
errors a handful of Cases, and 5% of them trips `DefaultMaxErrorRate` and marks a good baseline
unusable. M2 adds **`errs.ErrTransportTransient`**, retried under the same reservation-per-attempt
discipline, with a test asserting request counts **at the server** against a connection-closing
server.

### 2.2 Agent references

**The grammar in the plan's first draft did not exist (P1-M3).** `proto/kno/v1/common.proto:193-207`
documents:

```
//	openai:gpt-4.1
//	anthropic:claude-sonnet-4-6
//	http://localhost:8000/v1
//	tuned:<job-ref>
//	exec:kno-agent-mybot
```

with schemes `"openai", "anthropic", "http", "tuned", "exec"`. There is no `@base-url` form and no
`openai-compat` scheme. DESIGN.md's Ring-1 list specifies **one** openai-compatible adapter,
"`base_url`-configurable," plus separate `http` and `shell` entries.

**Resolution — settled in the M2-0 proto diff, not discovered later:**

- Keep the documented schemes. `openai:` and `anthropic:` route to their adapters.
- Add an optional `@base-url` suffix to the existing schemes rather than a new scheme:
  `openai:llama-3.3-70b@https://api.groq.com/openai/v1`. One adapter, `base_url`-configurable,
  exactly as DESIGN.md specifies — the first draft's `openai-compat:` scheme created two
  user-visible names for one adapter and is dropped.
- The bare `http://host/v1` form has no model slot and is therefore **not** an agent reference this
  milestone accepts. It is documented as reserved. §6's plain-HTTP rule would otherwise refuse the
  schema's own example, which Phase 1 caught (P1-M3).
- `exec:` (shell agents) and `tuned:` stay out of scope and stay documented as such.

The proto comment is the OpenAPI source of truth, so this is a **proto-comment diff in M2-0** — not
prose written elsewhere.

**The parser gets both a fuzz target and a golden table (P1-M4).** Fuzzing finds panics; the real
hazards here are misparses that succeed: `openai:llama3:8b@https://host/v1` (model containing a
colon), `…@https://user:pass@host/v1` (userinfo — splitting on the last `@` breaks one case,
splitting on the first breaks the other), OpenRouter's `vendor/model:free`. The fuzz target repays
part of `docs/debt.md#4`; a table of ~20 named cases with expected `(scheme, target, base URL)`
triples plus explicit refusals is what makes the parser correct.

### 2.3 Cost: what is reserved, what is settled, and what the cap actually guarantees

**The design here is deliberately boring, and the second Phase-1 pass is why.**

The first draft claimed a pessimistic estimate "can only be too high" and then said local token
counts differ from provider counts routinely. Both cannot be true — a BPE approximation under-counts
on non-English text, code, and long tool schemas.

The second draft accepted pass one's H8 (pure pessimism makes the cap unusable) and built an
adaptive p95 estimator on it. **Pass two showed H8's arithmetic was wrong (H-A)**, and measurement
against the real guard confirms it:

| | Cases completed | Spent | % of a $5.00 cap |
|---|---|---|---|
| Plan's claim | ~152 | ~$0.30 | 6% |
| **Measured** | **2455** | **$4.91** | **98.2%** |

*(pessimistic reservation $0.0328/Case, actual $0.002/Case, concurrency 8)*

The claim assumed reservations accumulate. They do not: `Settle` calls `releaseLocked`
(`budget.go:316,350`), which decrements `g.reserved`, so `reserved` is only ever the in-flight set
and `spent` accumulates **actual** cost. `fitsLocked` therefore denies at
`spent + N×pessimistic + pessimistic > cap` — i.e. at ~94% of real spend, not 6%.

**So there is no adaptive estimator, no running quantile, no per-Case estimator injected into
`BaselineOptions`, and no public `core` API change.** All of that existed only to solve a problem
the guard does not have. §2.11 is deleted. What remains:

**Reservation = pessimistic, always.** `input_tokens × input_price + max_output_tokens ×
output_price`, counted locally. One-sided in the safe direction: it can only cause the guard to
refuse early, and the residual under-count is the BPE input error alone — small and genuinely
bounded, unlike an adaptive reservation whose δ is unbounded once it stops being a ceiling.

**The real failure mode is `N × pessimistic` against the cap, not `cap ÷ pessimistic`** (H-A).
Measured: a 32k reasoning ceiling ($0.256/Case) against `--max-cost-usd 1.00` at concurrency 8
denies the **4th** Case with **$0.00 spent**, `IsFatal` fires, and the run ends `BUDGET_STOPPED`
having done almost nothing. At concurrency 1 the same run proceeds.

**Fix: a startup feasibility check, ~20 lines in `core`.** If `concurrency × pessimistic` exceeds a
fraction of the cap, reduce effective concurrency to `max(1, floor(cap × f / pessimistic))` and say
so; if even one Case does not fit, refuse with a message naming both numbers rather than starting a
run that cannot finish one Case. This is what Alternative F was rejected against a false premise for.

**Settlement, and a real ceiling.** Settle at the provider's reported usage. The reclaimed budget
from dropping adaptation goes into the thing the second draft deferred to "before 1.0": **a
settlement-time ceiling in `stats/budget`.** Today `Settle` adds `actual` to `spent` with no check,
and `remainingLocked` clamps at `max(0, …)` — so a blown cap reports `Remaining: 0` and the
overshoot is *unobservable*. Adding the check makes the bound below enforced rather than merely
stated, and closes §11's largest accepted risk inside the milestone instead of after it.

**The honest contract**, now tight:

> A cost cap of `C` bounds total spend at `C + Σδ` over calls in flight when the cap binds, where
> `δ` is per-call under-estimation — under pessimistic reservation, the input-token count error
> alone. With concurrency `N`, at most `C + N × δ_max`.

**Missing usage block.** Settle at the reservation — which under pessimism is a true ceiling, so it
over-charges the budget, which is the safe direction. Never zero: a zero settlement is what made the
cap unenforceable in M1. Mark the Response, count them, warn above a threshold, and expose the count
in `--json` (M-l) so a CI gate parsing `spent_usd` can tell how much of it was inferred.

Note this rule is only safe *because* reservation is pessimistic (H-B). Under the deleted adaptive
scheme, settling at the reservation fed reservation-derived values back into the sample that
computed the reservation — `r_{n+1} ≈ r_n × m`, geometric drift upward, for any provider that omits
`usage` on a majority of responses. Another reason the simple design wins.

**Pricing** is a table keyed by `(scheme, model)`, versioned and dated, never fetched at runtime — a
pricing endpoint that is down or lying is a spend path with no ceiling. **The price is a vector, not
a pair (P1-M10):** input, cached-read, cache-write, output. Both target providers price cached input
differently; a two-field model settles cache reads at full input price, a systematic overstatement
the user notices as divergence from their invoice.

An unknown model is **not** priced at zero. With a cost cap set, it is a refusal naming the model
and the override flags. With no cap, it runs with a warning that spend is unbounded and unpredicted.

**Naming (M-k).** Three concepts, three words, used identically in code, CLI, events, and docs:
**prediction** (what we think a call costs), **reservation** (what the guard authorizes —
`prediction` unchanged, under this design), **settlement** (what it actually cost). `Reservation.Estimate()`
returns the reservation. `--strict-reservation` is not needed: pessimism is now the only policy.

### 2.4 Confirmation is run-level, capped, and computed where the remaining count is known

**The human currently consents to one Case's price and pays for the whole run (H4).**
`ConfirmFunc` receives a single operation's estimate, and `Guard.askOnce` sets `g.confirmed = true`
once for the life of the run (`budget.go:271`). 10,000 Cases at $0.04, threshold $0.01: the first
Case prompts with **$0.04**, the user types `y`, the run spends **$400**. DESIGN.md:265 says the CLI
*"prints an estimate and confirms before any run over threshold."* What ships is *sample and confirm*.

The second draft's fix was under-specified in three ways, each putting a wrong number in front of a
human at the consent moment (H-D):

1. **It ignored the cap.** With `--max-cost-usd 5.00`, honest maximum exposure is $5.00 + `N × δ_max`,
   not `10,000 × prediction`. Showing $328 when the guard stops at $5 is simply false.
2. **It could not be computed on resume.** §2.4 put it in the CLI *"because it already has
   `counts.Dev`"* — but the completed-Case set is loaded **inside** `core.Baseline`. A run killed at
   9,988/10,000 would prompt for **$328** to finish 12 Cases.
3. Which prediction, given the (now-deleted) varying estimate.

**Corrected.** The confirmation is computed in `core.Baseline`, **after** `CompletedCases` is
loaded, over `DevCases − done`, and is **bounded by the cap**:

> `about to run 9,988 cases, ~$328 at $0.0328/case — capped at $5.00, so at most $5.00`

With no cap set, the range has no upper bound and says so, which is exactly when a human should look
hardest. Putting it in `core` also keeps `cli`/`api`/`tui` as shells over one engine call rather
than three reimplementations of the same estimate — prime directive 3's actual purpose.

### 2.5 Determinism, and what a baseline is reproducible against

**Nothing currently pins provider nondeterminism (H5).** OpenAI Chat Completions defaults to
`temperature: 1`. Two `kno baseline` runs against the same evals and the same model produce
different baselines — and §1 calls this stage "the reference every later delta is measured against."

Worse on resume. `core/baseline.go:368` compares `run.GetAgent().GetRef()`, and `openai:gpt-4.1` is
a moving pointer. A run interrupted Monday and resumed Friday after the alias re-points passes the
check cleanly and blends two models into one `AggregateScore` — precisely what `checkResumable`'s
own godoc says it exists to prevent.

- **`temperature: 0` is routed through the capability matrix, not defaulted blindly (H-F).**
  OpenAI's reasoning models reject any non-default `temperature` with a 400; Anthropic constrains it
  under extended thinking. A blanket default means every Case 400s, every Case errors,
  `ErrorRateExceeded` fires, and the user is told *"too many cases errored for this to be a usable
  baseline"* — a message naming nothing about the cause. Sampling-parameter support is a capability
  (§2.10), and the default applies only where the matrix says it is accepted.
- `seed` where supported. Both recorded on the `Run`.
- **The resolved model is a hard resume refusal; `system_fingerprint` is recorded and warned, never
  refused (H-F).** `response.model` resolves the alias, and comparing it is exactly what
  `checkResumable` was written for. `system_fingerprint` changes whenever the provider's backend
  config changes — routinely, within hours, with no model change — and an equality check turns that
  into `ErrCheckpointStale`, whose fix is *"re-run without --resume"*: a false refusal that costs
  the entire run's price, which is worse than the blending it prevents.
- **Record the *set* of fingerprints observed, not the first.** With concurrency N there is no
  "first response," and during a provider rollout two workers in the *same* run legitimately see
  different fingerprints. A set makes the mixture visible; a first-writer-wins sample hides it.
- `docs/what-the-numbers-mean.md` gains a plain statement: **a baseline is reproducible only to the
  extent the provider is**, and neither `temperature: 0` nor a seed makes a hosted model
  deterministic across a silent version bump.

### 2.6 Rate limiting and retry, bounded by time

Per-host token bucket, seeded from rate-limit response headers where they exist and a conservative
default where they do not. `Retry-After` honored in both forms (delta-seconds and HTTP-date),
clamped so a hostile header cannot hang a run.

**The retry defaults were tuned against a fake and will fail the first real run (P1-M12).**
`DefaultMaxAttempts = 3` and `DefaultRetryBackoff = 500ms` give a total retry window of **1.5
seconds** before a Case is declared errored. A real provider's sustained 429 window is minutes. A
rate-limited account would mark a perfectly good baseline `ErrorRateExceeded` and print *"too many
cases errored for this to be a usable baseline."*

So retry is bounded by **time**, not attempt count, driven by the parsed `Retry-After`, with the
defaults re-tuned and a written rationale — M2 is the first milestone with any evidence about what
they should be.

Debt 23's trigger requires re-running the executor's conformance and concurrency tests against a
real transport. That is a deliverable of M2-6, not a follow-up.

### 2.7 Observability: spans and events are not optional

**Both were missing entirely from the first draft (P1-M11).**

CLAUDE.md: *"every stage, every adapter call, every plugin invocation is a span with run ID
correlation"* and *"Spans never contain conversation content."* M2 introduces the first adapter
call in the project's history. Every adapter call is a span carrying IDs, timings, token counts,
and cost — never Case input, never Response output, never headers. A test asserts the absence.

CLAUDE.md: *"New user-visible state = new event type, never a side channel."* M2 introduces three
kinds of state a user can see and currently cannot: a retry in progress, a rate-limit wait (a run
that appears frozen for 60 seconds), and a settlement overshoot. These are **proto-defined event
payloads and must land in M2-0** under the standing proto-first rule — deferring them past the
first diff is what "reported" would otherwise mean by accident.

### 2.8 Secrets and where a key is allowed to go

**Plain HTTP was the wrong threat model (H11).** Refusing `http://` is necessary and misses both
ways a key actually leaks:

1. **HTTPS to an attacker host.** `openai:m@https://evil.example.com/v1` sends `OPENAI_API_KEY`
   over a perfectly valid TLS connection to an attacker. Confidentiality in transit was never the
   threat; destination is.
2. **Cross-host redirect leaks the Anthropic key from a trusted base URL.** Go's `net/http` strips
   only `Authorization`, `WWW-Authenticate`, and `Cookie` on a cross-domain redirect. Anthropic
   authenticates with **`x-api-key`**, which is not on that list. Any base URL — including a
   compromised or misconfigured trusted one — returning a 302 to an attacker host forwards the key
   verbatim, with no plain-HTTP hop to refuse and nothing the user did wrong.

Rules, all in M2-1:

- A `CheckRedirect` that **refuses cross-host redirects outright.** An API endpoint has no
  legitimate reason to redirect you somewhere else.
- **Per-host key resolution (H-H).** The second draft bound each key env var to a default-host
  allowlist and left the `@base-url` form — the milestone's headline feature — with no credential
  model at all. `openai:llama-3.3-70b@https://api.groq.com/openai/v1` needs `GROQ_API_KEY`, not
  `OPENAI_API_KEY`, and "opt in to send your key to a non-default host" would have shipped a
  cookbook recipe that mails the user's OpenAI key to a third party — threat #1 of this very
  section, reached by following the docs.

  So: `KNO_API_KEY_<NORMALIZED_HOST>` resolves per host, falling back to the scheme's default env
  var **only for that scheme's default host**. The opt-in flag means *"use key K for host H"*, never
  *"send whatever key you have wherever."* **A key bound to host A must never reach host B, flag or
  no flag.**
- Refuse loopback, link-local (`169.254.169.254`), and RFC1918 base URLs unless opted in — SSRF
  from a CI runner with an instance-metadata endpoint is a credential-theft primitive.
- `HTTPS_PROXY` handling is stated explicitly rather than inherited by default.
- Keys from env or the OS keychain only. Never `kno.yaml`, never a flag — a flag lands in shell
  history. This is a deliberate exception to DESIGN.md's "every field mirrored by a flag and a
  `KNO_*` env var" rule, and it is flagged as such rather than silently taken (L2).

`SECURITY.md` describes the actual rule, and gains a **key-revocation runbook** (P1-M1): if a
key reaches a fixture, `make secrets-scan` runs gitleaks over the working tree *and git history*,
so the remedy is revoke-then-rewrite-history, and that needs to be written down before it is needed
rather than during.

### 2.9a `kno purge` and the resumed aggregate collide, and the fix is a schema column

**Two PRs in this milestone would destroy each other's data (H-E).** `store/sqlite.go`'s `outcomes`
table stores the Score **only** as `score_proto BLOB` — there is no numeric score column:

```sql
scored           INTEGER NOT NULL,
response_proto   BLOB,
score_proto      BLOB,
```

- `docs/debt.md#25` (M2-1): purge must **null the blobs**, never delete rows.
- `docs/debt.md#27` (M2-8): the resumed aggregate is fixed by **summing `score_proto`** in the store.

So M2-1 nulls what M2-8 must sum. Concrete: run `kno baseline`, run `kno purge` for privacy, get
interrupted, `--resume`. `CompletedCases` and `SettledSpend` survive — they read dedicated columns,
so entry 25's guarantee holds — but the aggregate now sums over the unpurged subset and reports it
as the run's `AggregateScore`, printed at `cli/render.go:166`, emitted in `--json`, **and carried on
the event stream as `RunFinished.aggregate_score`.** A silently-wrong reference number produced by
two features the same milestone ships: exactly the prime-directive-5 failure entry 27 exists to close.

**Fix, in M2-0/M2-1:** add `score_value REAL` and `score_passed INTEGER` columns to `outcomes`. The
number lives in a column; the blob keeps only what is genuinely trace content (`rationale`,
`judge_model`). Purge nulls `response_proto` and `score_proto`; M2-8 sums the column, which purge
never touches. The test entry 25 explicitly requires — *"asserting `CompletedCases` is unchanged by
a purge"* — is extended to assert `SettledSpend` **and the aggregate** are unchanged too.

### 2.9 Fixtures are customer data, and scrubbing is the wrong shape

The first draft protected keys and forgot traces (P1-M1). CLAUDE.md: *"Traces are customer
data."* Recorded fixtures contain both the Case input sent and the provider response returned,
committed to the repo forever.

Denylist scrubbing can only remove what someone anticipated — `sk-…` matches, but an org header, a
project ID, a session cookie, or a self-hosted key format may not, and the failure is silent and
permanent. So:

- **A real allowlist (M-h).** The second draft argued against denylists and then specified "no
  `Authorization` / `x-api-key` / `Cookie`" — three anticipated names, which is a denylist wearing
  the other word. `OpenAI-Organization`, `OpenAI-Project`, `anthropic-beta`, `x-request-id`, and
  `Set-Cookie` are all unlisted, and gitleaks catches `sk-…` shapes, not org IDs. So: enumerate the
  fields a fixture **may** contain — method, URL, status, and a named response field set — and fail
  the scan on **any** request header outside an explicitly permitted set.
- Record against a **synthetic Case corpus checked into `testdata/`**, never a user's evals.
- The existing gitleaks and golden-file scans gate the result.

### 2.10 Capabilities

Answered from a static per-adapter matrix, not by probing — a probe costs money to learn something
that changes monthly, and a probe failure is indistinguishable from an outage.

`errs.ErrCapabilityUnsupported`'s fix line names `kno doctor`, which does not exist — a fix that
does not fix. **Two more become live in M2 (L3):** `ErrBudgetExceeded` says *"raise max_cost_usd in
kno.yaml"* and `ErrRateLimited` says *"lower concurrency in kno.yaml"*, and there is no `kno.yaml`
and no loader. `ErrRateLimited` starts firing at real users in this milestone. All three fix lines
are corrected in M2-5 whether or not `doctor` lands; `doctor` is not in DESIGN.md's CLI surface and
DESIGN already gives capability printing a home in `kno plugins list`, so the conflict is flagged
rather than resolved by fiat (L2).

Also in M2-5: `cli/render.go:41-43` returns `ErrCapabilityUnsupported` for an unknown agent scheme,
but `errs.go:80` documents `ErrInvalidInput` as the sentinel for *"an unknown adapter name."* M2
rewrites that function; the sentinel is fixed there (L4).

### 2.11 *(deleted)*

The second draft added an injected `func(*Case) budget.Estimate` on `BaselineOptions` —
a public Go API change — to carry a per-Case adaptive estimate. §2.3 deletes the adaptation,
so this dissolves with it (H-A, H-C). The flat `EstCostPerCallUSDMicros` is replaced by an
adapter-supplied pessimistic prediction computed at the adapter, and `core` keeps the scalar
shape it already has. No `core` API break, and `core/baseline.go` does not grow a streaming
quantile estimator on top of its current 657 lines (M-j).

## 3. PR decomposition

M2-2 in the second draft carried five workstreams across five packages (M-j). §2.3's deletion of the
adaptive estimator removes two of them; the rest are split out below.

| PR | Scope | Depends on | Repays |
|---|---|---|---|
| M2-0 | Proto: agent-ref grammar, price vector, refusal/truncation/usage-estimated flags, resolved-model + fingerprint-set fields, retry/rate-limit/overshoot **events**, `RunResumed` payload, `BaselineDetail` submessage | — | 26, 29 |
| M2-1 | `store`: `score_value`/`score_passed` columns; `kno purge` nulling blobs only | M2-0 | 25 |
| M2-2 | `internal/transport`: rate limiter, `GetBody` pinning + round-trip counter, redirect refusal, per-host key resolution, SSRF refusal, timeouts, redaction, OTel spans, `goleak.VerifyTestMain` | M2-0 | — |
| M2-3 | Agent-ref parser: fuzz target + golden table | M2-0 | 4 (part) |
| M2-4 | Pricing table + pessimistic prediction; concurrency-vs-cap feasibility check; **settlement-time ceiling in `stats/budget`**; overshoot recording | M2-0 | — |
| M2-5 | `core`: run-level confirmation after `CompletedCases`; time-bounded retry (`MaxAttempts`/`RetryBackoff` semantics change); `ErrTransportTransient`; resume check on resolved model; per-attempt spend on `Outcome` | M2-4 | — |
| M2-6 | `openaicompat` adapter, fixtures, `goleak.VerifyTestMain`, executor conformance against a real transport | M2-2, M2-4 | 18, 23, 11 |
| M2-7 | `anthropic` adapter, fixtures, `goleak.VerifyTestMain` | M2-6 | 18 |
| M2-8 | Resumed `AggregateScore` summed from `score_value` | M2-1 | 27 |
| M2-9 | Event **emission** for the M2-0 payloads; `core/baseline.go` split (657 lines against a ~400 soft cap, and M2 adds to it) | M2-5 | — |
| M2-10 | CLI wiring, flag surface (§9), capability matrix, corrected fix lines, docs, cookbook recipe, vhs tape | M2-6 | — |

**Ordering notes.** `kno purge` moved from last to **first after proto** (P1-M5): merges are
incremental, so `main` must not carry real trace collection with no purge. It now also lands the
`score_value` column that M2-8 depends on and purge must not destroy (§2.9a).

**M2-9 exists because the second draft had no row for event emission** (M-b, M-m). `event.proto`
already defines `SpendRecorded` and `StageProgress` that **nothing emits**; adding three more
schema-only payloads would make five. Emission is a deliverable, not an implication.

## 4. Alternatives considered

**A. Ship `value` first, against the fake (rejected).** `fake.Invoke` returns `c.GetExpected()`, so
every Case scores 1.000 and no delta is meaningful. It would build the stage without ever
exercising what the stage measures.

**B. One adapter only, defer Anthropic (viable; M2-5 is severable).** Cheaper. Not chosen because
the Messages API differs enough that an OpenAI-shaped assumption produces subtly wrong numbers.
Phase 1 correctly rejected the first draft's *stated* reason (proving interface neutrality) — M2
exercises only `Invoke`, so two adapters prove less about neutrality than claimed. **This remains
the correct thing to cut if scope needs cutting.**

**C. Exact per-provider tokenizers (rejected).** Accurate, and a large dependency plus per-model
vocabulary files that go stale silently. The estimate's job is to bound the reservation, not to
predict the invoice — and §2.3's adaptive quantile learns the real distribution from settlement
anyway, which a static tokenizer cannot.

**D. Fetch pricing at runtime (rejected).** Always current, and a network dependency on the spend
path whose failure mode is either "refuse to run" or "run with no cap." Neither is acceptable.

**E. Probe capabilities at startup (rejected).** Costs money, goes stale, and a probe failure looks
like an outage.

**F. Pure-pessimistic reservation with no adaptation — REJECTED IN DRAFT 2, NOW ADOPTED.** Draft 2
rejected it on H8's claim that it stops runs at ~6% of their cap. Measured against the real guard
that is 98.2% (§2.3), because reservations release on settle. This is now the design; the adaptive
alternative is rejected in its place — it is more code, needs state `core` has nowhere to put, is
lost on resume, and feeds settled values back into the sample that produces them (H-B).

**G. Adaptive p95 reservation (rejected — was draft 2's design).** Beyond H-B's feedback loop: a
p95 over 20 samples is an extreme order statistic with enormous variance and is undefined below
~20, the first N reservations under concurrency N are all drawn from an empty sample, and
`store.SettledSpend` returns an aggregate with no distribution — so a resumed run re-enters the
pessimistic phase with a nearly-exhausted cap and can stop having completed **zero** Cases while
printing *"Run `kno baseline --resume` to continue"* forever (H-C).

## 5. Proto / schema impact

All additive. Lands **first**, in its own diff, `buf breaking` passing, before dependent
workstreams start.

- `Response`: cached-token count (`prompt_tokens` and `completion_tokens` already exist at
  `case.proto:133,136` — the first draft double-counted them as new, L1), `usage_estimated`,
  refusal flag, truncation/stop-reason.
- `Run`: resolved model, `system_fingerprint`, temperature/seed, `max_output_tokens`, pricing-table
  provenance so a report can say which table produced its numbers.
- `AgentRef`: the `@base-url` grammar, in the proto comment that is the OpenAPI source of truth.
- New `Event` payloads: retry attempt, rate-limit wait, settlement overshoot.
- Pricing as a vector: input, cached-read, cache-write, output.
- **`RunResumed` payload (or a remaining-count field on the resumed `RunStarted`)** — repays
  `docs/debt.md#29` here rather than re-dating it (H-I). `core/baseline.go` currently calls
  `emitRunStarted(ctx, agg, opts.DevCases)` unconditionally — the full count on resume — so the
  emitter-side change is two lines, in M2-9.
- **New event payloads carry regime-neutral fields (M-m):** `attempt_ordinal`, `retry_after_ms`,
  `deadline_remaining_ms`. Freezing `attempt`/`max_attempts` in M2-0 and then replacing the
  attempt-count regime with a time bound in M2-5 would leave the generated OpenAPI documenting a
  field that means nothing.
- **`usage_estimated` is reconciled with `Capabilities.token_counts`** (`common.proto:181-186`),
  which already expresses "estimated rather than measured" at adapter granularity (M-l). One
  representation, not two.
- **`outcomes.score_value` / `score_passed` columns** — a store migration, not proto, but it lands
  with M2-0/M2-1 because §2.9a's collision depends on it.
- **`BaselineDetail` submessage — repays `docs/debt.md#26` here, not in M3.** The first draft
  re-dated it as "no benefit," which Phase 1 showed was simply wrong (P1-M7): §5 already opens the
  `Run` message for pricing provenance, the ledger already prescribes the additive submessage
  fix, and re-dating buys a second `Run`-touching proto churn plus another `buf breaking` review.

## 6. Edge cases

| Case | Mitigation |
|---|---|
| Provider returns no usage block | Settle at the **reservation**, never zero; mark `usage_estimated`; count and warn above a threshold |
| Reported usage exceeds the reservation | Settle at actual, record the signed overshoot, emit an event when it recurs |
| **Content-filter refusal** | Scored **and flagged.** Not error-only, not score-only — see below |
| **`finish_reason: length` / `stop_reason: max_tokens`** | A distinct outcome, recorded. Kno's own `max_output_tokens` must not silently depress the baseline |
| `Retry-After` as an HTTP-date | Parse both forms; clamp so a hostile header cannot hang a run |
| Sustained 429 | Retry bounded by **time**, not attempt count; then the Case errors |
| Context length exceeded | Permanent 4xx, not retried; actionable message naming the limit |
| Mid-stream deadline / partial read | Response incomplete: error the Case, never score a truncated answer |
| Connection reuse across a cancelled request | Exercised by the conformance suite, not assumed |
| Cross-host redirect | Refused outright — `x-api-key` is not stripped by `net/http` |
| Base URL over plain HTTP, **with a credential attached** | Refused unless opted in |
| Base URL over plain HTTP, **no credential** | Allowed — `http://localhost:8000/v1` is the documented local-vLLM/Ollama case (M-i) |
| Base URL at loopback / RFC1918, no credential | Allowed. §2.8's own principle is that destination matters *because a key travels*; no key, no question |
| Base URL at link-local (`169.254.169.254`) | Refused unconditionally — instance metadata is SSRF regardless of credentials |
| Key resolved for host A, request to host B | Refused. No flag overrides this |
| Unknown model with a cost cap | Refuse, naming the model and the override flags |
| Unknown model without a cap | Run, warning that spend is unbounded and unpredicted |
| Model alias re-points mid-run or between resume | Resolved model recorded and compared on resume |
| Two runs sharing one provider's rate limit | Limiter is per-host per-process; cross-process coordination is out of scope and stated so |

**Content-filter refusals get their own row because the first draft got this wrong (H6).** It said
a refusal is "a scored Case with a low score, not an error." Taken literally with `exactmatch`, an
account whose safety settings refuse every Case produces 100% scored Cases, `AggregateScore =
0.000`, and **passes** the 5% error-rate threshold because zero Cases errored. `warningsFor` emits
nothing. The user gets a confident-looking reference number from a run in which the agent was never
measured. Second-order: refusals are provider policy and nondeterministic, so a refusal present in
the baseline and absent in a later Value run reads as improvement attributable to the injected
Asset. So: score it **and** flag it, count refusals on the `Run`, warn above a threshold, and carry
the flag forward so every later delta can exclude or mark refused Cases.

**Truncation (H7)** was missing entirely — the first draft's only truncation row covered transport
failure. `finish_reason: length` is a well-formed 200 with valid JSON and a truncated answer, which
would be scored as a wrong answer, meaning **Kno's own choice of `max_output_tokens` depresses the
baseline invisibly.** The coupling is the trap: `max_output_tokens` is also §2.3's estimate ceiling,
so raising it to stop truncating inflates every reservation and lowering it truncates more. Record
it on the `Run` and include it in `InputFingerprint`, so a resume with a different ceiling is
refused.

## 7. Test plan

- **Fixtures for everything**, recorded via `make record-fixtures` (now spend-guarded, PR #23),
  allowlist-scrubbed per §2.9, against a synthetic corpus. No adapter test in PR CI touches the
  network.
- **A deliberately misbehaving server** in `testdata/`: truncates mid-response, returns 429 in both
  `Retry-After` forms, returns usage disagreeing with the body, closes after headers, returns 200
  with malformed JSON, redirects cross-host, returns `finish_reason: length`, returns a refusal.
  **This is also the coverage strategy (P1-M13):** retry, timeout, partial-read, and connection-reuse
  paths are exactly what does not get covered by accident, and the first real adapter sets the
  package's coverage floor where `fake` currently records 0.0.
- **The estimator property test is rewritten (H2).** The first draft asserted "the reservation is
  never below the settled cost," which §6 itself contradicts and which a generator producing both
  sides proves by construction. Instead: (a) a bounded-overshoot property where "actual" is drawn
  from **recorded provider usage blocks**, not the estimator's own model; (b) a cap test asserting
  `total_spend ≤ cap + concurrency × δ_max` with `δ_max` a declared constant — well-defined now that
  reservation is uniformly pessimistic, which it was not under draft 2's adaptation.
- **A feasibility-check test** (§2.3): a 32k-ceiling model against a $1.00 cap at concurrency 8 must
  either reduce concurrency and complete, or refuse naming both numbers — never start and stop after
  three Cases with $0.00 spent, which is the measured behavior today.
- **A guard throughput test** pinning the measurement in §2.3, so the arithmetic that produced two
  wrong designs cannot be asserted from reading again.
- **Budget-cap integration test** against the misbehaving server: under concurrency, and across a
  kill/resume cycle.
- **A purge test** (M-e): `docs/debt.md#25` explicitly requires one asserting `CompletedCases` is
  unchanged by a purge, and the second draft's ten-item test plan had none. Extended per §2.9a to
  assert `SettledSpend` and the resumed aggregate are unchanged too.
- **A round-trip counter test** (H-G): requests counted **at the server**, asserting exactly one per
  reservation, including against a server that closes idle connections.
- **A retry-accounting test** (M-a): `store/store.go:124` documents `Outcome.Spend` as *"including
  any failed attempts preceding a successful retry"*, and the code sets `spendOf(...)` = `Calls: 1`.
  The guard settles each attempt; the store persists one. So `Guard.Restore(SettledSpend)` on resume
  under-restores the call cap by (attempts − 1) per retried Case. Kill a run mid-retry, resume, and
  assert `SettledSpend.Calls == Guard.Spent().Calls`.
- **Executor conformance re-run against the real transport** — debt 23's explicit requirement.
- **`goleak.VerifyTestMain` in each adapter package** — debt 18, in M2-6 and M2-7, plus `internal/transport` in M2-2 (L-a). Note the first
  draft assigned it to M2-1, which is `internal/transport`, not an adapter package (P1-M6).
- **Fuzz target + golden table on the agent-ref parser** (§2.2).
- **A span test** asserting no Case or Response content, and no headers, reach any span (§2.7).
- **A redaction test** over real error paths — with a real transport it stops being theoretical.
- **Live tests** (`KNO_LIVE_TESTS=1`) opt-in, never in PR CI, nightly with `KNO_MAX_COST_USD`
  actually read by code — debt 11, interlock deleted in M2-2.

## 8. Rollback story

Each adapter is a package behind a scheme prefix; removing it from the resolver removes it from
reach without touching `core`. The transport is internal with no other consumer. Pricing is data.

Three things do not roll back cleanly and therefore land first and separately:

- the additive proto fields;
- the `outcomes` schema columns (§2.9a);
- **the retry-bound semantics change** (M-b): `DefaultMaxAttempts`, `DefaultRetryBackoff`, and
  `BaselineOptions.MaxAttempts` are public `core` API, and §2.6 changes their meaning from
  attempt-count to time-budget. The second draft's §8 missed this.

The `BaselineOptions` estimator signature is no longer among them — §2.3's simplification removed
the API change entirely.

## 9. Docs impact

Started from this plan, not after the code.

- **README** — status table; quickstart gains a real-provider path alongside `fake:`.
- **`docs/cookbook/`** — a new recipe for pointing Kno at your own provider, with key handling and
  the cost cap. `ci-gate.md` gains the real-spend caveat.
- **`docs/what-the-numbers-mean.md`** — what a cost number claims (reported usage at a dated price,
  not an invoice); what a refusal or a truncation does to a score; **and that a baseline is
  reproducible only to the extent the provider is** (§2.5).
- **`docs/mental-model.md`** — where cost numbers come from, and the honest cap bound from §2.3.
- **`CONTRIBUTING.md`** — `goleak.VerifyTestMain` is required in adapter packages. Debt 18's text
  requires this explicitly (*"and CONTRIBUTING must say so"*) and the first draft omitted it (P1-M6).
- **`SECURITY.md`** — the actual host/redirect/key rules, plus the key-revocation runbook (§2.8).
- **The flag surface** (M-n), enumerated here because M2-10 silently owned it: `--base-url` (or the
  `@` form), `--allow-non-default-host`, `--allow-insecure-base-url`, `--max-output-tokens`,
  `--temperature`, `--seed`, plus per-host key selection. DESIGN.md's mirroring rule means each also
  needs a `KNO_*` env var; keys are the stated exception (§11a). `make docs` snapshot-tests CLI help
  and the vhs tape is re-recorded when output changes — both budgeted in M2-10.
- **godoc, CLI help, CHANGELOG** — standing.

## 10. Debt ledger

| # | Trigger | Disposition |
|---|---|---|
| 4 | "when the agent-ref parser lands" | Partly repaid, M2-3 |
| 11 | Re-dated 2026-08-21 to M2 | Repaid **M2-6**. The entry says *"that PR must wire the env cap into the live-test path and delete the grep interlock"* — the live-test path is the adapter tests, so a bare "M2-4 wires the read" would not satisfy it (M-g). Its mitigation hole was fixed separately in PR #23 |
| 18 | "when the first adapter lands" | Repaid M2-6, M2-7, **plus M2-2** (`internal/transport` is where the goroutines actually live — idle connections, rate-limiter timers, retry timers — so the entry's letter and its purpose both get served, L-a), **plus the CONTRIBUTING.md sentence the entry requires** |
| 20 | "M2, with the first real adapter" | Investigated in M2-6. OpenAI supports `Idempotency-Key` today, so the answer is known-positive for one provider — but persisting a key *before* the call is a second durable write per Case on the hot path, and it switches on stdlib replay (§2.1, H-G). **M2 investigates and records the finding; implementation is M3** (M-o), mirrored to the entry |
| 21 | "when the second stage lands (P1-M2)" | `value` moved to M3, so the trigger's condition has not occurred. Re-dated to M3 with that written reason |
| 23 | "M2, with the first real adapter" | Repaid **M2-6** (the second draft said M2-3 in §2.6 and M2-4 in §10 — renumbering damage, M-d) |
| 25 | "before `kno purge` is implemented (P1-M2)" | Repaid M2-1, **including the test the entry requires** and which the second draft omitted (M-e), extended per §2.9a |
| 26 | "before the first non-Case-executing stage writes a `Run`" | **Partly repaid, M2-0** — schema only. The writer (`closeRun`) and reader (`cli/jsonreport.go`) migration lands in M2-9; until both, the flat counters and the submessage coexist. Labelled "partly" rather than "repaid" because a `DEBT()` comment pointing at a row marked repaid is worse than a live marker (M-f). Trigger retained until M2-9 |
| 27 | "before any stage reads `AggregateScore` — M2 at the latest" | Repaid M2-8, not re-dated |
| 29 | "before the TUI renders progress" | **Repaid M2-0**, not re-dated. Additive `event.proto` change in a diff already opening `event.proto` — the same argument this plan makes for 26 (H-I) |
| *new* | Cross-process rate-limit coordination | Two concurrent `kno` runs against one provider can collectively exceed its limits. Trigger: when `kno serve` lands (v0.3), which makes concurrent runs the normal case |
| *new* | Streaming unimplemented | `Capabilities.stream` exists in the schema; M2 makes non-streaming requests only. Trigger: when any stage needs incremental output. That PR adds an SSE frame parser **and its fuzz target** |
| *new* | Approximate token counting | Bounded by pessimistic reservation and reconciled at settlement, but a systematically mis-counting model skews reservations. Trigger: when a provider's reported input count diverges from ours by more than a stated threshold in nightly live tests |

**On 27.** The second draft's argument was that no *stage* reads `AggregateScore`. True and
irrelevant: `cli/render.go:166` prints it every run, `cli/jsonreport.go:65` puts it in `--json`
(what a CI gate parses), and `RunFinished.aggregate_score` puts it on the event stream. Re-dating a
trigger that already says "M2 at the latest," in M2, would be the ledger rule failing its first test.

**Count of re-dates: one** (entry 21, on a condition that demonstrably has not occurred). The first
draft proposed three and should have said four; the second proposed one to a milestone that does
not exist.

## 11. Accepted risks

Every entry here has a **row in §10** with a trigger and an owner. The second draft claimed five
risks were "mirrored to the ledger" and added one row (M-c) — a plan contradicting itself in the two
sections that exist to prevent exactly that drift.

1. **The cap is a soft bound**, `C + N × δ_max` (§2.3) — **narrowed, not accepted.** The
   settlement-time ceiling moved into M2-4 with the budget reclaimed from deleting the adaptive
   estimator, so the bound is enforced rather than stated, and δ is the input-count error alone.
2. **The dark-spend window** (`docs/debt.md#20`) is only partly closable. M2 investigates; M3
   implements (§10).
3. **Cross-process rate-limit coordination** is out of scope — §10 row, trigger `kno serve` (v0.3).
4. **Streaming is not implemented** — §10 row, trigger: when a stage needs incremental output.
5. **Approximate token counting** — §10 row, trigger: divergence threshold in nightly live tests.

## 11a. DESIGN.md conflicts, flagged rather than resolved

Prime directive 1 requires stopping and flagging rather than silently picking. Four:

- **OTel spans in M2** vs DESIGN.md:399 placing "OTel export" at v0.3 (L-e). CLAUDE.md says tracing
  is built in with local default off, so the two reconcile — but the divergence is stated, not
  assumed.
- **`kno doctor` and `kno purge`** are both absent from DESIGN.md's CLI surface (L-d). `purge` is
  required by CLAUDE.md; `doctor` is named by an existing error's fix line. DESIGN already gives
  capability printing a home in `kno plugins list`.
- **Keys never mirrored to a flag**, against DESIGN.md's "every field mirrored by a flag and a
  `KNO_*` env var" rule (L-2). Deliberate: a flag lands in shell history.
- **`temperature: 0` as a default** is a measurement decision DESIGN.md does not discuss, and it is
  not universally accepted (§2.5).

## 12. What both reviews changed, and what was not adopted

**Nothing was rejected.** All 20 HIGH and 28 MEDIUM findings across the two passes are either fixed
above or converted into a §10 ledger row with a trigger.

The plan got **smaller** under review, which is the outcome worth noting. Draft 2 answered pass one
by adding an adaptive estimator, a public `core` API change, running quantile state, and a resume
seeding story. Pass two showed the premise was arithmetically false, and all of it came out. What
replaced it is ~20 lines of feasibility check plus a settlement-time ceiling that closes the
milestone's largest accepted risk inside the milestone rather than deferring it to 1.0.

Three lessons recorded here because they are process defects, not code defects:

1. **I accepted H8's arithmetic without measuring it**, then built the largest section of the plan
   on it. The guard's behavior was three functions away and is now pinned by a test (§7). A review
   finding is a hypothesis, not a fact, and this one was wrong by 16×.
2. **I applied my own reasoning inconsistently**: repay debt 26 in M2-0 because the proto diff is
   already open, re-date debt 29 which is the same shape in the same file. Pass two caught it.
3. **I re-dated a ledger entry to "M4," a milestone that does not exist**, in the same section where
   I accused draft 1 of exactly that. Both draft 1 and draft 2 failed the ledger rule; the current
   draft re-dates one entry, on a condition that demonstrably has not occurred.

Two corrections to the reviews themselves, for the record:

- **Pass one's H8 was wrong** (measured, §2.3). Its H4, H6, H7, and H11 were right and material.
- **Pass two's H-A, H-E, and H-I were verified independently** before adoption — the guard
  throughput by measurement, the purge/aggregate collision against `store/sqlite.go`'s schema, and
  "M4" by grep across the repository. All three held.

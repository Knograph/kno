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
wrong numbers rather than loud failures), and **M2-8 — the Anthropic adapter — remains severable if scope needs cutting** (M-1: two
earlier drafts named two different PRs here, both of which now point at the money machinery).

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

| | Spent of a $5.00 cap | |
|---|---|---|
| Pass one's claim | ~$0.30 | 6% |
| Draft 3's measurement | $4.91 | 98.2% |
| **Correct** | **$4.77** | **95.3%** |

*(pessimistic $0.0328/Case, actual $0.002/Case, concurrency 8)*

**Draft 3's number was also wrong (pass three).** It settled in the same instant it reserved, so
`N` reservations never overlapped — a provider call holds one open for its whole duration. Under a
real hold the figure is 95.3% at concurrency 8 and **80.5% at concurrency 32**.

Three arithmetic claims in this lineage have now failed measurement, the third made while
correcting the second. So the number is no longer prose: `stats/budget/throughput_test.go`
asserts it, **as a bound rather than a count** — the exact figure varies with scheduling by more
than a point, and a test pinned to `2455` would be flaky on arrival (M-3).

The forfeiture is `N × pessimistic` of headroom held at the boundary, and it is **not negligible at
high concurrency**. `docs/mental-model.md` states it alongside the overshoot bound (H-5).

Pass one assumed reservations accumulate. They do not: `Settle` calls `releaseLocked`
(`budget.go:322`, declared `:350`), which decrements `g.reserved`, so `reserved` is only ever the
in-flight set and `spent` accumulates **actual** cost. `fitsLocked` denies when
`spent + (N−1)×pessimistic + pessimistic > cap`, i.e. `spent + N×pessimistic > cap` (L-7).

**So there is no adaptive estimator, no running quantile, and no injected
`func(*Case) budget.Estimate` on `BaselineOptions`.** All of that existed only to solve a problem
the guard does not have.

**There is still one `core` API addition, and draft 3 denied it (H-1).** A pessimistic reservation
is `input_tokens × input_price + …`, and `input_tokens` is a property of **the Case**. `core.Agent`
is `Invoke(ctx, *Case) (*Response, error)` and nothing else (`core/ring0.go:18-25`), and
`core/baseline.go:397` builds the estimate from a run-scalar *before* `Invoke`. There is no path by
which an adapter supplies a per-Case number without one. Draft 3 claimed "an adapter-supplied
prediction… no `core` API break," which is not reachable from the code — the same failure class as
draft 2, one section over.

Admitted rather than hidden: **`core.Estimator`, an optional Ring-0 interface** alongside `Capable`
and `ContextInjector`.

```go
// Estimator reports what a Case will cost before the call is made.
type Estimator interface {
    Estimate(ctx context.Context, c *Case) (budget.Estimate, error)
}
```

`core` type-asserts it; an adapter that does not implement it falls back to
`EstCostPerCallUSDMicros`, so `fake` and every existing caller are unaffected. It is listed in §5,
in §8 as not rolling back cleanly, and it has its own PR row.

**Reservation = pessimistic, always.** `input_tokens × input_price + max_output_tokens ×
output_price`, counted locally. One-sided in the safe direction: it can only cause the guard to
refuse early, and the residual under-count is the BPE input error alone — small and genuinely
bounded, unlike an adaptive reservation whose δ is unbounded once it stops being a ceiling.

**The real failure mode is `N × pessimistic` against the cap, not `cap ÷ pessimistic`** (H-A).
Measured: a 32k reasoning ceiling ($0.256/Case) against `--max-cost-usd 1.00` at concurrency 8
denies the **4th** Case with **$0.00 spent**, `IsFatal` fires, and the run ends `BUDGET_STOPPED`
having done almost nothing. At concurrency 1 the same run proceeds.

**Fix: a feasibility check in `core`, specified rather than gestured at (H-5).**

- **`f = 0.25`.** Effective concurrency is `max(1, floor(cap × 0.25 / pessimistic))`, so a run
  never holds more than a quarter of its cap as un-spendable headroom. Chosen because the forfeiture
  is `N × pessimistic` and 25% is the point where the measured throughput above stops being
  comfortable (95.3% at N=8, 80.5% at N=32). Written down so it is a decision, not a constant
  someone later has to reverse-engineer.
- **It runs after `Guard.Restore`, against `cap − spent`, not against `cap`.** Draft 3 called it a
  "startup" check and left placement unsaid — which is *verbatim* the defect it had just diagnosed
  for the confirmation prompt in §2.4 and fixed there. Applying it in one section and not the other,
  in the same edit, is the process defect §12 records.
- **A refusal exits 2, not 1.** Today an exhausted resume starts, is denied, `IsFatal` fires, and
  the run ends `BUDGET_STOPPED` — exit `2`, "resumable, nothing wrong with the data". A startup
  refusal returning `ErrInvalidInput` would report the identical situation as exit `1`, "broken
  build", against a covenant `errs.go:11-13` describes as the thing CI gates branch on. The refusal
  uses **`ErrBudgetExceeded`**.
- **It cannot see its own input error.** If `pessimistic` is wrong — unknown model, stale price
  table — reducing concurrency does nothing. That is why an unknown model with a cap is a refusal
  rather than a guess.

**Settlement: observability, not enforcement (H-4).** Draft 3 called a settlement-time check a
"ceiling" that would make the cap bound *enforced*. It cannot. `fitsLocked` already denies every
subsequent `Authorize` once `spent ≥ cap`, so enforcement after an overshoot is already total —
and the money is spent regardless. What a check adds is exactly one thing: **visibility**, which
`remainingLocked`'s `max(0, …)` at `budget.go:375` currently destroys by clamping a blown cap to
`Remaining: 0`.

And making `Settle` fail would be worse than the problem. It is called at `core/baseline.go:486`
and `:493` inside `defer res.Release()`: an error return there is ignored (`errcheck` is zero-
tolerance, so it would not compile clean), honoring it turns a successful, paid, *scored* call into
an errored Case — losing paid work and inflating `ErrorRateExceeded` — and clamping `spent` makes
`Guard.Spent()`, documented at `budget.go:416` as *"the number a report shows"*, under-report the
real invoice to `--json`.

So: **`Settle` stays void.** Add `Guard.Overshoot() int64` and the overshoot event. The bound in
§11.1 is **accepted, not narrowed**, and it carries a §10 row like every other accepted risk.

Note also that `Restore` (`budget.go:402-409`) is additive with no check and runs at
`core/baseline.go:212`, so a resume with a *lowered* `--max-cost-usd` enters over-cap without ever
reaching `Settle`. That is a supported stop, and it is stated rather than discovered.

**The honest contract**, now tight:

> A cost cap of `C` bounds total spend at `C + Σδ` over calls in flight when the cap binds, where
> `δ` is per-call under-estimation — under pessimistic reservation, the input-token count error
> alone. With concurrency `N`, at most `C + N × δ_max`.

**Missing usage block.** Settle at the reservation — under pessimism a true ceiling, so it
over-charges the budget, which is the safe direction. Never zero: a zero settlement is what made the
cap unenforceable in M1.

**But `core` must not be the one to do it (H-2).** `core/baseline.go:490-493` carries the comment
*"The same computation the sink persists. Two independent derivations of one Case's cost could
drift, and the persisted one is what `Guard.Restore` reads on resume."* The reservation is held by
`core`'s `res`, invisible to `spendOf` and absent from the sink entirely. Charging the guard the
reservation while the store persists `spendOf(resp)` = 0 means `SettledSpend` under-restores by the
full inferred cost of every usage-less Case — resume double-spend, the exact amnesia that comment
closes.

So **the adapter stamps its own estimate onto `Response.cost_usd_micros`** and sets
`usage_estimated`. `spendOf` stays the single derivation, guard and store agree, and resume is
correct. Mark the Response, count them, warn above a threshold, and expose the count
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

**Naming (M-k), corrected in M2-2.** Draft 4 proposed **prediction / reservation / settlement**.
That was itself the drift M-k warned about: `stats/budget` already names these `Estimate`,
`Reservation`, and `Spend`, so "prediction" was a fourth word for something `Estimate` already
covers — and under pessimistic-only reservation, "prediction" and "reservation" are the *same
value*, so keeping both guarantees a synonym.

The code's words win, because they were already right: **`Estimate`** (what a call is predicted to
cost), **`Reservation`** (the authorization holding it), **`Spend`** (what it actually cost). Three
concepts, three words, no renames to `stats/budget`.

For the same reason the Ring-0 interface is **`core.Estimator`** with an `Estimate` method, not
`Predictor`/`Predict` as earlier drafts called it: an interface named for the value it produces
rather than for a second verb meaning the same act.

### 2.4 Confirmation is run-level, capped, and computed where the remaining count is known

**The human currently consents to one Case's price and pays for the whole run (H4).**
`ConfirmFunc` receives a single operation's estimate, and `Guard.askOnce` sets `g.confirmed = true`
once for the life of the run (`budget.go:271`). 10,000 Cases at $0.04, threshold $0.01: the first
Case prompts with **$0.04**, the user types `y`, the run spends **$400**. DESIGN.md:265 says the CLI
*"prints an estimate and confirms before any run over threshold."* What ships is *sample and confirm*.

The second draft's fix was under-specified in three ways, each putting a wrong number in front of a
human at the consent moment (H-D):

1. **It ignored the cap.** With `--max-cost-usd 5.00`, honest maximum exposure is $5.00 + `N × δ_max`,
   not `10,000 × estimate`. Showing $328 when the guard stops at $5 is simply false.
2. **It could not be computed on resume.** §2.4 put it in the CLI *"because it already has
   `counts.Dev`"* — but the completed-Case set is loaded **inside** `core.Baseline`. A run killed at
   9,988/10,000 would prompt for **$328** to finish 12 Cases.
3. Which estimate, given the (now-deleted) varying one.

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

So retry is bounded by **time as well as attempts** — not time instead of attempts (M-10). A pure
time budget reintroduces §2.1's own argument one level down: each attempt settles `Calls: 1`
(`core/baseline.go:486`), so under a clamped `Retry-After` a single Case can consume dozens of calls
against `--max-calls`. Both bounds, whichever binds first.

**And the break is made loud (M-11).** Reinterpreting `MaxAttempts` as a time budget would keep
every caller compiling while silently changing behavior — its godoc (`core/baseline.go:99-101`)
currently promises *"1 disables retry"*. So `MaxAttempts`/`DefaultMaxAttempts` are **removed** and
`RetryBudget`/`DefaultRetryBudget` added alongside a retained attempt ceiling. A deleted identifier
is a compile error; a reinterpreted one is a silent behavior change.

Debt 23's trigger requires re-running the executor's conformance and concurrency tests against a
real transport. That is a deliverable of M2-7, not a follow-up.

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

  Draft 3 proposed `KNO_API_KEY_<NORMALIZED_HOST>`, which **pass three killed (H-7)**: env var
  names permit only `[A-Za-z0-9_]`, so any mapping from DNS collapses characters. `api.groq.com`,
  `api-groq-com`, and `api_groq_com` all normalize to `API_GROQ_COM` — so an attacker registering
  the typosquat `api-groq.com` receives the key bound to `api.groq.com`, **with no flag involved**.
  A mechanism introduced to prevent threat #1, delivering threat #1. Ports, trailing dots, case, and
  punycode were all unspecified, and the user could not guess the name anyway.

  So the binding is **explicit, never derived**: `--key-env api.groq.com=GROQ_API_KEY` (the *name*
  of a variable is not a secret, so this does not touch the no-keys-in-flags rule), or a
  `KNO_API_KEYS` host-to-varname map. A scheme's default env var applies **only** to that scheme's
  default host. **A key bound to host A never reaches host B, flag or no flag**, and two bindings
  resolving to the same host are refused at startup rather than silently ordered.
- **Private-address rules, stated once (H-7).** Draft 3 had §2.8 refusing loopback/RFC1918 unless
  opted in and §6 allowing them without a credential — two contradictory specs for one security
  control in one document. §6's reasoning was also wrong: SSRF's other harm is using Kno as a proxy
  into an internal network, where the response body lands in `Response.output` and is **persisted as
  a trace**. That harm is credential-independent.

  The single rule: **link-local (`169.254.0.0/16`) is refused unconditionally.** Loopback and
  RFC1918 are refused **unless opted in**, credential or not — local vLLM and Ollama are the
  documented case (`common.proto:198`, DESIGN.md:222) and they get one flag, not two. §6 carries no
  competing rows.
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
- `docs/debt.md#27` (M2-9): the resumed aggregate is fixed by **summing `score_proto`** in the store.

So M2-1 nulls what M2-9 must sum. Concrete: run `kno baseline`, run `kno purge` for privacy, get
interrupted, `--resume`. `CompletedCases` and `SettledSpend` survive — they read dedicated columns,
so entry 25's guarantee holds — but the aggregate now sums over the unpurged subset and reports it
as the run's `AggregateScore`, printed at `cli/render.go:166`, emitted in `--json`, **and carried on
the event stream as `RunFinished.aggregate_score`.** A silently-wrong reference number produced by
two features the same milestone ships: exactly the prime-directive-5 failure entry 27 exists to close.

**And it needs a migration, which draft 3 never mentioned (H-3).** `store/sqlite.go:19-20` says
opening an existing database is *"a no-op rather than a migration"* — the schema is
`CREATE TABLE IF NOT EXISTS`. So for any user with an M1-era `kno.db` (the default `--db` path):
`NewSQLite` succeeds because the table exists and the statement is skipped, the first Case makes a
**paid** provider call, `RecordOutcome`'s INSERT names `score_value`, SQLite returns *"table
outcomes has no column named score_value"*, and the run aborts. Money spent, zero rows recorded,
zero resumability, and every retry repeats it. The first paid run of the whole project would fail
this way.

Worse, an `ALTER TABLE ADD COLUMN` alone is not enough: M2-9 summing `score_value` over rows written
before the column existed sums NULL while `OutcomeCounts` still counts those Cases in `priorScored`
(`core/baseline_events.go:52,84-89`). The resumed aggregate would then be **numerically wrong and
biased toward zero** — strictly worse than debt 27, which is at least honestly partial.

**Fix, in M2-0/M2-1:** `PRAGMA user_version` and a real migration path, with a test that opens an
M1-era database. Backfill `score_value` from `score_proto` where it exists. Where it does not —
purged rows, or purged-then-upgraded — mark the Run and **refuse to report an aggregate rather than
report a wrong one.** Prime directive 5: no number is better than a number that is quietly false.

Add `score_value REAL` and `score_passed INTEGER` columns to `outcomes`. The
number lives in a column; the blob keeps only what is genuinely trace content (`rationale`,
`judge_model`). Purge nulls `response_proto` and `score_proto`; M2-9 sums the column, which purge
never touches. **The same reasoning applies to the fields §5 adds (M-8):** the refusal flag,
truncation/stop-reason, and `usage_estimated` all live *inside* `Response`, which purge nulls — and
§6 requires the refusal flag survive so M3's Value stage can exclude refused Cases. So the migration
adds a column per fact: `score_value`, `score_passed`, `refused`, `truncated`, `usage_estimated`,
`provider_build_id`, `resolved_model`. An earlier draft listed only the first four, which
[ADR-0004](../adr/0004-per-run-observations.md) contradicted — the Phase-3 review of M2-0 caught the
two documents disagreeing about the same migration. The test entry 25 explicitly requires — *"asserting `CompletedCases` is unchanged by
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
are corrected in M2-6 whether or not `doctor` lands; `doctor` is not in DESIGN.md's CLI surface and
DESIGN already gives capability printing a home in `kno plugins list`, so the conflict is flagged
rather than resolved by fiat (L2).

Also in M2-6: `cli/render.go:41-43` returns `ErrCapabilityUnsupported` for an unknown agent scheme,
but `errs.go:80` documents `ErrInvalidInput` as the sentinel for *"an unknown adapter name."* M2
rewrites that function; the sentinel is fixed there (L4).

### 2.11 *(deleted)*

The second draft added an injected `func(*Case) budget.Estimate` on `BaselineOptions` —
a public Go API change — to carry a per-Case adaptive estimate. §2.3 deletes the adaptation,
so this dissolves with it (H-A, H-C). The flat `EstCostPerCallUSDMicros` is replaced by an
adapter-supplied pessimistic estimate computed at the adapter, and `core` keeps the scalar
shape it already has. No `core` API break, and `core/baseline.go` does not grow a streaming
quantile estimator on top of its current 657 lines (M-j).

## 3. PR decomposition

| PR | Scope | Depends on | Repays |
|---|---|---|---|
| M2-0 | **Settles §10a's open decision (M-7)**, with an ADR. Proto: agent-ref grammar, price vector, refusal/truncation/usage-estimated flags, resolved-model and provider-build fields, retry/rate-limit/overshoot **events**, `RunResumed` payload, `CaseExecution` submessage (ADR-0004) | — | 26 (partly), 29 (partly) |
| M2-1 | `store`: `PRAGMA user_version` migration; `score_value`/`score_passed`/`refused`/`truncated` columns; backfill from `score_proto`; **`kno purge`** (`store` + `cli`, nulling blobs only) | M2-0 | 25 |
| M2-2 | `core.Estimator` optional Ring-0 interface; `Guard.Overshoot`; `budget` naming | M2-0 | — |
| M2-3 | `internal/transport`: rate limiter, `GetBody` pinning + round-trip counter, redirect refusal, explicit per-host key binding, private-address rules, timeouts, redaction, OTel spans, `goleak.VerifyTestMain` | M2-0 | 18 (partly) |
| M2-4 | Agent-ref parser: fuzz target + golden table | M2-0 | 4 (partly) |
| M2-5 | Pricing table + pessimistic estimate behind `core.Estimator` | M2-2, M2-4 | — |
| M2-6 | `core`: feasibility check after `Restore`; run-level confirmation after `CompletedCases`; `RetryBudget` replacing `MaxAttempts`; `ErrTransportTransient`; resume check on resolved model; per-attempt spend on `Outcome` | M2-5 | — |
| M2-7 | `openaicompat` adapter, fixtures, `goleak.VerifyTestMain`, executor conformance against a real transport; **wire the env cap into the live-test path and delete the grep interlock** | M2-3, M2-5 | 11, 18, 23; investigates 20 |
| M2-8 | `anthropic` adapter, fixtures, `goleak.VerifyTestMain` | M2-7 | 18 |
| M2-9 | Resumed `AggregateScore` summed from `score_value`; refuse an aggregate where the column cannot be backfilled | M2-1, M2-6 | 27 |
| M2-10 | Event **emission** for the M2-0 payloads; `closeRun`/`jsonreport` migration to `BaselineDetail`; resumed `RunStarted` remaining-count; `core/baseline.go` split | M2-6 | 26, 29 |
| M2-11 | CLI wiring, flag surface (§9), capability matrix, corrected `errs` fix lines, `cli/render.go:41-43` sentinel, docs, cookbook, vhs tape | M2-4, M2-6, M2-7, M2-8 | — |

**Ordering.** `kno purge` lands right after proto (P1-M5): merges are incremental, so `main` must
never carry real trace collection with no purge. It also lands the migration and columns M2-9
depends on and purge must not destroy (§2.9a).

**Dependency edges corrected (M-12).** M2-7 depends on M2-4 (it is unreachable without the
`@base-url` parser); M2-11 depends on M2-8 (otherwise the `anthropic:` scheme merges unreachable)
and on M2-6 (whose flag semantics it documents); M2-9 and M2-10 both touch the aggregator, so M2-10
follows M2-6 and M2-9 follows M2-1.

**Package boundaries (M-13).** Draft 3 put the feasibility check "in `core`" while scoping that PR
to `stats/budget`, and labelled the purge PR `store` when `kno purge` is a `cli` command. Both are
corrected above: `core` work is M2-2/M2-6, and M2-1 explicitly spans `store` + `cli`.

**Severability (M-1).** If scope must be cut, the severable PR is **M2-8** (the Anthropic adapter) —
draft 3 said "M2-4" in §0 and "M2-5" in §4, two different numbers for one thing, both of which now
point at the money machinery.

**M2-10 exists because no draft had a row for event emission** (M-b, M-m). `event.proto` already
defines `SpendRecorded` and `StageProgress` (`:54,57`) that **nothing emits**, referenced only by
`internal/schema/schema_test.go`. Adding payloads without emission would make five.

## 4. Alternatives considered

**A. Ship `value` first, against the fake (rejected).** `fake.Invoke` returns `c.GetExpected()`, so
every Case scores 1.000 and no delta is meaningful. It would build the stage without ever
exercising what the stage measures.

**B. One adapter only, defer Anthropic (viable; M2-8 is severable).** Cheaper. Not chosen because
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
  emitter-side change is two lines, in M2-10.
- **New event payloads carry regime-neutral fields (M-m):** `attempt_ordinal`, `retry_after_ms`,
  `deadline_remaining_ms`. Freezing `attempt`/`max_attempts` in M2-0 and then replacing the
  attempt-count regime with a time bound in M2-6 would leave the generated OpenAPI documenting a
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
| Base URL over plain HTTP | Refused unless opted in — one flag, per §2.8 |
| Base URL at loopback / RFC1918 | Refused unless opted in. Not credential-conditional: a trace persisted from an internal endpoint is harm without a key (H-7) |
| Base URL at link-local (`169.254.0.0/16`) | Refused **unconditionally**. Instance metadata has no legitimate use here |
| Key bound to host A, request to host B | Refused. No flag overrides this |
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
- **`goleak.VerifyTestMain` in each adapter package** — debt 18, in M2-7 and M2-8, plus `internal/transport` in M2-3 (L-a). Note the first
  draft assigned it to M2-1, which is `internal/transport`, not an adapter package (P1-M6).
- **Fuzz target + golden table on the agent-ref parser** (§2.2).
- **A span test** asserting no Case or Response content, and no headers, reach any span (§2.7).
- **A redaction test** over real error paths — with a real transport it stops being theoretical.
- **Live tests** (`KNO_LIVE_TESTS=1`) opt-in, never in PR CI, nightly with `KNO_MAX_COST_USD`
  actually read by code — debt 11. **The interlock is deleted in M2-7, the same PR that wires the
  cap** (H-6). Draft 3 said M2-3, which is `internal/transport` — no live tests, no env-cap wiring.
  Between M2-3 and M2-7 merging, `main` would have carried live targets with the grep gone and still
  no Go file reading the variable: setting the env var satisfies the first check and nothing
  enforces the cap. That is entry 11's original trap, re-armed — the one PR #23 just closed.

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
- **The flag surface** (M-n), enumerated here because M2-11 silently owned it: `--base-url` (or the
  `@` form), `--allow-non-default-host`, `--allow-insecure-base-url`, `--max-output-tokens`,
  `--temperature`, `--seed`, plus per-host key selection. DESIGN.md's mirroring rule means each also
  needs a `KNO_*` env var; keys are the stated exception (§11a). `make docs` snapshot-tests CLI help
  and the vhs tape is re-recorded when output changes — both budgeted in M2-11.
- **godoc, CLI help, CHANGELOG** — standing.

## 10. Debt ledger

Owner column included because `docs/debt.md`'s schema requires one and CLAUDE.md requires one, and
draft 3's §11 claimed every row had an owner while §10's header did not have the column (M-14).

| # | Trigger | Disposition | Owner |
|---|---|---|---|
| 4 | "when the agent-ref parser lands" | Partly repaid, M2-4 | @devarispbrown |
| 11 | Re-dated 2026-08-21 to M2 | Repaid **M2-7** — cap wiring and interlock deletion in **one** PR, as the entry's "that PR must … and …" requires. Draft 3 split them across M2-2 and M2-6, which would have re-armed the trap on `main` (H-6). Mitigation hole fixed separately in PR #23 | @devarispbrown |
| 18 | "when the first adapter lands" | Repaid M2-7, M2-8, **plus M2-3** — `internal/transport` is where the goroutines actually live (idle connections, rate-limiter and retry timers), so both the entry's letter and its purpose get served (L-a) — **plus the CONTRIBUTING.md sentence the entry requires** | @devarispbrown |
| 20 | "M2, with the first real adapter" | **Investigated M2-7, implemented M3.** OpenAI supports `Idempotency-Key` today, so the answer is known-positive for one provider — but persisting a key *before* the call is a second durable write per Case on the hot path, and it switches on stdlib replay (§2.1). M2 records the finding; M3 implements (M-o) | @devarispbrown |
| 21 | "when the second stage lands (M2)" | `value` moved to M3, so the trigger's condition has not occurred. Re-dated to M3 with that written reason | @devarispbrown |
| 23 | "M2, with the first real adapter" | Repaid **M2-7** | @devarispbrown |
| 25 | "before `kno purge` is implemented (M2)" | Repaid M2-1, **including the test the entry requires** and which draft 2 omitted (P1-M5), extended per §2.9a to cover `SettledSpend` and the aggregate | @devarispbrown |
| 26 | "before the first non-Case-executing stage writes a `Run`" | **Partly repaid M2-0** (schema), **fully M2-10** (writer + reader migration). Labelled "partly" because a `DEBT()` comment pointing at a row marked repaid is worse than a live marker (P1-M6). Trigger retained until M2-10 | @devarispbrown |
| 27 | "before any stage reads `AggregateScore` — M2 at the latest" | Repaid M2-9, not re-dated | @devarispbrown |
| 29 | "before the TUI renders progress" | **Partly repaid M2-0** (payload), **fully M2-10** (emitter). Draft 3 labelled this "repaid" while labelling entry 26 — identical shape, identical split, same file — "partly" (M-2). Same label now, and the trigger is retained until M2-10 | @devarispbrown |
| *new* | **The cap is a soft bound**, `C + N × δ_max`, and settlement cannot enforce it — only observe it (H-4) | Trigger: before 1.0, or when a provider's settled cost exceeds its reservation in nightly live tests. Ledger entry 32. `Guard.Overshoot()` landed in M2-2, which makes the excess *computable*; nothing SURFACES it until M2-10 emits `SettlementOvershoot` | @devarispbrown |
| *new* | **Headroom forfeiture**, `N × pessimistic`, measured at 4.7% (N=8) and 19.5% (N=32) of a $5.00 cap | Trigger: when a user reports a run stopping below its cap, or when `f = 0.25` (§2.3) is shown wrong by live data | @devarispbrown |
| *new* | Cross-process rate-limit coordination | Trigger: when `kno serve` lands (v0.3), which makes concurrent runs the normal case | @devarispbrown |
| *new* | Streaming unimplemented | Trigger: when any stage needs incremental output. That PR adds an SSE frame parser **and its fuzz target** | @devarispbrown |
| *new* | Approximate token counting | Trigger: **when nightly live tests report a median divergence above 10%** between our input count and the provider's. M2-7 adds that comparison to the nightly job — draft 3 conditioned this row on a check no PR performed, which is "someday in a lab coat" (M-15) | @devarispbrown |

**On 27.** No *stage* reads `AggregateScore`, which is true and irrelevant: `cli/render.go:166`
prints it every run, `cli/jsonreport.go:65` puts it in `--json` (what a CI gate parses), and
`RunFinished.aggregate_score` puts it on the event stream. Re-dating a trigger that says "M2 at the
latest," in M2, would be the ledger rule failing its first test.

**Re-dates: one** (entry 21, on a condition that demonstrably has not occurred). Draft 1 proposed
three and should have said four; draft 2 proposed one, to a milestone that does not exist.

## 11. Accepted risks

Every entry has a row in **`docs/debt.md`** — entries 32-36 — with a trigger and an owner.

Draft 4 said "a §10 row", meaning this document's own table, and that was not mirroring: CLAUDE.md
requires accepted debt to live in the ledger, not in the plan that accepted it. The rows were
written during M2-2, after a Phase-3 review pointed out that an accepted-risk row was phrased as
though its repayment mechanism already worked. Draft 2 claimed five were mirrored and
added one row; draft 3 added rows but no owner column and claimed one risk was "narrowed, not
accepted" when it was not (M-14, H-4). Both are corrected.

1. **The cap is a soft bound**, `C + N × δ_max` — **accepted, now observable.** `Settle` cannot
   enforce a ceiling: `fitsLocked` already denies everything once `spent ≥ cap`, and the money is
   spent regardless. `Guard.Overshoot()` makes the breach visible where `remainingLocked`'s
   `max(0, …)` currently hides it. §10 row.
2. **Headroom forfeiture**, `N × pessimistic`, **measured** at 4.7% of a $5.00 cap at concurrency 8
   and 19.5% at concurrency 32 (`stats/budget/throughput_test.go`). Mitigated by the
   feasibility check at `f = 0.25`; stated in `docs/mental-model.md` rather than left to be
   discovered. §10 row.
3. **The dark-spend window** (`docs/debt.md#20`) is only partly closable. M2 investigates, M3
   implements. §10 row.
4. **Cross-process rate-limit coordination** is out of scope. §10 row, trigger `kno serve` (v0.3).
5. **Streaming is not implemented.** §10 row.
6. **Approximate token counting.** §10 row, with the divergence check that makes its trigger real.

## 10a. Open decision — SETTLED in M2-0 by [ADR-0004](../adr/0004-per-run-observations.md)

**Resolution: option B**, with one deviation from the ledger's prescribed naming. Facts about a
Case-executing Run live in a `CaseExecution` submessage on `Run`, aggregated from persisted outcome
rows at close rather than from in-memory counters — so they survive a crash and stay correct across
a resume, the same property that repays entry 27. Not named `BaselineDetail` as entry 26 prescribed,
because Value also executes Cases and a stage-named message would be wrong for it. The backend
identity is `observed_backends`, repeated, and does not reuse the word "fingerprint". Original
statement of the question follows.

### The question as it stood

**Pass three's M-7 is unresolved, and this plan does not resolve it.** Recorded here rather than
patched over, because three drafts of this document have now demonstrated that a design question
answered in prose gets answered wrongly.

**The question: where do per-run observations accumulate?**

M2 wants four things "on the `Run`" that nothing today can produce:

- the **set** of `system_fingerprint` values observed (§2.5 says a set; §5 lists a singular field —
  the plan contradicts itself, which is the tell that the question was never settled);
- the refusal count (§6);
- the truncation count (§6);
- the usage-estimated count (§2.3).

None has anywhere to live. `Run` is written twice — at `openRun`, before any Response exists, and
at `closeRun`. The only cross-worker state is `core/baseline_events.go`'s `aggregator`, which
carries `sum`, `scored`, `errored`, `seq`, and the two resume-seeded priors. Adding four
observation channels to it is a real design change, not a field.

Separately: `Run` already has `input_fingerprint`, documented at `run.proto:105-108` as the hash
whose mismatch refuses a resume. Adding `system_fingerprint` puts two unrelated meanings of
"fingerprint" on one message — the vocabulary drift prime directive 2 forbids.

**Options, none chosen here:**

| | Approach | Cost |
|---|---|---|
| A | Extend `aggregator` with the four channels; `closeRun` writes them | Grows the one piece of shared mutable state in the stage; `core/baseline.go` is already 657 lines against a ~400 soft cap |
| B | A `RunObservations` submessage on `Run`, accumulated in the store rather than in memory — the counts are already per-outcome columns after §2.9a's migration, so `closeRun` could aggregate with SQL | One more query at close; keeps `aggregator` untouched; the numbers survive a crash because they are already persisted |
| C | Emit them only as events, never on `Run` | Cheapest; a consumer that did not replay full history cannot see them, and `--json` cannot report them |

**B looks right** — the per-outcome columns exist anyway, and deriving a run-level number from
persisted rows rather than from in-memory counters is what fixed debt 27. But it is a proto and
store decision, so **M2-0 settles it and records the choice in an ADR**, and this row stays open
until then. On the naming collision, `resolved_model` and `backend_fingerprint` avoid reusing
"fingerprint" for the resume hash; M2-0 picks the names.

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

## 12. Review coverage

**Three Phase-1 passes, all BLOCK.** 20 HIGH + 28 MEDIUM from passes one and two; 8 HIGH + 15
MEDIUM + 7 LOW from pass three attacking the result.

The three review documents are checked in beside this plan at `docs/plans/reviews/` so **coverage is
verifiable by someone other than the author.** Pass three showed why that matters (H-8): draft 3
asserted "all 48 findings addressed" while three IDs — `H1`, `H10`, and `P1-M8` — appeared nowhere
in the document, and `H1` appeared only as an example of the numbering convention. An audit whose
evidence lives in one conversation is not an audit.

### The three untraceable IDs, dispositioned

Pass three found `H1`, `H10`, and `P1-M8` cited nowhere substantive, and concluded findings had been
"dropped rather than fixed." **Checked against the pass-one document now in-repo: all three are
substantively addressed — the failure was traceability, not coverage.** Recorded explicitly so the
distinction is auditable rather than asserted:

| ID | Finding | Where it is addressed |
|---|---|---|
| **H1** | The estimate "can only be too high" contradicts itself; the guard has no settlement-time ceiling | §2.3 opens on the contradiction; the ceiling is dispositioned in full under H-4 — it is *observability*, not enforcement, and carries a §10 row |
| **H10** | §10 omitted two ledger entries whose triggers name M2 | §10 has rows for entries **20** and **21** |
| **P1-M8** | Re-dating entry 27 is not defensible — a human and a CI gate read `AggregateScore` every run | Entry 27 is **repaid in M2-9**, not re-dated; the argument is restated under §10's "On 27" |

Pass three's stronger claim — that findings were dropped — is **not correct**, and saying so matters
more than being agreeable: the plan's job is to be accurate, including about its reviews. Its
narrower claim, that an audit whose evidence lives in one conversation is not an audit, was exactly
right, and is why `docs/plans/reviews/` now exists.

### Findings not adopted

Two, both rejected on measurement rather than judgment:

- **Pass one's H8** — "pessimistic reservation makes the cap unusable, stopping at ~6% of a $5 cap."
  Wrong: reservations release on settle. Measured at 95.3% (N=8). This is why draft 2's entire
  adaptive estimator was deleted.
- **Pass three's framing of the settlement ceiling as achievable enforcement** is accepted; its
  implication that draft 3's deferral was therefore fine is not — the risk now carries a §10 row
  either way (H-4).

Everything else is fixed above or carries a §10 row with a trigger and an owner.

### Process defects, recorded because they are the actual pattern

Each round fixed the previous round's findings and introduced new ones **of the same kind**:

1. **Draft 2:** accepted pass one's H8 arithmetic without measuring it, and built the plan's largest
   section on it. Wrong by 16×.
2. **Draft 3:** corrected that arithmetic — and got the correction wrong too, by settling in the same
   instant it reserved so `N` reservations never overlapped. 98.2% where the truth is 95.3%. Also
   claimed "no `core` API change" while specifying a per-Case reservation that requires one (H-1);
   applied its own resume-placement reasoning to §2.4 and not to the feasibility check it wrote in
   the same edit (H-5); and gave entries 26 and 29 opposite labels for identical situations (M-2).
3. **Draft 3's audit was not an audit** (H-8), which is what let (1) and (2) stay invisible.

Two countermeasures, both now in the repo rather than in prose:

- **Every quantitative claim ships the test that produced it.** `stats/budget/throughput_test.go`
  asserts the forfeiture as a **bound**, not a count — three runs at concurrency 8 spread by more
  than a point, so a test pinned to `2455` would be flaky on arrival (M-3).
- **The review documents are in-repo**, so ID coverage is mechanically checkable.
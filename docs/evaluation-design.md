# Evaluation design

Kno measures how candidate data changes the outcomes you care about. **Attribution quality is
bounded by the quality and granularity of your eval signal** — this page is the deep version of
the README's best-practices section, and it uses the vocabulary the CLI does (Case, Evals,
Goal, Asset, Pool, holdout).

## 1. How granular should evals be?

Granular enough that each Goal names one behavior you would act on. "The agent is good" is not
a behavior; "answers refund-policy questions correctly" is. The test: if a Goal moves, do you
know what to change? If the answer is "something, somewhere", split it.

Granularity has a real cost — every Goal needs its own Cases — so the practical rule is to
split what you'd act on separately and merge what you'd fix together.

## 2. How many Cases are enough?

There is no flat number, and any doc that gives one is lying about statistics. What "enough"
means is: **enough that the confidence interval excludes the effect you'd act on**. A Case set
too small to separate "helped a little" from "variance" is underpowered — and Kno says so: the
interval is reported with every delta, and an underpowered measurement is labeled, never
presented as a point estimate pretending to be truth.

Practical anchors, honestly labeled as heuristics:

- **~10+ Cases per behavior** before a small improvement is distinguishable from noise.
- **The interval is the answer.** If the interval spans "would act" and "wouldn't", add Cases
  before deciding — that is exactly the information the interval exists to carry.
- **A single Case is a demonstration, not evidence.** One Case can show the mechanism works;
  it cannot attribute a change.

## 3. Deterministic scoring vs LLM judges

Prefer deterministic when a behavior has a mechanical answer: exact match, structured
assertions, programmatic checks. Deterministic scores are reproducible, free, and their noise
model is honest.

Use a judge Goal when the behavior is genuinely semantic ("tone", "helpfulness"). When you do:

- Write the rubric as criteria, not vibes. "Answers with the policy, cites it, and offers the
  next step" beats "answers helpfully".
- Anchor the scale. A 1-5 with no definitions is a random number generator wearing a rubric.
- Calibration matters. Judge agreement with a human-labeled set is how you know the judge
  isn't the thing being measured. (`judge calibrate` is the v0.2 tooling for this.)

## 4. Multi-dimensional Goals

Accuracy, policy compliance, tool selection, tone, latency, and cost are different questions
with different audiences and different fixes. Measure them separately, then aggregate — Kno
can roll specific outcomes into a higher-level view more reliably than it can explain a single
coarse score.

When you do aggregate, do it with intent: a weighted sum where the weights encode what you'd
trade is a decision; a sum where the weights are whatever the schema defaulted to is a
number.

## 5. Holdouts and leakage

This one is not advice — it is enforced. Kno seals a holdout at baseline time: no stage before
`validate` can read those Cases, and the seal is a code-level boundary, not a convention. What
that means for eval design:

- **Never tune the eval on the same Cases you measure with.** Authoring expectations for a
  Case means you've seen it; those Cases belong in dev, and the holdout exists precisely
  because dev scores drift upward from that familiarity.
- **The holdout is the honest number.** Every report before `validate` says so explicitly:
  dev estimates are labeled "not yet validated on holdout" until the holdout speaks.

## 6. Variance and confidence intervals

Kno reports deltas with intervals, never bare point estimates. A delta without its interval is
not reported at all. The full epistemics — what each number claims and does not, the
screening correction, the tokenizer-bias caveat on the cost denominator — is
[What the numbers mean](what-the-numbers-mean.md). Design evals assuming that page is the
contract: if your Case set can't distinguish real effect from noise, Kno will say so rather
than pretend.

## 7. Examples by workload

**RAG / knowledge-base agents.** Goals: answer-with-citation, answer-correctness per topic,
refusal-on-no-grounding. Cases: one question per Case, tagged by topic; the pool is the
knowledge assets you're choosing between. Watch for: citation-less correct answers scoring
"correct" — if grounding is a behavior you act on, it's its own Goal.

**Support agents.** Goals: policy-accuracy per policy family, escalation-detection,
no-unsupported-promises. Cases: real ticket questions with the expected response. Watch for:
a single "CSAT-like" score that blends speed, tone, and accuracy into one unactionable
number.

**Coding agents.** Goals: tests-pass, tool-choice, diff-minimality, plan-adherence. Cases:
repo-scoped tasks with a runnable check, not "write good code". Watch for: eval leakage
through the task itself (a Case whose test you wrote after seeing the model's first attempt
is a dev Case, not a holdout Case).

**Workflow agents.** Goals: step-order, idempotence, retry-recovery, side-effect-correctness.
Cases: scripted world states with expected final states. Watch for: judging the final state
alone when the path matters — a workflow that deletes and re-creates arrives at the same
state as one that updates in place, and only one is acceptable.

## 8. Common anti-patterns

- **One giant score.** Unactionable, and worse: it teaches the team to read changes in it as
  "the model got better/worse" without a cause.
- **Few Cases, many assets.** With three Cases per behavior, every Asset's delta is
  indistinguishable from noise — the tool will correctly refuse to rank anything, and the
  eval, not the tool, is the bottleneck.
- **Reusing eval Cases as tuning data without a holdout.** Dev/holdout separation is the
  mechanism that keeps the measurement honest; designing it away defeats the tool.
- **Changing the eval mid-experiment.** The baseline is the reference; a moved Goal moves the
  meaning of every delta. New behavior = new Goal = new baseline.
- **Judging what the model was told.** If the expected answer is pasted into the prompt or
  the Asset, the Goal measures copying, not the behavior.

## The inspect idea

A `kno eval inspect` command — flagging underpowered behaviors, multi-behavior Cases, and
coarse Goals from the data a run already records — is on the roadmap; this page is written so
that command's output has a vocabulary to point at.

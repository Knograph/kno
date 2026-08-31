# The README quickstart, recorded.
#
#   brew install vhs && make tape
#
# What this shows, and why in this order: what the build supports, a real eval
# file, one command, a scored run that costs nothing, and three Cases held
# back. The held-back line is the point of the whole tool, so it has to survive
# into a GIF that people watch for eleven seconds and then decide about.
#
# `fake:` throughout. A quickstart that bills the viewer's API key the first
# time they paste it is not a quickstart, and a tape recorded against a real
# provider would make this file's output vary with someone else's model.
#
# The score will read 1.000, and that is honest rather than flattering: `fake:`
# answers every Case with what the Case expects, by construction. The GIF is
# showing that a run HAPPENED, was scored against a declared Goal, cost nothing,
# and sealed a holdout — not that anybody's agent is perfect. The README says so
# in the line directly under it, and this comment exists so nobody "improves"
# the tape by picking an eval file that makes the number look earned.

Output docs/quickstart.gif

Require kno

Set Shell zsh
Set FontSize 16
Set Width 1100
Set Height 620
Set Padding 20
Set Framerate 24
Set PlaybackSpeed 1.0
Set TypingSpeed 55ms
# Theme is pinned rather than left to the recorder's terminal, so a
# re-recording after a CLI change differs only where the CLI changed.
Set Theme "Catppuccin Mocha"

Hide
Type "cd $(mktemp -d)" Enter
# Twelve Cases, and the number is load-bearing rather than arbitrary: the
# split is id-keyed, so eight land in dev and four are held back, and the
# control arm reserves 0.3 of dev — int(8 * 0.3) = 2, which is the minimum
# an interval can be computed from. At nine Cases the reserve rounds to ONE
# control Case, every interval comes back nil, and the value table renders
# "sample too small or ragged to form an interval" for every row. This tape
# spent its first life claiming deltas of 0.0000 that the screen never
# showed. Do not shrink this file without re-recording and reading what the
# frames actually say.
Type "cat > cases.jsonl <<'JSONL'" Enter
Type '{"id":"refund-01","input":"How do I get a refund?","expected":"Refunds are issued within 5 business days.","tags":["refunds"]}' Enter
Type '{"id":"refund-02","input":"Refund policy for sale items?","expected":"Sale items are final and non-refundable.","tags":["refunds"]}' Enter
Type '{"id":"refund-03","input":"Can I refund a gift?","expected":"Gift refunds go to store credit.","tags":["refunds"]}' Enter
Type '{"id":"ship-01","input":"When will my order arrive?","expected":"Standard shipping takes 3-5 business days.","tags":["shipping"]}' Enter
Type '{"id":"ship-02","input":"Do you ship internationally?","expected":"We ship to 40 countries.","tags":["shipping"]}' Enter
Type '{"id":"ship-03","input":"Can I change my delivery address?","expected":"Yes, until the order ships.","tags":["shipping"]}' Enter
Type '{"id":"acct-01","input":"How do I reset my password?","expected":"Use the reset link on the sign-in page.","tags":["account"]}' Enter
Type '{"id":"acct-02","input":"Can I change my email?","expected":"Yes, from Settings > Account.","tags":["account"]}' Enter
Type '{"id":"acct-03","input":"How do I close my account?","expected":"Contact support and we close it within 24 hours.","tags":["account"]}' Enter
Type '{"id":"bill-01","input":"Why was I charged twice?","expected":"A duplicate charge is released within 48 hours.","tags":["billing"]}' Enter
Type '{"id":"bill-02","input":"Where is my invoice?","expected":"Invoices are emailed and available under Billing.","tags":["billing"]}' Enter
Type '{"id":"bill-03","input":"Do you take purchase orders?","expected":"Yes, for annual plans.","tags":["billing"]}' Enter
Type "JSONL" Enter
Type "clear" Enter
Show

Type "kno doctor" Sleep 500ms Enter
Sleep 3.5s
Type "clear" Enter

Type "head -2 cases.jsonl" Sleep 500ms Enter
Sleep 2.5s

Type "kno baseline --evals cases.jsonl --agent fake: --goal exact-match --run-id qs" Sleep 500ms Enter
Sleep 5s

# The distinctive half: value three candidate assets against that baseline.
# With fake: the deltas read 0.0000 WITH their intervals — the tape is
# proving the LOOP (routed, injected, re-measured against controls, reported
# with intervals), not that any asset helps. The interval is the point: it is
# what lets select say "no-effect, CI crosses zero" rather than the weaker
# "underpowered". Do not "improve" this tape by choosing assets that make the
# numbers look earned.
Type "cat > pool.jsonl <<'JSONL'" Enter
Type '{"id":"refund-policy-v3","content":"Refunds are issued within 5 business days.","kind":"knowledge"}' Enter
Type '{"id":"ship-promise","content":"Standard shipping takes 3-5 business days.","kind":"knowledge"}' Enter
Type '{"id":"brand-guide","content":"Use sentence case everywhere.","kind":"knowledge"}' Enter
Type "JSONL" Enter
Type "clear" Enter

Type "kno value --evals cases.jsonl --pool pool.jsonl --baseline-run-id qs --run-id qs-value" Sleep 500ms Enter
Sleep 5s

# Select: the honest verdict. With fake: every delta is 0.0000, so the
# corrected intervals all cross zero and the portfolio is empty — "include
# nothing new" is a legal, first-class outcome, and the rejection log says
# WHY for each asset. This is the tool doing its job: proving that these
# three assets do not earn their place. Do not "improve" this tape by
# choosing assets that make the numbers look earned.
Type "kno select --value-run-id qs-value --pool pool.jsonl --max-context-tokens 5000 --max-training-examples 10 --max-cost-usd 1 --run-id qs-select" Sleep 500ms Enter
Sleep 4s

# Export the (empty, honestly empty) portfolio to the tuning-set grammar.
Type "kno export --select-run-id qs-select --pool pool.jsonl --destination tuning_set --out tuning.jsonl" Sleep 500ms Enter
Sleep 3s

# Report: the one-page verdict across the recorded stages. The dev estimate
# carries the "not yet validated on holdout" caveat, because at v0.1 honesty
# is the feature.
Type "kno report --value-run-id qs-value --select-run-id qs-select" Sleep 500ms Enter
Sleep 4s

# Held on the result, because this is the frame people screenshot.
Sleep 3s

# Kno

Kno measures the marginal value of every data asset you're considering feeding an LLM agent — then tells you which ones earn their place, what they cost, and where each belongs: context, knowledge base, or fine-tuning set.

Most teams curate agent data by intuition and dump the rest into JSONL. Kno replaces the guess with a measurement. It runs your agent against your evals, injects each candidate asset, re-runs the affected slice against untouched controls, and reports the delta with a confidence interval — ranked by improvement per dollar, validated as a portfolio against a holdout you never touched. Assets that do nothing get rejected with a reason. Failure modes nothing in your pool can fix get flagged as data you should start collecting.

Single Go binary. No infra. Works with any OpenAI-compatible endpoint, Anthropic, or your own agent behind a shell command.

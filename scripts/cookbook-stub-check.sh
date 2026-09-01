#!/usr/bin/env bash
# cookbook-stub-check.sh — the tombstone stubs stay tombstones.
#
# The recipes moved to uknoAI/kno-examples. What stayed behind is one line per
# old path, and that line is load-bearing: twenty-two branch-pinned links to
# `github.com/uknoAI/kno/blob/main/docs/cookbook/*.md` live in uknoAI/kno-www
# alone, plus however many are in merged PR bodies, issues, and bookmarks.
# Nothing would report them 404ing. `make docs` skips `https://` targets by
# construction and the website's Playwright crawl skips external hrefs, so the
# only thing standing between a deletion and silent rot is these files.
#
# Two invariants, and each one exists because of a specific failure:
#
#   1. A stub is EXACTLY one line whose only content is one link to this
#      recipe's kno-examples page. A stub that grows prose becomes a second
#      copy of the recipe, and a second copy is what the migration was for:
#      README.md and tapes/quickstart.tape carried the same scenario twice and
#      had already drifted over whether refunds are "processed" or "issued".
#      A stub cannot drift if it cannot hold a claim.
#
#   2. The set of stub paths EQUALS the frozen migrated set. A missing stub is
#      an inbound 404, and a 404 is exactly what nothing else in either
#      repository would notice. Stubs are never removed; they are the permanent
#      redirect layer in a system that has no redirect layer.
#
# A page that has NOT migrated is declared in RESIDENT and is checked to be a
# real page rather than a stub, so "not migrated yet" cannot be spelled the
# same way as "migrated and tombstoned".

set -euo pipefail

cd "$(cd "$(dirname "$0")/.." && pwd)"

DIR=docs/cookbook
BASE="https://github.com/uknoAI/kno-examples/blob/main/recipes"

# Frozen at migration. Append when a page migrates; never remove.
MIGRATED="
anthropic
bedrock
braintrust
calibrate-a-judge
check-your-evals
ci-gate
confluence
export-a-tuning-set
first-baseline
github
hubspot
huggingface
jira
langfuse
langsmith
n8n
notion
read-the-whole-story
retention
salesforce
select-a-portfolio
shopify
stripe
value-a-pool
vertex
your-own-provider
zendesk
"

# Pages that still live here, in full. THIS LIST IS EMPTY, and keeping it
# empty is the point rather than an accident of timing.
#
# RESIDENT is not a parking space. A page belongs here only while its commands
# are unreleased, because kno-examples checks every command against the
# RELEASED binary and a page about an unreleased command can carry no honest
# tier there. The claim expires when the command ships, and then the page moves
# and leaves a stub.
#
# Both pages that were ever on this list ran that course within a day of each
# other: `check-your-evals` left when v0.1.4 shipped `kno eval inspect`, and
# `calibrate-a-judge` left when v0.1.5 shipped `kno judge calibrate`. Neither
# needed to settle for a hand-checked tier — both commands turned out to be
# free, offline and deterministic, so both migrated as `executed` pages with
# scenarios behind them.
#
# If you are adding a name below: it must be a page whose commands are not in
# any release, and the comment you add with it must name the release that will
# ship them. Anything else is a page rotting here unchecked, which is what the
# migration was for.
RESIDENT=""

# README.md is the index — a pointer page, not a recipe and not a stub.
INDEX=README.md

RED=$'\033[31m'
GREEN=$'\033[32m'
OFF=$'\033[0m'

fail() {
	printf '%s FAIL %s cookbook-stub-check: %s\n' "$RED" "$OFF" "$1" >&2
	broken=1
}

broken=0
count=0

for name in $MIGRATED; do
	f="$DIR/$name.md"
	if [ ! -f "$f" ]; then
		fail "$f is missing. A stub is never removed: every inbound link to it 404s, and nothing in either repository would tell you."
		continue
	fi
	want="Moved to <$BASE/$name.md>."
	lines=$(wc -l <"$f" | tr -d ' ')
	if [ "$lines" != "1" ]; then
		fail "$f is $lines lines. A stub is exactly one line; anything more is a second copy of a recipe waiting to drift from the real one."
		continue
	fi
	got=$(cat "$f")
	if [ "$got" != "$want" ]; then
		fail "$f does not carry its one link. Expected exactly: $want"
		continue
	fi
	count=$((count + 1))
done

for name in $RESIDENT; do
	f="$DIR/$name.md"
	if [ ! -f "$f" ]; then
		fail "$f is declared RESIDENT but does not exist. Either it migrated — move it to MIGRATED and leave a stub — or the declaration is stale."
		continue
	fi
	if [ "$(wc -l <"$f" | tr -d ' ')" -le 1 ]; then
		fail "$f is declared RESIDENT but reads like a stub. A migrated page belongs in MIGRATED so the stub itself is checked."
	fi
done

# Nothing may appear in docs/cookbook/ without being declared. An undeclared
# page is a recipe that will rot here, unchecked by kno-examples' runner and
# unnoticed by this gate.
for f in "$DIR"/*.md; do
	base=$(basename "$f")
	[ "$base" = "$INDEX" ] && continue
	name=${base%.md}
	declared=0
	for d in $MIGRATED $RESIDENT; do
		[ "$d" = "$name" ] && declared=1 && break
	done
	if [ "$declared" -eq 0 ]; then
		fail "$f is not declared in this script. A new page is either a stub (MIGRATED) or a page that lives here (RESIDENT); an undeclared one is checked by nothing."
	fi
done

if [ "$broken" -ne 0 ]; then
	exit 1
fi

printf '%s  OK  %s %d cookbook tombstone stubs are one line and one link\n' "$GREEN" "$OFF" "$count"

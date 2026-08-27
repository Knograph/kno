#!/usr/bin/env bash
# fold-changelog.sh — rename the hand-written [Unreleased] changelog heading
# to the release it shipped in, via a PR (never a direct push to main).
#
# Debt #76 was the manual version of this: "At release time the hand-written
# heading is renamed from [Unreleased] to the version." The rename must land
# on main AFTER the release publishes — running it before would fold entries
# into a section whose release failed.
#
# Idempotent: when the heading is already folded (a re-tag of the same
# version), it exits 0 without touching anything.
#
# Usage: fold-changelog.sh VERSION
set -euo pipefail

version="${1:?usage: fold-changelog.sh VERSION}"
branch="chore/changelog-${version}"
heading="## ${version} — in detail"

if ! command -v gh >/dev/null 2>&1; then
	echo "fold-changelog: gh is required" >&2
	exit 1
fi

git fetch --quiet origin main
git checkout --quiet -B "$branch" origin/main

if grep -qF "$heading" CHANGELOG.md; then
	echo "fold-changelog: $heading already present — nothing to fold (re-tag)."
	exit 0
fi

# The FIRST [Unreleased] occurrence is the hand-written section this
# mechanism owns. Later occurrences (if any) are prose references and must
# not be touched — hence the 0,/…/…/ construction.
if ! grep -qF "## [Unreleased]" CHANGELOG.md; then
	echo "fold-changelog: no [Unreleased] heading to fold; a release shipped " \
		"with no hand-written entries — leaving CHANGELOG.md untouched." >&2
	exit 0
fi
# awk, not sed: the GNU-only "0,/re/" address for "first match only" is a
# silent no-op on BSD sed — measured on the exact bug it was meant to fix.
# Exact-line equality matters: later prose that MENTIONS the heading must
# not match.
awk -v h="## ${version} — in detail" '
	!done && $0 == "## [Unreleased]" { print h; done = 1; next }
	{ print }
' CHANGELOG.md > CHANGELOG.md.tmp
mv CHANGELOG.md.tmp CHANGELOG.md

git add CHANGELOG.md
if ! git -c user.name="DeVaris Brown" -c user.email="devaris@devaris.com" \
	commit --quiet -s -m "docs: fold the hand-written changelog into ${version}"; then
	# An empty diff (the heading matched but the sed produced no change) is
	# not an error worth a PR.
	echo "fold-changelog: nothing to commit." >&2
	exit 0
fi

git push --quiet origin "$branch"
pr=$(gh pr create --base main --head "$branch" \
	--title "docs: fold the hand-written changelog into ${version}" \
	--body "Automated by the release pipeline (scripts/fold-changelog.sh): the
hand-written changelog section shipped in ${version} and this PR folds its
heading from [Unreleased] to the release, the manual step debt #76 named.
Auto-merges when checks are green.")
# --auto is best-effort: when branch protection or repo settings refuse it,
# the PR simply waits for a human merge — which is still the old manual step
# minus the rename itself.
gh pr merge "$pr" --auto --squash >/dev/null 2>&1 || true
echo "fold-changelog: PR ${pr} opened and set to auto-merge."

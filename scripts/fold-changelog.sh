#!/usr/bin/env bash
# fold-changelog.sh — post-publish reconciliation, via a PR (never a direct
# push to main):
#   1. rename the hand-written [Unreleased] changelog heading to the release
#      it shipped in,
#   2. repair .release-please-manifest.json when the pinned release-please
#      action's post-merge bump did not land — measured once, when 0.0.3
#      shipped and the manifest stayed at 0.0.2, making release-please
#      re-propose 0.0.3 against a tag that already existed.
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

# unreleased_block FILE — the lines under the first "## [Unreleased]",
# up to but excluding the next "## " heading. Used twice: once on the tree
# and once on the tag, to detect the race described below.
unreleased_block() {
	awk '
		!seen && $0 == "## [Unreleased]" { seen = 1; next }
		seen && /^## / { exit }
		seen { print }
	' "$1"
}

# The race, and why this script now checks for it.
#
# This runs at release time and opens a PR that merges LATER, when its checks
# go green. Anything merged into main in that window lands under the very
# [Unreleased] heading this rename is about to consume — so a fix that shipped
# in the NEXT version gets filed under the one that just went out. It is not
# hypothetical: v0.1.4's fold PR sat open while two PRs merged, and rebasing it
# swept both of their entries into "## v0.1.4 — in detail".
#
# The tag is the ground truth for what shipped, so compare against it. When the
# tree matches the tag, the fold is unambiguous and auto-merge is safe. When it
# does not, the fold still happens — the heading genuinely does belong to this
# release — but auto-merge is withheld and the PR says what was added since, so
# a human splits it rather than a script guessing which entry belongs where.
# The caller passes GITHUB_REF_NAME, which already carries the leading "v"
# ("v0.1.4") — the headings this script writes read "## v0.1.4 — in detail".
# Normalising rather than prefixing, so a caller that passes a bare "0.1.4"
# still resolves the right tag instead of looking up "vv0.1.4".
tag="v${version#v}"

drifted=""
if git cat-file -e "${tag}:CHANGELOG.md" 2>/dev/null; then
	git show "${tag}:CHANGELOG.md" > /tmp/fold-tagged.md 2>/dev/null || true
	if [ -s /tmp/fold-tagged.md ] &&
		! diff -q <(unreleased_block /tmp/fold-tagged.md) \
			<(unreleased_block CHANGELOG.md) >/dev/null 2>&1; then
		drifted=$(diff <(unreleased_block /tmp/fold-tagged.md) \
			<(unreleased_block CHANGELOG.md) | grep '^>' | head -40 || true)
	fi
	rm -f /tmp/fold-tagged.md
fi

# The FIRST [Unreleased] occurrence is the hand-written section this
# mechanism owns. Later occurrences (if any) are prose references and must
# not be touched — hence the exact-line equality below.
if ! grep -qF "## [Unreleased]" CHANGELOG.md; then
	echo "fold-changelog: no [Unreleased] heading to fold; a release shipped " \
		"with no hand-written entries." >&2
else
# awk, not sed: the GNU-only "0,/re/" address for "first match only" is a
# silent no-op on BSD sed — measured on the exact bug it was meant to fix.
# Exact-line equality matters: later prose that MENTIONS the heading must
# not match.
#
# A FRESH "## [Unreleased]" is printed above the folded heading, and that is
# not cosmetic. Without it the file has no [Unreleased] section at all until
# someone recreates it by hand, and .github/workflows/changelog.yml fails
# every feat:/fix:/docs: PR for want of "an entry under ## [Unreleased]" —
# so the next contributor meets a red gate with no obvious cause.
	awk -v h="## ${version} — in detail" '
		!done && $0 == "## [Unreleased]" {
			print "## [Unreleased]"
			print ""
			print h
			done = 1
			next
		}
		{ print }
	' CHANGELOG.md > CHANGELOG.md.tmp
	mv CHANGELOG.md.tmp CHANGELOG.md
fi

# 2. Manifest repair. The manifest must say the version that just shipped;
# anything else is the staleness that made release-please re-propose a
# released version. Recorded as part of debt #76's repayment.
manifest=".release-please-manifest.json"
if [ -f "$manifest" ]; then
	want="{\".\": \"${version}\"}"
	if [ "$(cat "$manifest" | tr -d ' \n')" != "$(printf '%s' "$want" | tr -d ' \n')" ]; then
		printf '{\n  ".": "%s"\n}\n' "$version" > "$manifest"
		echo "fold-changelog: manifest recorded as ${version}."
	fi
fi

git add CHANGELOG.md
if ! git -c user.name="DeVaris Brown" -c user.email="devaris@devaris.com" \
	commit --quiet -s -m "docs: fold the hand-written changelog into ${version}"; then
	# An empty diff (the heading matched but the sed produced no change) is
	# not an error worth a PR.
	echo "fold-changelog: nothing to commit." >&2
	exit 0
fi

git push --quiet origin "$branch"
body=$(mktemp)
{
	printf 'Automated by the release pipeline (scripts/fold-changelog.sh): the\n'
	printf 'hand-written changelog section shipped in %s and this PR folds its\n' "$version"
	printf 'heading from [Unreleased] to the release, the manual step debt #76 named.\n'
	printf 'A fresh empty [Unreleased] is left above it so the next PR has somewhere\n'
	printf 'to write and the changelog gate keeps passing.\n'
	if [ -n "$drifted" ]; then
		printf '\n## Needs a human split — auto-merge withheld\n\n'
		printf 'The [Unreleased] section changed after `%s` was tagged, so some of\n' "$tag"
		printf 'what is about to be folded did NOT ship in this release. Move those\n'
		printf 'entries back under [Unreleased] before merging.\n\n'
		printf 'Added since the tag:\n\n```diff\n%s\n```\n' "$drifted"
	else
		printf '\nThe [Unreleased] section is unchanged since the tag, so everything\n'
		printf 'folded here shipped in %s. Auto-merges when checks are green.\n' "$version"
	fi
} > "$body"

pr=$(gh pr create --base main --head "$branch" \
	--title "docs: fold the hand-written changelog into ${version}" \
	--body-file "$body")
rm -f "$body"

if [ -n "$drifted" ]; then
	echo "fold-changelog: PR ${pr} opened WITHOUT auto-merge — entries were added"
	echo "fold-changelog: after the tag and a human must split them."
else
	# --auto is best-effort: when branch protection or repo settings refuse it,
	# the PR simply waits for a human merge — which is still the old manual step
	# minus the rename itself.
	gh pr merge "$pr" --auto --squash >/dev/null 2>&1 || true
	echo "fold-changelog: PR ${pr} opened and set to auto-merge."
fi

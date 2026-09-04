#!/usr/bin/env bash
# fold-drift-check.sh — refuse a changelog fold that captures entries which
# shipped AFTER the release it folds into.
#
# `scripts/fold-changelog.sh` renames the hand-written "## [Unreleased]" heading
# to "## vX.Y.Z — in detail" when X.Y.Z publishes. It compares the tree against
# the tag first and warns when they differ — but it does that ONCE, when it
# opens the PR, and the PR then waits for checks. Everything merged in that
# window lands under the very heading the rename is about to consume.
#
# That is not a hypothetical failure: it happened at v0.1.6, v0.1.7 and v0.2.0.
# Each time the fold PR was correct when opened and wrong by the time it could
# merge, and each time it was caught by a human reading the diff rather than by
# a gate. A warning printed into a PR body is a snapshot; this is a check.
#
# It runs on the MERGE RESULT. GitHub's pull_request event checks out the
# prospective merge commit, so this sees exactly the CHANGELOG that would land,
# including entries added to the base branch after the PR opened. That is the
# property that makes it a merge-time check rather than a second creation-time
# one.
#
# The rule: every entry under "## vX.Y.Z — in detail" must have been present
# under "## [Unreleased]" at tag vX.Y.Z. An entry that was not is one that
# shipped later and is being misfiled.
set -euo pipefail

cd "$(cd "$(dirname "$0")/.." && pwd)"

RED=$'\033[31m'
GREEN=$'\033[32m'
BLUE=$'\033[34m'
OFF=$'\033[0m'

# CHANGELOG is the file to CHECK; TRACKED is the path to read at the tag. They
# differ only under test, where a fixture stands in for the working tree — the
# tag always carries the real CHANGELOG.md.
CHANGELOG=${1:-CHANGELOG.md}
TRACKED=CHANGELOG.md

# section HEADING FILE — the lines under an exact heading, to the next "## ".
section() {
	awk -v want="$1" '
		!seen && $0 == want { seen = 1; next }
		seen && /^## / { exit }
		seen { print }
	' "$2"
}

# entries TEXT — the first line of each bullet, which identifies it. Bodies are
# reflowed by editors; the opening line is what stays stable.
entries() {
	grep -E '^- \*\*' || true
}

# The folded sections in the working tree, newest first. A file may carry many;
# only ones whose tag exists can be checked, and only the newest can have
# drifted — older ones were checked when they landed.
mapfile -t folded < <(grep -oE '^## v[0-9]+\.[0-9]+\.[0-9]+ — in detail$' "$CHANGELOG" || true)

if [ ${#folded[@]} -eq 0 ]; then
	printf '%s  OK  %s no changelog fold in this diff\n' "$GREEN" "$OFF"
	exit 0
fi

heading=${folded[0]}
version=$(printf '%s' "$heading" | sed -E 's/^## (v[0-9]+\.[0-9]+\.[0-9]+) — in detail$/\1/')

if ! git cat-file -e "${version}:${TRACKED}" 2>/dev/null; then
	# A fold for a tag that does not exist yet is the release pipeline's own
	# PR, opened moments before the tag lands. Nothing to compare against, and
	# refusing would block the mechanism this check protects.
	printf '%s PEND %s %s is not tagged yet; nothing to compare a fold against\n' \
		"$BLUE" "$OFF" "$version"
	exit 0
fi

tagged=$(mktemp)
git show "${version}:${TRACKED}" > "$tagged"

# What the fold claims shipped in this release, against what was actually
# pending when it was tagged.
claimed=$(section "$heading" "$CHANGELOG" | entries)
actual=$(section '## [Unreleased]' "$tagged" | entries)

misfiled=0
while IFS= read -r line; do
	[ -n "$line" ] || continue
	if ! printf '%s\n' "$actual" | grep -qxF -- "$line"; then
		if [ "$misfiled" -eq 0 ]; then
			printf '%s FAIL %s the %s fold captures entries that shipped after it:\n' \
				"$RED" "$OFF" "$version"
		fi
		printf '        %s\n' "${line:0:96}"
		misfiled=$((misfiled + 1))
	fi
done <<< "$claimed"

rm -f "$tagged"

if [ "$misfiled" -ne 0 ]; then
	printf '\n'
	printf '        Those entries were not under [Unreleased] when %s was tagged,\n' "$version"
	printf '        so they belong to a later release. This happens when a fold PR\n'
	printf '        waits behind other merges: it was correct when opened and is\n'
	printf '        wrong now. Move them back under [Unreleased] and re-push.\n'
	exit 1
fi

printf '%s  OK  %s every entry the %s fold captures was pending at its tag\n' \
	"$GREEN" "$OFF" "$version"

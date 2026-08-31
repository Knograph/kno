#!/usr/bin/env bash
# actions-pin-check.sh — every GitHub Action must be pinned to a commit SHA.
#
# docs/debt.md#14: every Go tool this repo builds with is byte-pinned by
# go.sum, while the actions that run in CI — including the job that holds
# `id-token: write` and signs the release — are pinned to `@v4`-style tags. A
# tag is a mutable pointer: whoever controls the upstream repository can move
# it, and Dependabot's bump would look identical to an attacker's.
#
# The rule, and it is the only rule: the ref after `@` must be a full
# 40-character commit SHA. A tag comment after it is encouraged, because that
# is what lets Dependabot keep bumping the pin:
#
#     uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
#
# Local actions (`./path`) have no ref to pin and are skipped: they are this
# repository's own code, already fixed by the commit under test.
#
# Not part of `make check`. It exits non-zero until every pin is a SHA; wiring
# it into the PR gate is the last commit of the work it describes, and that
# commit repays docs/debt.md#14.

set -euo pipefail

cd "$(cd "$(dirname "$0")/.." && pwd)"

RED=$'\033[31m'
GREEN=$'\033[32m'
OFF=$'\033[0m'

WORKFLOWS=.github/workflows

unpinned=""
unpinned_count=0
pinned_count=0
# repo<TAB>sha<TAB>comment for every pin already correct in this repo, so an
# offender can be told the answer when we already have it somewhere.
known_pins=""

while IFS=: read -r file lineno rest; do
	# The action spec is the first whitespace-delimited token after `uses:`,
	# minus any quoting. Anything after it on the line is a comment.
	spec=${rest#*uses:}
	spec=${spec#"${spec%%[![:space:]]*}"}
	spec=${spec%%[[:space:]]*}
	spec=${spec//\'/}
	spec=${spec//\"/}
	[ -n "$spec" ] || continue

	case $spec in
	./* | .\\* | docker://*)
		continue
		;;
	esac

	repo=${spec%@*}
	ref=${spec##*@}

	if [ "$spec" = "$ref" ]; then
		# No `@` at all: an implicit default branch, which is the most mutable
		# ref there is.
		ref="(no ref — implicit default branch)"
	elif printf '%s' "$ref" | grep -qE '^[0-9a-f]{40}$'; then
		pinned_count=$((pinned_count + 1))
		comment=$(printf '%s' "$rest" | sed -n 's/.*#[[:space:]]*\(.*\)$/\1/p')
		known_pins="${known_pins}${repo}	${ref}	${comment}
"
		continue
	fi

	unpinned_count=$((unpinned_count + 1))
	unpinned="${unpinned}${file}:${lineno}	${repo}	${ref}
"
done <<EOF
$(grep -nE '^[[:space:]]*(-[[:space:]]+)?uses:' "$WORKFLOWS"/*.yml || true)
EOF

if [ "$unpinned_count" -eq 0 ]; then
	printf '%s  OK  %s all %d GitHub Actions are pinned to commit SHAs\n' "$GREEN" "$OFF" "$pinned_count"
	exit 0
fi

printf '%s FAIL %s %d GitHub Action references are pinned to a mutable ref, not a commit SHA.\n' \
	"$RED" "$OFF" "$unpinned_count"
printf '\n'
printf '        docs/debt.md#14. %d references in this repo are already pinned\n' "$pinned_count"
printf '        correctly, so the defect is inconsistency: the same action is\n'
printf '        byte-pinned in one workflow and tag-pinned in the next.\n'
printf '\n'
printf '        What this is, stated plainly: a mechanical chore. Once this checker\n'
printf '        exists the remaining work is looking each SHA up and pasting it, and\n'
printf '        there is no design judgement left in it. It is a good FIRST pull\n'
printf '        request for what surrounds the diff rather than the diff itself —\n'
printf '        DCO sign-off (git commit -s), a Conventional Commit title, and one\n'
printf '        red-to-green CI run — on a change whose content cannot be got wrong.\n'
printf '        It is not a design contribution and is not offered as one.\n'
printf '\n'
printf '        Fix each line below by replacing the ref with the tag'"'"'s full\n'
printf '        40-character commit SHA, keeping the tag as a trailing comment so\n'
printf '        Dependabot can still bump it:\n'
printf '\n'
printf '            uses: actions/checkout@<40-hex-sha> # v7.0.1\n'
printf '\n'
printf '        Look a SHA up with:\n'
printf '\n'
printf '            gh api repos/<owner>/<repo>/git/ref/tags/<tag> --jq .object.sha\n'
printf '\n'
printf '        If that returns an ANNOTATED tag rather than a commit, dereference it\n'
printf '        — pinning the tag object instead of the commit does not work:\n'
printf '\n'
printf '            gh api repos/<owner>/<repo>/git/tags/<sha> --jq .object.sha\n'
printf '\n'
printf '        Pin to the SHA of a release you have read, not whatever the tag points\n'
printf '        at today, and say in the PR body which version each SHA is.\n'
printf '\n'

printf '%s' "$unpinned" | while IFS=$'\t' read -r where repo ref; do
	[ -n "$where" ] || continue
	printf '        %-40s %s@%s\n' "$where" "$repo" "$ref"
	hint=$(printf '%s' "$known_pins" | awk -F'\t' -v r="$repo" '$1 == r { print $2 "\t" $3; exit }')
	if [ -n "$hint" ]; then
		sha=${hint%%	*}
		comment=${hint#*	}
		if [ -n "$comment" ]; then
			printf '        %-40s already pinned elsewhere in this repo: %s # %s\n' "" "$sha" "$comment"
		else
			printf '        %-40s already pinned elsewhere in this repo: %s\n' "" "$sha"
		fi
	fi
done

printf '\n'
printf '        The last of these PRs adds this target to "make check" and repays\n'
printf '        docs/debt.md#14. Until then it is expected to be red when run, and it\n'
printf '        is deliberately not wired into CI so that redness is on demand.\n'
exit 1

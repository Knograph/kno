#!/usr/bin/env bash
# fixture-consistency.sh — the demo fixture spells one thing, everywhere.
#
# WHY THIS EXISTS, AND WHY IT IS EMBARRASSING THAT IT DID NOT
#
# The support-refunds fixture is one worked example living in four copies:
# `cli/demodata/` (so `kno demo` works on a plane), `tapes/quickstart.tape` (a
# VHS tape records a terminal and cannot read a file the viewer will not see),
# this README's quickstart (the front door must not depend on another
# repository being reachable), and
# `uknoAI/kno-examples/scenarios/support-refunds`.
#
# The disagreement between two of those is the founding story of kno-examples,
# quoted in its README and in its `internal/fixture` package doc: this README
# said refunds are "processed within 5 business days" while
# `tapes/quickstart.tape` said "issued", and nothing could tell you which was
# right.
#
# A detector was then built — and it compares `cli/demodata/`, the tape, and
# the scenario. Not the README. So the file that the story is ABOUT was the one
# copy left unwatched, and it was still wrong: step 1 of the quickstart wrote a
# Case expecting "issued", and step 3 told the reader to add an Asset saying
# "processed". A worked example whose candidate Asset contradicts the answer it
# is supposed to supply.
#
# WHAT THIS CHECKS
#
# Not equality with kno-examples — that would need its checkout and would
# redden on its schedule rather than ours. The weaker, local, sufficient
# property: every mention on a surface a reader copies spells the sentence the
# same way. One spelling cannot drift from itself, and changing the canonical
# wording becomes a one-line edit here plus a red build, which is the right
# amount of friction for changing a fixture in four places.
set -euo pipefail

cd "$(cd "$(dirname "$0")/.." && pwd)"

CANONICAL="Refunds are issued within 5 business days."
PATTERN="Refunds are [a-z]* within 5 business days\."

# Surfaces a reader copies. `docs/plans/` is excluded deliberately: those
# documents QUOTE the drift as history, and a check that forbade writing the
# wrong spelling would forbid explaining what went wrong. Excluding them is a
# real hole, and it is the correct one — the alternative is a codebase that
# cannot describe its own bugs.
SURFACES=(README.md cli/demodata tapes docs)

RED=$'\033[31m'
GREEN=$'\033[32m'
OFF=$'\033[0m'

# A `while read` loop rather than `mapfile`: mapfile is bash 4+, and macOS
# ships bash 3.2, so `#!/usr/bin/env bash` on a developer's Mac — and on the
# macos-latest CI runner — resolves to a shell that does not have it. This
# script is a gate against "it worked in the place I ran it", so it does not
# get to be an instance of that.
hits=()
while IFS= read -r line; do
	[ -n "$line" ] && hits+=("$line")
done < <(grep -rnE "$PATTERN" "${SURFACES[@]}" 2>/dev/null | grep -v '^docs/plans/' || true)

if [ "${#hits[@]}" -eq 0 ]; then
	printf '%s FAIL %s fixture-consistency: the refund fixture appears on no reader-facing surface.\n' "$RED" "$OFF" >&2
	printf '  If the quickstart legitimately stopped using it, delete this check in the same commit.\n' >&2
	printf '  A gate that silently matches nothing is worse than no gate.\n' >&2
	exit 1
fi

broken=0
for hit in "${hits[@]}"; do
	sentence=$(printf '%s' "$hit" | grep -oE "$PATTERN" | head -1)
	if [ "$sentence" != "$CANONICAL" ]; then
		printf '%s FAIL %s fixture-consistency: %s\n' "$RED" "$OFF" "$hit" >&2
		printf '  says   %s\n' "$sentence" >&2
		printf '  wanted %s\n' "$CANONICAL" >&2
		broken=1
	fi
done

if [ "$broken" -ne 0 ]; then
	printf '\nOne worked example, four copies. cli/demodata, tapes/quickstart.tape and\n' >&2
	printf 'kno-examples are compared nightly by that repository; this check is what\n' >&2
	printf 'covers the README, which is the copy the original drift was in.\n' >&2
	exit 1
fi

printf '%s  OK  %s %d mentions of the demo fixture, one spelling\n' "$GREEN" "$OFF" "${#hits[@]}"

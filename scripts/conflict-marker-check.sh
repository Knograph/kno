#!/usr/bin/env bash
# conflict-marker-check.sh — refuse a tracked file containing merge-conflict
# markers.
#
# This exists because markers reached `main`. A scripted rebase resolved
# docs/debt.md with a parser and then `git add`ed EVERY unmerged path,
# including a CHANGELOG.md whose markers were still in it. Nothing caught it:
# the compiler never sees a .md, the changelog gate only asks whether an entry
# exists under [Unreleased], and gitleaks looks for credentials. So a file
# reading "<<<<<<< HEAD" shipped, and the next release would have folded it
# into a published changelog.
#
# Markers are cheap to detect and impossible to intend. Checking git's own
# file list rather than walking the tree keeps agent worktrees and node_modules
# out of it for free — the same scoping mistake `make docs` made.
set -euo pipefail

cd "$(cd "$(dirname "$0")/.." && pwd)"

RED=$'\033[31m'
GREEN=$'\033[32m'
OFF=$'\033[0m'

# Anchored at column zero and matched as fixed strings: prose ABOUT a conflict
# marker (this file, a runbook, a postmortem) is legitimate and indented or
# quoted. The three-way "|||||||" form counts too.
found=0
while IFS= read -r f; do
	[ -f "$f" ] || continue
	if LC_ALL=C grep -nE '^(<{7}|={7}|>{7}|\|{7})( |$)' "$f" >/dev/null 2>&1; then
		printf '%s FAIL %s conflict markers in %s:\n' "$RED" "$OFF" "$f"
		LC_ALL=C grep -nE '^(<{7}|={7}|>{7}|\|{7})( |$)' "$f" | sed 's/^/        /'
		found=1
	fi
done < <(git ls-files)

if [ "$found" -ne 0 ]; then
	printf '        A conflict marker is never intended. Resolve the file and\n'
	printf '        re-stage it; a scripted rebase that `git add`s every unmerged\n'
	printf '        path is how these reach a branch.\n'
	exit 1
fi

printf '%s  OK  %s no conflict markers in tracked files\n' "$GREEN" "$OFF"

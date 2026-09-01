#!/usr/bin/env bash
# selftest.sh — prove that each quality gate FAILS when its invariant is broken.
#
# docs/debt.md#16: every gate in this repo is trusted on the strength of a
# one-off manual check, and nothing verifies that a gate still fails when it
# should. The .SHELLFLAGS defect is the concrete precedent — eleven recipes
# reported success while the commands inside them failed, and `make check` was
# green throughout. A gate nobody has watched fail is a gate nobody knows works.
#
# The shape of a case, and it is the whole idea:
#
#   1. break the invariant the gate exists to protect — in a scratch file, or
#      in a copy restored by a trap. Never leave the tree dirty.
#   2. run the gate, require a NON-ZERO exit, and require the output to name
#      the break. A gate that failed for an unrelated reason must not read as
#      a pass; that is the whole .SHELLFLAGS lesson.
#   3. restore the tree, run the gate again, and require a ZERO exit. A gate
#      that is simply always red must not read as a pass either — that is
#      docs/debt.md#70's failure mode.
#
# Two gates are covered below as the worked pattern. The rest are listed in
# UNCOVERED and this script exits non-zero until that list is empty. It is
# deliberately NOT part of `make check`: wiring it into the PR gate is the last
# commit of the last case, not the first.

set -euo pipefail

cd "$(cd "$(dirname "$0")/.." && pwd)"

MAKE=${MAKE:-make}

RED=$'\033[31m'
GREEN=$'\033[32m'
BLUE=$'\033[34m'
OFF=$'\033[0m'

PASSED=0
FAILED=0

# GATE_STATUS and GATE_OUTPUT hold the last gate run. Globals rather than a
# return value because the callers need both the exit status and the text.
GATE_STATUS=0
GATE_OUTPUT=""

# run_gate <gate> [VAR=value ...] — run `make <gate>`, capturing combined output.
run_gate() {
	local gate=$1
	shift
	set +e
	GATE_OUTPUT=$("$MAKE" --no-print-directory "$gate" "$@" 2>&1)
	GATE_STATUS=$?
	set -e
}

ok() {
	PASSED=$((PASSED + 1))
	printf '%s  OK  %s selftest/%s: %s\n' "$GREEN" "$OFF" "$1" "$2"
}

bad() {
	FAILED=$((FAILED + 1))
	printf '%s FAIL %s selftest/%s: %s\n' "$RED" "$OFF" "$1" "$2"
	printf '        the gate ran, so the gate is not the thing that is broken here:\n'
	printf '        either the invariant moved or the case below it is stale.\n'
}

# expect_break_caught <gate> <label> <substring> [VAR=value ...]
#
# The invariant must already be broken when this is called. Requires a
# non-zero exit AND an output naming the break.
expect_break_caught() {
	local gate=$1 label=$2 want=$3
	shift 3
	run_gate "$gate" "$@"
	if [ "$GATE_STATUS" -eq 0 ]; then
		bad "$gate" "$label was broken and \`make $gate\` still exited 0"
		return 0
	fi
	case $GATE_OUTPUT in
	*"$want"*) ok "$gate" "$label breaks it, and it says so ($want)" ;;
	*)
		bad "$gate" "exited $GATE_STATUS with $label broken, but never said '$want' — it failed for some other reason"
		printf '%s\n' "$GATE_OUTPUT" | sed 's/^/        | /'
		;;
	esac
}

# expect_intact_passes <gate> <label> [VAR=value ...]
expect_intact_passes() {
	local gate=$1 label=$2
	shift 2
	run_gate "$gate" "$@"
	if [ "$GATE_STATUS" -ne 0 ]; then
		bad "$gate" "$label is intact and \`make $gate\` still exited $GATE_STATUS"
		printf '%s\n' "$GATE_OUTPUT" | sed 's/^/        | /'
		return 0
	fi
	ok "$gate" "$label intact, exits 0"
}

# expect_output_lacks <gate> <label> <substring>
#
# Reads the output of the LAST run_gate. Used where the defect is a line that
# should not be there at all, rather than an exit status.
expect_output_lacks() {
	local gate=$1 label=$2 unwanted=$3
	case $GATE_OUTPUT in
	*"$unwanted"*)
		bad "$gate" "$label: its output still says '$unwanted'"
		;;
	*) ok "$gate" "$label: its output never says '$unwanted'" ;;
	esac
}

## ─── Covered gates ──────────────────────────────────────────────────────────

# The probe is a real markdown file in the tree, because that is the only thing
# the link checker looks at. It is removed by the EXIT trap, and refused up
# front if a killed run left one behind — a stray probe would fail `make docs`
# for reasons nobody could see.
DOCS_PROBE=.selftest-probe.md

cleanup() { rm -f "$DOCS_PROBE"; }
trap cleanup EXIT INT TERM

case_docs() {
	if [ -e "$DOCS_PROBE" ]; then
		bad docs "$DOCS_PROBE already exists — a previous run was killed. Delete it and rerun."
		return 0
	fi
	printf '# selftest probe\n\n[a link to nothing](./no-such-file.md)\n' >"$DOCS_PROBE"
	expect_break_caught docs "an unresolvable relative link" "broken link in"
	cleanup
	expect_intact_passes docs "every link"
}

# ledger-check has two invariants, and the second one is the bug this script
# shipped alongside: the recipe used to print ".goreleaser.yaml is valid" on a
# pass, so a green ledger gate announced a gate that had not run.
case_ledger_check() {
	# Both variables are passed empty rather than unset: make exports
	# command-line variables to its recipes, so this drives the guard whatever
	# the ambient environment holds — including a CI job where GITHUB_REF_NAME
	# is always set.
	expect_break_caught ledger-check "no version to check against" \
		"needs the version being released" VERSION= GITHUB_REF_NAME=

	# A version string no ledger trigger can name, so the pass path is exercised
	# without depending on which entries happen to be open today.
	expect_intact_passes ledger-check "a release nothing names" VERSION=0.0.0-selftest
	expect_output_lacks ledger-check "a passing run names the ledger" ".goreleaser.yaml"
}

# The gates a case exists for. Kept next to the calls so the count in the
# summary cannot drift from what actually ran.
COVERED="docs, ledger-check"

case_docs
case_ledger_check

## ─── Uncovered gates ────────────────────────────────────────────────────────

# gate<TAB>the invariant a case must break
#
# Delete a row when its case lands. Each row is one issue's worth of work; they
# are independent, so several people can take one each.
UNCOVERED=$(
	cat <<-'ROWS'
		fmt-check|a .go file gofumpt would reformat
		lint|an unchecked error return, or a naked TODO with no ledger reference
		test|a test that fails, and a test binary that panics
		test-integration|KNO_LIVE_TESTS set, which must refuse to run at all
		coverage-check|a package below its floor, and a package below its baseline
		secrets-scan|a planted fake credential in the working tree
		typecheck-proto|a breaking proto change against main
		generate-check|a hand-edit to a file under gen/
		vuln|a dependency with a known advisory
		fuzz-short|a seed corpus entry the parser cannot survive
		bench-diff|a benchmark existing at all, which must trip the tripwire
		release-check|an invalid .goreleaser.yaml
		release-identity-check|the cosign identity changed in one of the four files that quote it
		release-stamp|a binary whose version stamp does not match the tag
		pr-ready|a feat: branch with no CHANGELOG entry
		fold-changelog|an [Unreleased] block that changed since the tag, which must refuse to auto-merge
		test-live|KNO_MAX_COST_USD unset, which must refuse to spend
		record-fixtures|KNO_MAX_COST_USD unset, which must refuse to spend
	ROWS
)

REMAINING=$(printf '%s\n' "$UNCOVERED" | grep -c '|')

printf '\n'
if [ "$FAILED" -ne 0 ]; then
	printf '%s FAIL %s selftest: %d of %d covered cases did not hold.\n' "$RED" "$OFF" "$FAILED" "$((PASSED + FAILED))"
	printf '        A covered case failing means a gate stopped catching what it was\n'
	printf '        written to catch. Fix the gate, not the case.\n'
	exit 1
fi

printf '%s PEND %s selftest: %d gates have never been seen to fail.\n' "$BLUE" "$OFF" "$REMAINING"
printf '\n'
printf '        %s are covered — %d assertions, all holding. docs/debt.md#16 is\n' "$COVERED" "$PASSED"
printf '        why the rest matter: a gate nobody has watched fail is a gate nobody\n'
printf '        knows works.\n'
printf '\n'
printf '        To cover one — this is the whole task, and it is one gate per PR:\n'
printf '\n'
printf '          1. read case_docs in scripts/selftest.sh. It is 6 lines and it is\n'
printf '             the pattern: break, assert non-zero AND the right message,\n'
printf '             restore, assert zero.\n'
printf '          2. write case_<gate> for one row below, using expect_break_caught\n'
printf '             and expect_intact_passes.\n'
printf '          3. break the invariant in a scratch file or a trap-restored copy.\n'
printf '             A case that can leave the tree dirty is not finished.\n'
printf '          4. call it next to case_docs, delete its row from UNCOVERED, and\n'
printf '             check that this line counts one lower.\n'
printf '\n'
printf '        When the list is empty, the last PR wires selftest into "make check"\n'
printf '        and repays docs/debt.md#16. Not before: a gate that is red for\n'
printf '        bookkeeping reasons is a gate people learn to ignore.\n'
printf '\n'
printf '%s\n' "$UNCOVERED" | while IFS='|' read -r gate invariant; do
	[ -n "$gate" ] || continue
	printf '          %-24s break: %s\n' "$gate" "$invariant"
done
printf '\n'
exit 1

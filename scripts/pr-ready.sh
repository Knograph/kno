#!/usr/bin/env bash
# pr-ready.sh — the changelog gate, run BEFORE pushing instead of after.
#
# Mirrors .github/workflows/changelog.yml's "A behavior change documents
# itself" check locally: the check fires on the PR, where a red X costs a CI
# cycle and a human's attention; this fires on the branch, where the fix is
# one commit or one label at creation time.
#
# Rules, identical to the workflow:
#   - commit title type refactor|chore|test|build  -> pass (no changelog owed)
#   - CHANGELOG.md changed vs origin/main          -> pass
#   - otherwise FAIL, printing both remedies:
#       1. add an entry under "## [Unreleased]" in CHANGELOG.md, or
#       2. create the PR with --label no-changelog (a reviewer should agree
#          the PR changes nothing a user would notice).
#
# Usage: make pr-ready   (from the PR branch)
set -euo pipefail

base="${1:-origin/main}"

if ! git rev-parse --verify "$base" >/dev/null 2>&1; then
  echo "pr-ready: no ref $base — fetch first (git fetch origin main)" >&2
  exit 2
fi

title="$(git log -1 --format=%s "$base"..HEAD | head -1)"
type="$(printf '%s' "$title" | grep -oE '^[a-z]+' || true)"

case "$type" in
  refactor|chore|test|build)
    echo "pr-ready: OK — $type: does not change behavior a user can see"
    exit 0
    ;;
  "")
    echo "pr-ready: FAIL — cannot read a Conventional Commit type from the head commit title:" >&2
    echo "  $title" >&2
    exit 1
    ;;
esac

if git diff --quiet "$base"...HEAD -- CHANGELOG.md; then
  echo "pr-ready: FAIL — a '$type:' branch needs a CHANGELOG entry under ## [Unreleased]," >&2
  echo "  or the PR must carry the no-changelog label at creation:" >&2
  echo "    gh pr create --label no-changelog ..." >&2
  echo "  (legitimate only when a reviewer would agree nothing user-visible changes)" >&2
  exit 1
fi

echo "pr-ready: OK — CHANGELOG.md is in the diff"

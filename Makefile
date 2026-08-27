# Kno — quality gates.
#
# `make check` is the PR gate: everything CI runs on a pull request, ordered
# fail-fast cheapest-first so that running it locally is cheap enough that
# people actually do. If `make check` is green, CI should be green.
#
# Tools are pinned in tools/go.mod via Go's native `tool` directive and
# installed into ./bin, so contributors and CI run byte-identical versions.
# See docs/plans/2026-08-18-repo-foundation.md.

SHELL := /usr/bin/env bash

# .SHELLFLAGS requires GNU Make >= 3.82. macOS still ships 3.81, which IGNORES
# it silently — so every recipe there ran under a plain `-c` with no `set -e`
# and no `pipefail`. A recipe line that failed mid-way then reported the exit
# status of its LAST command, which is how `buf breaking` could print
# violations and still be followed by "OK ... clean".
#
# It is set for newer make, but nothing may depend on it: every multi-command
# recipe below starts with an explicit `set -euo pipefail` so the safety is a
# property of the recipe, not of the make version that happens to be installed.
.SHELLFLAGS := -euo pipefail -c

# Prefix for any recipe line that chains commands. Not optional.
SAFE := set -euo pipefail;
.DEFAULT_GOAL := check

# The gate ordering IS the design (cheapest failure first). Parallel make would
# destroy it, so refuse to run recipes in parallel.
.NOTPARALLEL:

BIN        := $(CURDIR)/bin

# Pin the Go toolchain, not just the tools.
#
# GOTOOLCHAIN=auto resolves the version per invocation, and with a mix of
# module go directives it can end up compiling packages with one toolchain
# while running the compile binary from another — producing
# `compile: version "go1.25.8" does not match go tool version "go1.25.5"`
# across the standard library. Pinning makes the toolchain as reproducible as
# everything else in tools/go.mod. Bump it deliberately, in a PR.
#
# The version is READ FROM go.mod's toolchain directive rather than duplicated
# here, so there is one source of truth. setup-go reads the same line, which is
# what stops CI from installing one toolchain and then downloading a second.
#
# It must satisfy the highest floor across both modules: go.mod requires
# >= 1.25.8 (GO-2026-4602 in the standard library) and tools/go.mod requires
# >= 1.25.10 (buf). Pinning below either one fails at `make tools`.
export GOTOOLCHAIN := $(shell awk '/^toolchain /{print $$2}' go.mod)

# Put the pinned tools ahead of everything on PATH for every recipe.
#
# buf resolves `local:` codegen plugins by name from $PATH, so without this it
# silently picks up whatever protoc-gen-go happens to be in a developer's
# ~/go/bin — or finds none at all in CI. That is the exact class of bug the
# tools/ module exists to prevent: a green local run that quietly depends on
# the machine it ran on.
export PATH := $(CURDIR)/bin:$(PATH)
TOOLS_MOD  := $(CURDIR)/tools
COVERAGE   := coverage.out
BASELINE   := .coverage-baseline
TOOLSTAMP  := $(BIN)/.tools-stamp

GOLANGCI   := $(BIN)/golangci-lint
GOFUMPT    := $(BIN)/gofumpt
GOVULNCHK  := $(BIN)/govulncheck
BUF        := $(BIN)/buf

# -race and -shuffle=on are mandatory per CLAUDE.md. `:=` not `?=` on purpose:
# with `?=`, `GOTESTFLAGS= make test` silently drops the race detector, which
# is precisely the "must not be removed to make a test pass" failure the rule
# exists to prevent. Pass extra flags via GOTESTEXTRA instead.
GOTESTFLAGS := -race -shuffle=on
GOTESTEXTRA ?=

# 30s in the PR gate; nightly overrides to a longer run.
# Measured in EXECUTIONS, not wall-clock.
#
# `-fuzztime=30s` failed intermittently on both CI runners with "context
# deadline exceeded" — the fuzzing coordinator timing out on a worker as the
# deadline lands, not a failing input. A gate that fails for reasons unrelated
# to the code is a gate that gets deleted.
#
# A count also makes the gate do the same WORK everywhere. Under a time budget
# a fast laptop explores several times more than a loaded CI runner, so "passes
# locally" and "passes in CI" meant different things.
FUZZTIME ?= 300000x

# Set in CI. Turns every "tool missing" warning into a hard failure, so an
# absent binary can never silently disable a gate where it counts.
KNO_CI ?=

# Two distinct kinds of "this gate did not run", deliberately labeled
# differently because they mean opposite things:
#
#   SKIP — a tool is missing on THIS machine. The gate is real and CI runs it.
#          Locally it is a warning; under KNO_CI it is a hard failure.
#   PEND — the gate's implementation lands in a later milestone. Nothing is
#          hidden; there is nothing to run yet. Each one names the milestone
#          that retires it, and each is pinned to a one-time token rather than
#          a re-evaluable condition (Phase-1 finding A8).
#
# skip_missing <tool> <gate name>
define skip_missing
	if [ -n "$(KNO_CI)" ]; then \
		printf '\033[31m FAIL \033[0m %s: %s not installed, and KNO_CI is set. Gates must never skip in CI.\n' "$(2)" "$(1)"; \
		exit 1; \
	else \
		printf '\033[33m SKIP \033[0m %s: %s not installed. This gate did NOT run.\n' "$(2)" "$(1)"; \
	fi
endef

# pending <gate name> <milestone>
define pending
	printf '\033[34m PEND \033[0m %s: implementation lands in %s.\n' "$(1)" "$(2)"
endef

# live_spend_guard <target name>
#
# Every target that sets KNO_LIVE_TESTS=1 must call this FIRST: KNO_MAX_COST_USD
# must be set, so a live run always has a stated ceiling.
#
# This lived inline in test-live only, which is how record-fixtures came to set
# KNO_LIVE_TESTS=1 itself while passing neither check. A guard that one target
# implements privately is a guard the next target forgets.
#
# It used to carry a SECOND check: a grep for the string KNO_MAX_COST_USD in any
# .go file, standing in for "some code enforces this". That interlock is gone,
# deleted in the same change that made it unnecessary — docs/debt.md#11 required
# both in one PR precisely so neither could land alone. The cap is now read by
# adapters/agent/openaicompat/live_test.go, which parses it into a budget.Guard
# and authorizes every live call against it, so a call that would breach the
# ceiling is denied rather than merely counted afterwards.
#
# Deleting the grep rather than keeping it as belt-and-braces is deliberate. It
# matched a COMMENT as readily as an enforcement, so once real enforcement
# existed the check could only mislead: it would keep passing after someone
# deleted the guard and left the mention behind, which is the exact failure mode
# entry 11 records. A proxy that outlives the thing it proxies for reads as
# green while protecting nothing.
define live_spend_guard
	if [ -z "$${KNO_MAX_COST_USD:-}" ]; then \
		printf '\033[31m FAIL \033[0m %s: refusing to run without KNO_MAX_COST_USD set.\n' "$(1)"; \
		printf '        A live run has no ceiling unless one is stated. See docs/debt.md#11.\n'; \
		exit 1; \
	fi
endef

.PHONY: check
check: fmt-check lint test typecheck-proto generate-check vuln docs fuzz-short bench-diff ## The PR gate: everything CI runs on a pull request
	@printf '\033[32m  OK  \033[0m all gates passed\n'

## ─── Tools ──────────────────────────────────────────────────────────────────

# A stamp file, not a .PHONY prerequisite: `tools` being phony made every
# target depending on it reinstall all five tools on every invocation, and made
# the CI tool cache inert.
$(TOOLSTAMP): $(TOOLS_MOD)/go.mod $(TOOLS_MOD)/go.sum
	@mkdir -p $(BIN)
	@GOBIN=$(BIN) go -C $(TOOLS_MOD) install tool
	@touch $@
	@printf '\033[32m  OK  \033[0m tools installed to ./bin\n'

$(GOLANGCI) $(GOFUMPT) $(GOVULNCHK) $(BUF): $(TOOLSTAMP)

.PHONY: tools
tools: $(TOOLSTAMP) ## Install pinned build tools into ./bin

## ─── Format & lint ──────────────────────────────────────────────────────────

.PHONY: fmt
fmt: $(GOLANGCI) ## Apply gofumpt formatting
	@$(GOLANGCI) fmt

.PHONY: fmt-check
fmt-check: $(GOLANGCI) ## Verify formatting without writing (CI-safe)
	@$(GOLANGCI) fmt --diff

.PHONY: lint
lint: $(GOLANGCI) ## golangci-lint; zero tolerance
	@$(GOLANGCI) run
	@$(MAKE) --no-print-directory lint-shell

.PHONY: lint-shell
lint-shell: ## shellcheck install.sh, the one artifact users pipe to a shell
	@$(SAFE) if ! command -v shellcheck >/dev/null 2>&1; then \
		printf '\033[33m WARN \033[0m shellcheck not installed; skipping install.sh.\n'; \
		printf '        brew install shellcheck (CI runs it either way).\n'; \
		exit 0; \
	fi; \
	shellcheck -s sh install.sh; \
	printf '\033[32m  OK  \033[0m install.sh\n'

.PHONY: lint-config
lint-config: $(GOLANGCI) ## Validate .golangci.yml against the v2 schema
	@$(GOLANGCI) config verify

## ─── Test & coverage ────────────────────────────────────────────────────────

.PHONY: test
test: ## Unit tests (-race, -shuffle=on) + fixture integration + coverage + secrets
	@go test $(GOTESTFLAGS) $(GOTESTEXTRA) -coverprofile=$(COVERAGE) -covermode=atomic ./...
	@$(MAKE) --no-print-directory test-integration
	@$(MAKE) --no-print-directory coverage-check
	@$(MAKE) --no-print-directory secrets-scan

.PHONY: test-integration
test-integration: ## Adapter tests against RECORDED FIXTURES; never live APIs
	@$(SAFE) if [ -n "$${KNO_LIVE_TESTS:-}" ]; then \
		printf '\033[31m FAIL \033[0m test-integration: KNO_LIVE_TESTS is set. This target is the fixture path and must never spend money. Use `make test-live`.\n'; \
		exit 1; \
	fi
	@go test $(GOTESTFLAGS) $(GOTESTEXTRA) -tags=integration ./...

# Separate target, never reachable from `check` or `test`, so a live run is
# always an explicit choice. Nightly CI calls this one.
.PHONY: test-live
test-live: ## Integration tests against LIVE providers. Spends real money.
	@$(SAFE) $(call live_spend_guard,test-live)
	@KNO_LIVE_TESTS=1 go test $(GOTESTFLAGS) -tags=integration ./...

.PHONY: coverage-check
coverage-check: ## Enforce coverage floors and the no-decrease ratchet
	@go run ./internal/cmd/covercheck -profile=$(COVERAGE) -baseline=$(BASELINE)

# The baseline must be the LOWEST reading across the platforms CI runs, not
# whichever one the maintainer happens to be sitting at. Coverage of concurrent
# code is platform-dependent -- executor measures 96.0% on darwin and 94.9% on
# linux for the same commit, every test passing on both -- so a baseline written
# on darwin fails linux forever, and a gate that always fails gets disabled.
.PHONY: update-coverage-baseline
update-coverage-baseline: test ## Regenerate .coverage-baseline (linux only; review the diff like code)
	@$(SAFE) if [ "$$(go env GOOS)" != "linux" ]; then \
		printf '\033[31m FAIL \033[0m update-coverage-baseline: refusing to write a baseline on %s.\n' "$$(go env GOOS)"; \
		printf '        Coverage of concurrent code is platform-dependent, and this file is the\n'; \
		printf '        floor across every platform CI runs. Writing it here would raise the floor\n'; \
		printf '        above what the linux job can meet, failing CI on every later PR.\n'; \
		printf '        Take the numbers from a CI run, or run this in a linux container.\n'; \
		exit 1; \
	fi
	@go run ./internal/cmd/covercheck -profile=$(COVERAGE) -baseline=$(BASELINE) -write

.PHONY: secrets-scan
secrets-scan: ## gitleaks over BOTH the working tree and git history
	@$(SAFE) if ! command -v gitleaks >/dev/null 2>&1; then \
		$(call skip_missing,gitleaks,secrets scan); \
	else \
		gitleaks dir . --no-banner --redact --exit-code 1; \
		gitleaks git . --no-banner --redact --exit-code 1; \
		printf '\033[32m  OK  \033[0m secrets scan: working tree and history clean\n'; \
	fi

# Recording calls real providers, so it spends real money — the same as
# test-live, and it must pass the same guard. It previously set
# KNO_LIVE_TESTS=1 itself and checked neither condition, which armed a live
# spend path the moment anyone wrote the first TestRecord.
.PHONY: record-fixtures
record-fixtures: ## Re-record adapter fixtures against LIVE providers. Spends real money.
	@$(SAFE) $(call live_spend_guard,record-fixtures)
	@KNO_RECORD_FIXTURES=1 KNO_LIVE_TESTS=1 go test -tags=integration -run TestRecord ./adapters/...

.PHONY: update-golden
update-golden: ## Regenerate golden files (review the diff like code)
	@go test ./... -update

## ─── Proto ──────────────────────────────────────────────────────────────────

# proto/PENDING is a one-time bootstrap token that M0b deletes — NOT a
# directory-presence heuristic (Phase-1 finding A8: that shape would silently
# re-disable breaking-change protection if proto/ were ever renamed or
# emptied). If any .proto exists while the token remains, this hard-fails.
# NOTE: this expands INSIDE a single `if` in each recipe below. It cannot be a
# standalone recipe line — make runs each line in its own shell, so an `exit`
# here would end only that line and the gate would run anyway.
define proto_bootstrap_check
		if find proto -name '*.proto' -print -quit 2>/dev/null | grep -q .; then \
			printf '\033[31m FAIL \033[0m .proto files exist but proto/PENDING is still present.\n'; \
			printf '        Delete proto/PENDING — the proto gates are being skipped.\n'; \
			exit 1; \
		fi; \
		$(call pending,$(1),M0b (feat/proto-contract))
endef

.PHONY: generate
generate: $(BUF) ## Regenerate code from proto (output is checked in)
	@$(SAFE) if [ -f proto/PENDING ]; then \
$(call proto_bootstrap_check,generate); \
	else \
		$(BUF) generate; \
	fi

.PHONY: typecheck-proto
# The breaking-change baseline is resolved inside the recipe rather than in a
# make variable: '#' begins a comment in make's own syntax, so a `$(shell ...)`
# containing '.git#ref=...' truncates mid-call. Recipe lines pass '#' through
# to the shell untouched.
typecheck-proto: $(BUF) ## buf lint + buf breaking against main
	@$(SAFE) if [ -f proto/PENDING ]; then \
$(call proto_bootstrap_check,typecheck-proto); \
	else \
		$(BUF) lint; \
		if git rev-parse --verify --quiet origin/main >/dev/null 2>&1; then \
			baseref=origin/main; baseline='.git#ref=origin/main'; \
		else \
			baseref=main; baseline='.git#branch=main'; \
		fi; \
		if [ -z "$$(git log "$$baseref" --oneline -- '*.proto' 2>/dev/null | head -1)" ]; then \
			printf '\033[34m PEND \033[0m buf breaking: %s has never contained a .proto file.\n' "$$baseref"; \
			printf '        This is the commit that establishes the baseline. buf errors on an\n'; \
			printf '        empty baseline rather than reporting zero changes, so there is\n'; \
			printf '        nothing to compare against yet.\n'; \
			printf '        Self-retiring: the moment %s contains a .proto, this branch is\n' "$$baseref"; \
			printf '        unreachable forever. It keys on the HISTORY of the baseline, not on\n'; \
			printf '        the presence of a directory, so emptying or renaming proto/ later\n'; \
			printf '        cannot resurrect it.\n'; \
		else \
			$(BUF) breaking --against "$$baseline"; \
			printf '\033[32m  OK  \033[0m proto lint + breaking clean\n'; \
		fi; \
	fi

.PHONY: generate-check
generate-check: $(BUF) ## Fail if regenerating from proto would change anything
	@$(SAFE) if [ -f proto/PENDING ]; then \
		$(call pending,generate-check,M0b (feat/proto-contract)); \
	else \
		before=$$(find gen -type f -print0 2>/dev/null | sort -z | xargs -0 shasum -a 256 2>/dev/null | shasum -a 256); \
		$(BUF) generate; \
		after=$$(find gen -type f -print0 2>/dev/null | sort -z | xargs -0 shasum -a 256 2>/dev/null | shasum -a 256); \
		if [ "$$before" != "$$after" ]; then \
			printf '\033[31m FAIL \033[0m gen/ is stale — regenerating changed it. Commit the result.\n'; \
			git status --porcelain -- gen/; \
			exit 1; \
		fi; \
		printf '\033[32m  OK  \033[0m gen/ matches proto/\n'; \
	fi

## ─── Security ───────────────────────────────────────────────────────────────

.PHONY: vuln
vuln: $(GOVULNCHK) ## govulncheck over the shipping module
	@$(GOVULNCHK) ./...
	@# DEBT(docs/debt.md#12): the tools module is not scanned. Its binaries run
	@# in CI, so its dependency graph is in scope for a real audit.

## ─── Fuzz & bench ───────────────────────────────────────────────────────────

# DEBT(docs/debt.md#4): the agent-ref parser has a target; the plugin handshake,
# NDJSON frame, and kno.yaml parsers do not exist yet and must land with theirs.
# This target discovers targets rather than hardcoding them, so each one starts
# running the moment it appears.
.PHONY: fuzz-short
fuzz-short: ## Bounded fuzz on parsers (executions, not wall-clock)
	@$(SAFE) found=0; \
	for pkg in $$(go list ./... 2>/dev/null); do \
		targets=$$(go test -list 'Fuzz.*' $$pkg 2>/dev/null | grep '^Fuzz' || true); \
		for target in $$targets; do \
			found=1; \
			echo "fuzzing $$target in $$pkg"; \
			go test -run='^$$' -fuzz="^$$target$$" -fuzztime=$(FUZZTIME) $$pkg; \
		done; \
	done; \
	if [ "$$found" -eq 0 ]; then \
		$(call pending,fuzz-short,the first parser — see docs/debt.md#4); \
	fi

# DEBT(docs/debt.md#3): benchmark comparison is unimplemented. This target is a
# TRIPWIRE, not a no-op: the moment anyone adds a benchmark, it fails and
# demands the gate be built. Documented in CONTRIBUTING.md so the red build is
# not a surprise.
.PHONY: bench-diff
bench-diff: ## Tripwire: fails once benchmarks exist, until the gate is implemented
	@$(SAFE) if grep -rql '^func Benchmark' --include='*_test.go' . 2>/dev/null; then \
		printf '\033[31m FAIL \033[0m benchmarks exist but bench-diff is unimplemented.\n'; \
		printf '        Repay docs/debt.md#3: implement the >10%% regression gate.\n'; \
		exit 1; \
	else \
		$(call pending,bench-diff,the first benchmark — see docs/debt.md#3); \
	fi

## ─── Docs ───────────────────────────────────────────────────────────────────

.PHONY: docs
docs: ## Regenerate OpenAPI, check godoc coverage, verify links
	@go run ./internal/cmd/godoccheck
	@$(call pending,OpenAPI generation,the first proto service definition)
	@$(SAFE) broken=0; checked=0; \
	for f in $$(find . -name '*.md' -not -path './bin/*' -not -path './.git/*'); do \
		dir=$$(dirname "$$f"); \
		for target in $$(grep -oE '\]\([^)[:space:]]+\)' "$$f" 2>/dev/null \
			| sed -e 's/^](//' -e 's/)$$//'); do \
			case "$$target" in \
				\#*|http://*|https://*|mailto:*|tel:*) continue ;; \
			esac; \
			path=$$(printf '%s' "$$target" | sed 's/#.*$$//'); \
			[ -n "$$path" ] || continue; \
			checked=$$((checked+1)); \
			if [ ! -e "$$dir/$$path" ]; then \
				printf '\033[31m FAIL \033[0m broken link in %s: %s\n' "$$f" "$$target"; \
				broken=1; \
			fi; \
		done; \
	done; \
	if [ "$$broken" -ne 0 ]; then exit 1; fi; \
	printf '\033[32m  OK  \033[0m %d internal doc links resolve\n' "$$checked"

## ─── Release ────────────────────────────────────────────────────────────────

# goreleaser lives in its OWN module, tools/goreleaser, byte-pinned by its own
# committed go.sum and installed into ./bin by the same idiom as every other
# tool. It is not in tools/go.mod: its ~500-package graph MVS-bumps grpc,
# genproto, yaml and x/time underneath golangci-lint and buf, and a release
# tool must not be able to change what the lint gate compiles. See that
# module's own doc comment for why byte-pinning matters more here than
# anywhere else in the repo.
#
# The tag workflow calls `make release`, so the goreleaser that validates a
# config on a PR is the same goreleaser that builds the tag. There is no
# second version input anywhere to drift from this one.
GORELEASER_MOD := $(CURDIR)/tools/goreleaser
GORELEASER     := $(BIN)/goreleaser

$(GORELEASER): $(GORELEASER_MOD)/go.mod $(GORELEASER_MOD)/go.sum
	@mkdir -p $(BIN)
	@GOBIN=$(BIN) go -C $(GORELEASER_MOD) install tool
	@printf '\033[32m  OK  \033[0m goreleaser installed to ./bin\n'

.PHONY: release-check
release-check: $(GORELEASER) ## Validate .goreleaser.yaml. CI runs this on every PR
	@$(GORELEASER) check
	@$(MAKE) --no-print-directory release-identity-check

.PHONY: ledger-check
ledger-check: ## A ledger trigger naming the release being cut must carry a disposition
	@$(SAFE) v="$${VERSION:-$${GITHUB_REF_NAME:-}}"; \
	if [ -z "$$v" ]; then \
		printf '\033[31m FAIL \033[0m ledger-check needs the version being released.\n'; \
		printf '        make ledger-check VERSION=v0.0.1\n'; \
		printf '        Deliberately not defaulted to .release-please-manifest.json: that\n'; \
		printf '        holds the LAST released version, so checking against it asks\n'; \
		printf '        whether a release already cut is clear, which nothing names and\n'; \
		printf '        which therefore always passes. A gate that cannot fail occupies\n'; \
		printf '        the slot where a real one would go --- see docs/debt.md#70.\n'; \
		exit 1; \
	fi; \
	python3 scripts/ledger-check.py "$$v"
	@printf '\033[32m  OK  \033[0m .goreleaser.yaml is valid\n'

# The cosign certificate identity is the ONLY root of trust in this pipeline: a
# verify-blob without it confirms that somebody signed the file, which is a
# guarantee an attacker can also produce. It is quoted in four hand-maintained
# places — the release notes footer, SECURITY.md, README.md and install.sh —
# and every one of them names a workflow FILE. Rename or move that file and all
# four keep looking authoritative while verifying nothing.
#
# So: the file must exist, and every copy must name it.
RELEASE_WORKFLOW := .github/workflows/release.yml
IDENTITY_DOCS    := .goreleaser.yaml SECURITY.md README.md install.sh

# The published cosign identity, defined ONCE and asserted verbatim.
#
# The first version of this gate grepped each file for the string
# "workflows/release", which every one of them also contains in ordinary prose
# — so deleting every verification command from .goreleaser.yaml and
# SECURITY.md left the gate passing, and so would changing @refs/tags/ to
# @refs/heads/ or dropping the ^ anchor. That is docs/debt.md#70's own
# diagnosis reproduced in a new gate, in the PR that cites it.
#
# Every part of this string is load-bearing. Without the ^ anchor a certificate
# whose identity merely CONTAINS ours matches. Without @refs/tags/ a signature
# minted from a branch verifies. Without the escaped dots, . matches anything.
COSIGN_IDENTITY := ^https://github\.com/knograph/kno/\.github/workflows/release\.yml@refs/tags/.+$$

release-identity-check: ## The published cosign identity is one string, and every copy of it matches
	@$(SAFE) if [ ! -f $(RELEASE_WORKFLOW) ]; then \
		printf '\033[31m FAIL \033[0m %s does not exist, but the published cosign identity names it.\n' '$(RELEASE_WORKFLOW)'; \
		exit 1; \
	fi; \
	for f in $(IDENTITY_DOCS); do \
		if ! grep -qF -- '$(COSIGN_IDENTITY)' "$$f"; then \
			printf '\033[31m FAIL \033[0m %s does not carry the published cosign identity verbatim.\n' "$$f"; \
			printf '        want: %s\n' '$(COSIGN_IDENTITY)'; \
			printf '        A verification command with a weakened identity verifies less than it claims,\n'; \
			printf '        and one naming the wrong workflow verifies nothing.\n'; \
			exit 1; \
		fi; \
	done; \
	printf '\033[32m  OK  \033[0m the cosign identity is byte-identical in all %d places\n' $(words $(IDENTITY_DOCS))

# The check that actually matters, and the reason it runs in CI rather than
# living in a target someone might remember to call.
#
# `goreleaser check` is schema validation: it cannot see a wrong `main:` path, a
# wrong -X symbol, or a binary that never gets built. An -X path naming the
# wrong package or variable fails SILENTLY — the build succeeds and every
# released binary reports its version as "dev" forever. The only way to catch
# that is to build one and read its --version back.
#
# --single-target builds for this host only, so it costs seconds.
.PHONY: release-stamp
release-stamp: $(GORELEASER) ## Build one binary and prove the version stamp reaches it
	@$(GORELEASER) build --snapshot --clean --single-target
	@$(SAFE) hostdir=$$(find dist -maxdepth 1 -type d -name "kno_$$(go env GOOS)_$$(go env GOARCH)*" | head -1); \
	if [ -z "$$hostdir" ]; then \
		printf '\033[31m FAIL \033[0m release-stamp: dist/ has no build for this host.\n'; \
		exit 1; \
	fi; \
	stamped=$$("$$hostdir/kno" --version); \
	case "$$stamped" in \
		*" dev"*|*" dev "*) \
			printf '\033[31m FAIL \033[0m release-stamp: binary reports "%s".\n' "$$stamped"; \
			printf '        The -X path in .goreleaser.yaml does not name cli/root.go'"'"'s variable.\n'; \
			printf '        This failure is otherwise SILENT: the build succeeds and every\n'; \
			printf '        released binary reports its version as "dev" forever.\n'; \
			exit 1 ;; \
	esac; \
	printf '\033[32m  OK  \033[0m the version stamp reaches the binary: %s\n' "$$stamped"

# The local dry run: all six platforms. It CANNOT publish — --snapshot disables
# every publisher unconditionally, so this is safe on a machine that happens to
# have a GITHUB_TOKEN exported.
#
# Signing and SBOM generation are skipped because neither can work here: keyless
# cosign needs an OIDC identity only the workflow has, and syft is installed by
# that workflow. --snapshot also skips goreleaser's own validation of the tag
# and the working tree, so a green run here is not evidence that a real release
# would start — only that six platforms compile.
.PHONY: release-snapshot
release-snapshot: $(GORELEASER) release-stamp ## Dry-run build of every platform. Cannot publish anything
	@$(GORELEASER) release --snapshot --clean --skip=sign,sbom
	@printf '\033[32m  OK  \033[0m six platforms built into dist/\n'

# The real thing. CI-only by construction: `nothing built on laptops ships`
# (CLAUDE.md) is enforced here rather than trusted, because the difference
# between this target and release-snapshot is one word and a published artifact.
.PHONY: release
release: $(GORELEASER) ## Build and publish a tagged release. Refuses to run outside CI
	@$(SAFE) if [ -z "$${GITHUB_ACTIONS:-}" ]; then \
		printf '\033[31m FAIL \033[0m release: refusing to publish from a developer machine.\n'; \
		printf '        CLAUDE.md: nothing built on laptops ships. Push a tag and let\n'; \
		printf '        .github/workflows/release.yml build it, so the artifacts carry\n'; \
		printf '        provenance that says where they came from.\n'; \
		printf '        For a local dry run: make release-snapshot (it cannot publish).\n'; \
		exit 1; \
	fi
	@$(GORELEASER) release --clean

## ─── Meta ───────────────────────────────────────────────────────────────────

.PHONY: clean
clean: ## Remove build and coverage artifacts
	@rm -rf $(BIN) $(COVERAGE) coverage.html dist

.PHONY: help
help: ## List targets
	@grep -hE '^[a-z0-9_-]+:.*?## ' $(MAKEFILE_LIST) 2>/dev/null | sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-26s\033[0m %s\n", $$1, $$2}' \
		|| true

.PHONY: tape
tape: ## Re-record the README quickstart GIF. Requires vhs
	@$(SAFE) if ! command -v vhs >/dev/null 2>&1; then \
		printf '\033[31m FAIL \033[0m vhs is not installed: brew install vhs\n'; \
		printf '        The DoD asks for a re-recorded tape when CLI output changes.\n'; \
		exit 1; \
	fi; \
	if ! command -v kno >/dev/null 2>&1; then \
		printf '\033[31m FAIL \033[0m kno is not on PATH; the tape records the real binary.\n'; \
		printf '        go build -o "$$(go env GOPATH)/bin/kno" ./cmd/kno\n'; \
		exit 1; \
	fi; \
	vhs tapes/quickstart.tape; \
	printf '\033[32m  OK  \033[0m docs/quickstart.gif re-recorded\n'

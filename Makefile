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
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := check

# The gate ordering IS the design (cheapest failure first). Parallel make would
# destroy it, so refuse to run recipes in parallel.
.NOTPARALLEL:

BIN        := $(CURDIR)/bin
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
FUZZTIME ?= 30s

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
	@if [ -n "$${KNO_LIVE_TESTS:-}" ]; then \
		printf '\033[31m FAIL \033[0m test-integration: KNO_LIVE_TESTS is set. This target is the fixture path and must never spend money. Use `make test-live`.\n'; \
		exit 1; \
	fi
	@go test $(GOTESTFLAGS) $(GOTESTEXTRA) -tags=integration ./...

# Separate target, never reachable from `check` or `test`, so a live run is
# always an explicit choice. Nightly CI calls this one.
.PHONY: test-live
test-live: ## Integration tests against LIVE providers. Spends real money.
	@if [ -z "$${KNO_MAX_COST_USD:-}" ]; then \
		printf '\033[31m FAIL \033[0m test-live: refusing to run without KNO_MAX_COST_USD set.\n'; \
		exit 1; \
	fi
	@if ! grep -rqs 'KNO_MAX_COST_USD' --include='*.go' .; then \
		printf '\033[31m FAIL \033[0m test-live: KNO_MAX_COST_USD is set but NO CODE READS IT.\n'; \
		printf '        The budget guard (stats/budget) does not exist yet, so this cap is\n'; \
		printf '        not enforced by anything. Refusing to spend. See docs/debt.md#11.\n'; \
		exit 1; \
	fi
	@KNO_LIVE_TESTS=1 go test $(GOTESTFLAGS) -tags=integration ./...

.PHONY: coverage-check
coverage-check: ## Enforce coverage floors and the no-decrease ratchet
	@if [ -x $(BIN)/covercheck ]; then \
		$(BIN)/covercheck -profile=$(COVERAGE) -baseline=$(BASELINE); \
	else \
		$(call pending,coverage ratchet,M0c (feat/ring0-surface)); \
	fi

.PHONY: update-coverage-baseline
update-coverage-baseline: test ## Regenerate .coverage-baseline (review the diff like code)
	@$(BIN)/covercheck -profile=$(COVERAGE) -baseline=$(BASELINE) -write

.PHONY: secrets-scan
secrets-scan: ## gitleaks over BOTH the working tree and git history
	@if ! command -v gitleaks >/dev/null 2>&1; then \
		$(call skip_missing,gitleaks,secrets scan); \
	else \
		gitleaks dir . --no-banner --redact --exit-code 1; \
		gitleaks git . --no-banner --redact --exit-code 1; \
		printf '\033[32m  OK  \033[0m secrets scan: working tree and history clean\n'; \
	fi

.PHONY: record-fixtures
record-fixtures: ## Re-record adapter fixtures; secrets scrubbed at record time
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
	@if [ -f proto/PENDING ]; then \
$(call proto_bootstrap_check,generate); \
	else \
		$(BUF) generate; \
	fi

.PHONY: typecheck-proto
typecheck-proto: $(BUF) ## buf lint + buf breaking against main
	@if [ -f proto/PENDING ]; then \
$(call proto_bootstrap_check,typecheck-proto); \
	else \
		$(BUF) lint; \
		$(BUF) breaking --against '.git#branch=main'; \
		printf '\033[32m  OK  \033[0m proto lint + breaking clean\n'; \
	fi

.PHONY: generate-check
generate-check: generate ## Fail if checked-in generated code drifted from proto
	@if [ -f proto/PENDING ]; then \
		$(call pending,generate-check,M0b (feat/proto-contract)); \
	elif [ -n "$$(git status --porcelain -- gen/)" ]; then \
		printf '\033[31m FAIL \033[0m gen/ is stale — run `make generate` and commit the result.\n'; \
		git status --porcelain -- gen/; \
		exit 1; \
	else \
		printf '\033[32m  OK  \033[0m gen/ matches proto/\n'; \
	fi

## ─── Security ───────────────────────────────────────────────────────────────

.PHONY: vuln
vuln: $(GOVULNCHK) ## govulncheck over the shipping module
	@$(GOVULNCHK) ./...
	@# DEBT(docs/debt.md#12): the tools module is not scanned. Its binaries run
	@# in CI, so its dependency graph is in scope for a real audit.

## ─── Fuzz & bench ───────────────────────────────────────────────────────────

# DEBT(docs/debt.md#4): there are no fuzz targets yet. When they land, they
# must cover the parsers CLAUDE.md names — plugin handshake, NDJSON frames,
# agent-ref, kno.yaml. This target discovers targets rather than hardcoding
# them, so it starts working the moment the first one appears.
.PHONY: fuzz-short
fuzz-short: ## 30s fuzz on parsers
	@found=0; \
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
	@if grep -rql '^func Benchmark' --include='*_test.go' . 2>/dev/null; then \
		printf '\033[31m FAIL \033[0m benchmarks exist but bench-diff is unimplemented.\n'; \
		printf '        Repay docs/debt.md#3: implement the >10%% regression gate.\n'; \
		exit 1; \
	else \
		$(call pending,bench-diff,the first benchmark — see docs/debt.md#3); \
	fi

## ─── Docs ───────────────────────────────────────────────────────────────────

.PHONY: docs
docs: ## Regenerate OpenAPI, check godoc coverage, verify links
	@if [ -x $(BIN)/godoccheck ]; then \
		$(BIN)/godoccheck ./...; \
	else \
		$(call pending,godoc coverage,M0c (feat/ring0-surface)); \
	fi
	@$(call pending,OpenAPI generation,the first proto service definition)

## ─── Meta ───────────────────────────────────────────────────────────────────

.PHONY: clean
clean: ## Remove build and coverage artifacts
	@rm -rf $(BIN) $(COVERAGE) coverage.html

.PHONY: help
help: ## List targets
	@grep -hE '^[a-z0-9_-]+:.*?## ' $(MAKEFILE_LIST) 2>/dev/null | sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-26s\033[0m %s\n", $$1, $$2}' \
		|| true

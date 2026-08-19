# Kno — quality gates.
#
# `make check` is the only command you need to remember; it runs every gate
# CLAUDE.md requires on a PR, ordered fail-fast cheapest-first so that running
# it locally is actually cheap enough that people do.
#
# Slow gates (integration, fuzz) are deliberately NOT in `check`, matching
# CLAUDE.md's own listing. CI runs them separately.
#
# Tools are pinned in tools/go.mod via Go's native `tool` directive and
# installed into ./bin, so contributors and CI run byte-identical versions.
# See docs/plans/2026-08-18-repo-foundation.md.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := check

BIN        := $(CURDIR)/bin
TOOLS_MOD  := $(CURDIR)/tools
COVERAGE   := coverage.out
BASELINE   := .coverage-baseline

GOLANGCI   := $(BIN)/golangci-lint
GOFUMPT    := $(BIN)/gofumpt
GOVULNCHK  := $(BIN)/govulncheck
BUF        := $(BIN)/buf

# Repo-wide test flags. -race and -shuffle=on are mandatory per CLAUDE.md;
# they are not optional knobs and must not be removed to make a test pass.
GOTESTFLAGS ?= -race -shuffle=on

# 30s in the PR gate; nightly overrides to a longer run.
FUZZTIME ?= 30s

# Two distinct kinds of "this gate did not run", deliberately labeled
# differently because they mean opposite things:
#
#   SKIP — a tool is missing on THIS machine. The gate is real and CI runs it;
#          locally it is a warning. CI fails on any SKIP, so a missing tool can
#          never silently disable a gate where it counts.
#   PEND — the gate's implementation lands in a later milestone. Nothing is
#          being hidden; there is nothing to run yet. CI does not fail on PEND,
#          and each one names the milestone that retires it.
#
# skip_missing <tool> <gate name>
define skip_missing
	printf '\033[33m SKIP \033[0m %s: %s not installed. This gate did NOT run.\n' "$(2)" "$(1)"
endef

# pending <gate name> <milestone>
define pending
	printf '\033[34m PEND \033[0m %s: implementation lands in %s.\n' "$(1)" "$(2)"
endef

.PHONY: check
check: fmt-check lint test typecheck-proto vuln docs ## Run every PR gate (fail-fast, cheapest first)
	@printf '\033[32m  OK  \033[0m all gates passed\n'

## ─── Tools ──────────────────────────────────────────────────────────────────

.PHONY: tools
tools: ## Install pinned build tools into ./bin
	@mkdir -p $(BIN)
	@GOBIN=$(BIN) go -C $(TOOLS_MOD) install tool
	@printf '\033[32m  OK  \033[0m tools installed to ./bin\n'

$(GOLANGCI) $(GOFUMPT) $(GOVULNCHK) $(BUF): tools

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
test: ## Unit tests (-race, -shuffle=on) + coverage ratchet + secrets scan
	@go test $(GOTESTFLAGS) -coverprofile=$(COVERAGE) -covermode=atomic ./...
	@$(MAKE) --no-print-directory coverage-check
	@$(MAKE) --no-print-directory secrets-scan

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
secrets-scan: ## gitleaks over the repo; never optional in CI
	@if command -v gitleaks >/dev/null 2>&1; then \
		gitleaks detect --no-banner --redact --exit-code 1; \
	else \
		$(call skip_missing,gitleaks,secrets scan); \
	fi

.PHONY: test-integration
test-integration: ## Adapter tests against recorded fixtures (no live APIs)
	@go test $(GOTESTFLAGS) -tags=integration ./...

.PHONY: record-fixtures
record-fixtures: ## Re-record adapter fixtures; secrets scrubbed at record time
	@KNO_RECORD_FIXTURES=1 KNO_LIVE_TESTS=1 go test -tags=integration -run TestRecord ./adapters/...

.PHONY: update-golden
update-golden: ## Regenerate golden files (review the diff like code)
	@go test ./... -update

## ─── Proto ──────────────────────────────────────────────────────────────────

.PHONY: generate
generate: $(BUF) ## Regenerate code from proto (output is checked in)
	@if [ -d proto ]; then $(BUF) generate; \
	else $(call pending,generate,M0b (feat/proto-contract)); fi

.PHONY: typecheck-proto
typecheck-proto: $(BUF) ## buf lint + buf breaking against main
	@if [ ! -d proto ]; then \
		$(call pending,typecheck-proto,M0b (feat/proto-contract)); \
	else \
		$(BUF) lint; \
		$(BUF) breaking --against '.git#branch=main'; \
	fi

.PHONY: generate-check
generate-check: generate ## Fail if checked-in generated code drifted from proto
	@if ! git diff --quiet -- gen/; then \
		printf '\033[31m FAIL \033[0m gen/ is stale — run `make generate` and commit the result.\n'; \
		git --no-pager diff --stat -- gen/; \
		exit 1; \
	fi

## ─── Security ───────────────────────────────────────────────────────────────

.PHONY: vuln
vuln: $(GOVULNCHK) ## govulncheck over the shipping module
	@$(GOVULNCHK) ./...

## ─── Fuzz & bench ───────────────────────────────────────────────────────────

# DEBT(docs/debt.md#4): this fuzzes the protobuf runtime's parser, not ours.
# The parsers CLAUDE.md actually names — plugin handshake, NDJSON frames,
# agent-ref, kno.yaml — do not exist yet. Labeled PLACEHOLDER so a green run is
# never mistaken for "the security-relevant parsers are fuzzed".
.PHONY: fuzz-short
fuzz-short: ## 30s fuzz on parsers
	@printf '\033[34m PEND \033[0m fuzz-short: PLACEHOLDER, fuzzes the protobuf runtime not our parsers — docs/debt.md#4\n'
	@for pkg in $$(go list ./... 2>/dev/null); do \
		for target in $$(go test -list 'Fuzz.*' $$pkg 2>/dev/null | grep '^Fuzz' || true); do \
			echo "fuzzing $$target in $$pkg"; \
			go test -run='^$$' -fuzz="^$$target$$" -fuzztime=$(FUZZTIME) $$pkg; \
		done; \
	done

# DEBT(docs/debt.md#3): no benchmarks and no hot path exist yet. This gate is
# inert until the first _bench_test.go lands alongside the scoring loop,
# routing, or NDJSON framing.
.PHONY: bench-diff
bench-diff: ## Benchmark hot paths against main; >10% regression fails
	@if ! grep -rql '^func Benchmark' --include='*_test.go' . 2>/dev/null; then \
		printf '\033[34m PEND \033[0m bench-diff: no benchmarks yet — PLACEHOLDER, see docs/debt.md#3\n'; \
	else \
		printf '\033[31m FAIL \033[0m benchmarks exist but bench-diff is unimplemented; repay docs/debt.md#3\n'; \
		exit 1; \
	fi

## ─── Docs ───────────────────────────────────────────────────────────────────

.PHONY: docs
docs: ## Regenerate OpenAPI, check godoc coverage, verify links
	@if [ -x $(BIN)/godoccheck ]; then $(BIN)/godoccheck ./...; \
	else $(call pending,godoc coverage,M0c (feat/ring0-surface)); fi
	@$(call pending,OpenAPI generation,the first proto service definition)

## ─── Meta ───────────────────────────────────────────────────────────────────

.PHONY: clean
clean: ## Remove build and coverage artifacts
	@rm -rf $(BIN) $(COVERAGE) coverage.html

.PHONY: help
help: ## List targets
	@grep -hE '^[a-z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-26s\033[0m %s\n", $$1, $$2}'

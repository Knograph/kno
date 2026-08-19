# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/). Entries are derived from
[Conventional Commits](https://www.conventionalcommits.org/) by release-please.

**Pre-1.0 compatibility:** minor bumps may break, with notice here and migration notes. After 1.0,
the proto schema, the plugin protocol, exit codes, the `kno.yaml` schema, and the public Go API are
covenants — breaking any of them requires a major version.

## 1.0.0 (2026-08-19)


### Features

* **build:** enforce coverage floors and godoc coverage ([#9](https://github.com/Knograph/kno/issues/9)) ([bf924d2](https://github.com/Knograph/kno/commit/bf924d2e72de1b5edf1ca2d9685f29d0c8f11f69))
* **proto:** add Goal Direction so the sign of every reported number is interpretable ([#8](https://github.com/Knograph/kno/issues/8)) ([e5d4b98](https://github.com/Knograph/kno/commit/e5d4b98780dfbd9159757cbd16161e1a846f2a65))
* **proto:** the kno.v1 contract — Ring-0 schema and generated types (M0b) ([#2](https://github.com/Knograph/kno/issues/2)) ([8361e2a](https://github.com/Knograph/kno/commit/8361e2a4b69fffcbad25f9550f3c3b5470d3c2e5))


### Bug Fixes

* **build:** make gate recipes enforce failure without relying on .SHELLFLAGS ([#7](https://github.com/Knograph/kno/issues/7)) ([199ea63](https://github.com/Knograph/kno/commit/199ea63d8b3f50924d92a8543d18f7066c6f31eb))


### Documentation

* initial repo scaffold with design and operating manual ([b5ea55c](https://github.com/Knograph/kno/commit/b5ea55c861f97d274d90e6cd1d151436fa699906))


### Build & Dependencies

* repository foundation — gates, governance, and process machinery (M0a) ([#1](https://github.com/Knograph/kno/issues/1)) ([fb55946](https://github.com/Knograph/kno/commit/fb559461eac9481fe0be4242e31eb1b6f6e97a53))

## [Unreleased]

### Changed

- Go toolchain pinned to `go1.25.8` (from 1.25.5), which `govulncheck` flagged for `GO-2026-4602`
  in the standard library, reachable from `covercheck`. `GOTOOLCHAIN` is pinned in the Makefile so
  the toolchain is as reproducible as every other tool.

### Added

- `covercheck` and `godoccheck`, retiring the two gates that had reported `PEND` since the
  foundation landed. Coverage floors (85% on `core`/`stats`/`bridge`/`plugin`, 70% repo-wide) and
  the no-decrease ratchet are now enforced, as is godoc coverage on every exported symbol.
- `Direction` enum (`DIRECTION_MAXIMIZE` / `DIRECTION_MINIMIZE`) and `Report.goal_direction`.
  `DESIGN.md` defines a Goal as having a direction and both `Score.value` and
  `Valuation.delta_goal` document their sign as relative to it, but direction was represented
  nowhere — so the sign of `holdout_gain`, the headline number, was uninterpretable.
- Repository foundation: Go module, Apache-2.0 license, governance and community files, the quality
  gate machinery (`make check` and every gate `CLAUDE.md` requires), CI workflows, and the
  [Debt Ledger](docs/debt.md).
- Gates distinguish `SKIP` (a tool is missing — a hard failure under `KNO_CI`) from `PEND` (the
  implementation lands in a named later milestone), so a gate can never pass quietly without
  running.
- `make test-live` is the only path that can spend money, and it refuses to start unless a budget
  cap is set *and* some code actually reads it.
- Build tools pinned in an isolated `tools/` module via Go's native `tool` directive, so
  contributors and CI run byte-identical versions and tool dependencies never enter the shipping
  module's dependency graph.
- [ADR-0001](docs/adr/0001-proto-as-domain-types.md): generated proto messages are the domain types.
- [ADR-0002](docs/adr/0002-generated-code-layout.md): generated code is checked in under `gen/`.
- [ADR-0003](docs/adr/0003-platform-schema-boundary.md): the platform never adds fields to `kno.v1`.

[Unreleased]: https://github.com/knograph/kno/commits/main

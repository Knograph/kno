# The release pipeline

Repays [docs/debt.md#13](../debt.md#13). The trigger is *"before the first tagged release"*, and
`release-please` is configured with `initial-version: 0.0.1` against a `0.0.0` manifest — so the
first tag is one merged release PR away. This is the change that has to land before that PR can be
merged, because merging it tags, and a tag with no pipeline behind it either ships nothing or ships
something unsigned.

**Amended after Phase-1 review.** Three of the reviewer's findings changed the design rather than
being accepted: the pipeline as first drafted would never have run at all (finding 1, now
[#74](../debt.md#74)); goreleaser is byte-pinned in its own module rather than fetched by version
(finding 3); and the verification that actually matters now runs in CI rather than in a target
someone has to remember (finding 4). The review is summarised at the end.

## Problem

There is no `.goreleaser.yaml`, no release workflow, no checksums, no signatures, no SBOM, no
provenance. `SECURITY.md` says so plainly to security researchers, in a paragraph that exists
because a Phase-3 review caught the file asserting the opposite. `cli/root.go` carries
`var version = "dev"` with a comment promising it is *"stamped at build time by goreleaser"* — a
promise nothing keeps. `README.md` tells people to `go install`, which is the only honest
instruction available today and stops being the right one the moment signed artifacts exist.

`CLAUDE.md` § Version control & releases specifies the destination exactly: *tag → goreleaser →
signed multi-platform binaries (darwin/linux/windows, amd64/arm64) + Homebrew tap + install
script*, with *SLSA provenance, cosign-signed artifacts, SBOM (syft) attached, checksums in release
notes*, and *nothing built on laptops ships*.

## Design

### One version, injected into the variable that already exists

`cli.version` is the variable. It is unexported, which is not an obstacle: `-X` addresses a symbol
by its linker name, not by Go visibility. Two siblings are added next to it — `commit` and `date` —
because the subject of this entry is provenance, and a binary that cannot say which commit produced
it is a binary whose provenance stops at the release page.

Those three are the *preferred* source, not the only one. `identity()` falls back to the module and
VCS metadata the Go toolchain already embeds (`debug.ReadBuildInfo`), so a `go install
…/cmd/kno@v0.0.1` binary reports `v0.0.1` and a local build reports its revision, with `-dirty`
when the tree was not clean. Without that fallback, every non-release install would report `dev`
forever — including in `kno doctor --json`, which exists to be pasted into a bug report.

`kno --version` renders all three; **`kno doctor --json` reports only the version**. That split is
deliberate: `--version` is read by a person, `doctor --json` is a jq contract (ADR-0001's
reasoning, restated in `doctor.go`'s own comment), and appending a commit hash to a field consumers
parse as a version would be a silent breaking change to a documented output shape.

### goreleaser lives in its own module

`tools/goreleaser/` is a third module, byte-pinned by its own committed `go.sum` and installed into
`./bin` by the same idiom as every other tool. It is deliberately *not* in `tools/go.mod`: measured,
`go get -tool` there pulled ~500 packages — `gocloud.dev`, `sigs.k8s.io/kind`, `k8s.io/klog`,
`software.sslmate.com/src/go-pkcs12` — and MVS-bumped `google.golang.org/grpc`, `genproto`,
`go.yaml.in/yaml/v3` and `golang.org/x/time` **underneath `golangci-lint` and `buf`**. A release
tool must not be able to change what the lint gate compiles.

Version ceiling, recorded where it will be hit: goreleaser v2.14+ requires Go ≥ 1.26 and the
Makefile pins `GOTOOLCHAIN` to `go.mod`'s toolchain directive, so v2.13.3 is the newest release the
pinned compiler can build. Bumping goreleaser means bumping the toolchain first, in its own PR —
which is the right order, since that toolchain compiles the shipping binary and goreleaser only
boxes it.

| Target | Runs | Can it publish? |
|---|---|---|
| `make release-check` | `goreleaser check`, plus a guard that the published cosign identity names a workflow that exists | No |
| `make release-stamp` | `goreleaser build --single-target`, then reads the binary's `--version` back | No |
| `make release-snapshot` | all six platforms, `--snapshot` | **No.** `--snapshot` disables every publisher unconditionally |
| the release workflow | `make release` → `goreleaser release --clean` | Yes, and it is the only place that can |

`make release` refuses to run unless `GITHUB_ACTIONS` is set. That is the enforcement of *nothing
built on laptops ships*: a developer with a `GITHUB_TOKEN` exported would otherwise have been one
typo away from publishing.

### Signing, SBOM, provenance

- **Checksums** — `checksums.txt`, SHA-256, one file covering every archive, and its contents are
  appended into the release notes (`CLAUDE.md` asks for *checksums in release notes*; a link to an
  attached file is not that).
- **cosign, keyless** — a single `sign-blob` over `checksums.txt`, producing `.sig` and `.pem`.
  `COSIGN_EXPERIMENTAL` is obsolete and is not set; keyless is the default in current cosign, and
  `--yes` supplies the non-interactive consent. Signing the checksum file rather than each archive
  is the standard Sigstore recipe, and is what makes one signature verification cover every
  artifact.
- **SBOM** — syft, one SPDX document per archive, uploaded alongside it.
- **SLSA provenance** — `actions/attest-build-provenance` with `subject-checksums:
  dist/checksums.txt`, so every artifact named in the checksum file is attested in one call and the
  attested set cannot drift from the released set.

### The release is a draft until everything has succeeded

release-please creates the release as a draft; goreleaser uploads into it; provenance is attested;
only then does the workflow append the checksums and flip it public. Every failure boundary in a
release leaves something behind, and the states are not equally bad — an empty release, or one
signed but not attested, is indistinguishable from a complete one to anyone reading the page.
`replace_existing_artifacts` makes a retry of a partial upload possible at all.

### What CI enforces at PR time

A `release-config` job in `ci.yml` runs `make release-check` **and** `make release-stamp`. The
second is the load-bearing one: schema validation cannot see a wrong `main:` path or an `-X` symbol
that does not exist, and that failure is silent.

## Alternatives considered

**Pin goreleaser in `tools/go.mod` like every other tool.** Rejected on measurement, above: it
changes the dependency graph of the lint gate.

**`go run github.com/goreleaser/goreleaser/v2@<version>` with the version pinned in the Makefile.**
This was the plan's first design and Phase-1 review killed it. It pins by version and the Go
checksum database rather than by a committed `go.sum`, and the binary in question runs in the one
job holding `id-token: write` — where it can sign artifacts as this repository, permanently, into a
public transparency log. That job deserves the stronger guarantee, and a third module costs one
`go.mod` to get it.

**`goreleaser/goreleaser-action` in the workflow, a different invocation in the Makefile.**
Rejected: two install paths and two version pins, which is exactly the drift a single source of
truth exists to prevent.

**Sign every archive individually rather than the checksum file.** Rejected: twelve signatures and
twelve certificates for the same guarantee, and a verifier who checks one archive's signature but
not the checksum file learns nothing about the other eleven.

**Publish the Homebrew tap now.** Rejected — see Accepted risks; the repository does not exist.

## Affected packages

`cli` (the build-identity block and its test), plus root-level build and docs files, and a new
`tools/goreleaser` module. No `core`, no `stats`, no proto, no generated code. No public Go API
change: the variables and `buildIdentity` are unexported.

## Proto/schema impact

None.

## Edge cases

| Case | Mitigation |
|---|---|
| **A tag pushed by release-please starts no workflow run** — GitHub does not create runs from events triggered by the default `GITHUB_TOKEN` | Phase-1 finding 1, and the one that would have made the whole pipeline inert. `release-please.yml` takes an optional `RELEASE_PLEASE_TOKEN`; `release.yml` has a `workflow_dispatch` trigger with the reason written above it; the release is a draft, so a tag whose workflow never ran cannot look finished. [#74](../debt.md#74) |
| `workflow_dispatch` pointed at a branch | Refused in the first step. A "release" built from a branch signs a certificate naming `refs/heads/...`, which every published verification command rejects — artifacts that look official and verify as nothing |
| CGO in the build | `modernc.org/sqlite` is pure Go, so `CGO_ENABLED=0` cross-compiles all six targets from one runner. A cgo driver would need a cross toolchain per platform; that driver choice is load-bearing and is noted in the config |
| Someone runs a real release on a laptop | `make release` refuses without `GITHUB_ACTIONS`; the local target passes `--snapshot`, under which goreleaser cannot publish |
| A release run fails halfway | Draft until attested, `replace_existing_artifacts: true` so a retry does not 422 on already-uploaded assets |
| The tap repo does not exist | The `brews:` block is present but commented out, with the precondition stated. Commented YAML is invisible to `goreleaser check`, so the PR that enables it is the first that validates it — said in the comment |
| `--version` output changes | Now pinned by a table test, because SECURITY.md, README.md, `install.sh` and `make release-stamp` all depend on its shape |
| macOS has no `sha256sum` | Every published checksum instruction uses `grep`+`shasum -a 256 -c -`. `shasum` does not implement `--ignore-missing` either, which is why the obvious one-liner is not used |
| The cosign identity string drifts from the workflow filename | `make release-identity-check` asserts the workflow exists and that all four documents naming it still do. It is the only root of trust in the pipeline |
| Actions pinned to mutable tags | Every action in new code is pinned to a commit SHA, resolved through the GitHub API and annotated with its version ([#14](../debt.md#14)) |
| A secret reaching a log line | The workflow passes no provider credentials. `GITHUB_TOKEN` is the only secret, passed by environment, never on a command line. Keyless signing means there is no private key at all |

## Test plan

1. `cli`: a table test over `buildIdentity.String()` (all five combinations of present and absent
   fields) and one asserting `identity()` never reports the toolchain's `(devel)` placeholder,
   which would be a downgrade from `dev`. These exist because the render shape is quoted in three
   documents and asserted by one gate — and because the coverage ratchet is real.
2. `make release-check` — schema validation plus the identity-drift guard, in CI on every PR.
3. `make release-stamp` — builds a binary and reads its `--version` back, in CI on every PR.
4. `make release-snapshot` — all six platforms, locally.
5. `make check` green.

No test asserts that a YAML file contains a string it obviously contains; the build config is
verified by building.

## Rollback

Delete `.goreleaser.yaml`, `.github/workflows/release.yml`, `install.sh` and `tools/goreleaser/`;
revert the `cli/root.go` identity block. Nothing imports any of it. A release already published
cannot be un-signed, which is the point.

## Docs impact

- `SECURITY.md` — the *"not yet true, stated plainly"* paragraph replaced by what is now true, with
  the commands a researcher would actually run and an honest list of what is still not covered.
- `README.md` — an Install section: install script, releases, `go install`, and verification.
- `CONTRIBUTING.md` — the two out-of-`make check` targets, in their own table.
- `CHANGELOG.md` — an `Unreleased` entry.
- `docs/debt.md` — entry 13 REPAID; entries 71–74 opened.

## Accepted risks

- **goreleaser's ~500-package graph executes in the privileged job, unscanned.** Byte-pinning is not
  scanning, and `make vuln` covers the shipping module only. Mirrored as [#71](../debt.md#71),
  which is the sharp end of [#12](../debt.md#12).
- **`make release-check` and `make release-stamp` are not in `make check`.** A ~500-package compile
  plus a real build does not belong in a fail-fast-cheapest-first gate; they run as their own CI
  job instead, following the `release-please-config` precedent. The cost is that "green
  `make check` means green CI" no longer holds for these files. Mirrored as [#72](../debt.md#72).
- **The Homebrew tap is configured but disabled.** No `homebrew-tap` repository exists; enabling the
  block would fail the release after signing, which is the worst place to fail. Mirrored as
  [#73](../debt.md#73).
- **The first release still needs a human, until a token exists.** Mirrored as
  [#74](../debt.md#74), with a trigger of *before the first tag*.
- **The install script is not itself signed.** `curl | sh` trusts GitHub's TLS and this repository.
  The script verifies the checksum always and the signature when cosign is present, so the artifact
  is checked even though the script is not. Stated in the script's own header, because the person
  running it will not have read this file.
- **Nothing verifies that the release job *fails* when it should** — the general form of
  [#16](../debt.md#16). A release workflow can only be exercised by tagging, and the first tag is
  its first real execution. `make release-snapshot` covers the build half and nothing else;
  `--snapshot` also skips goreleaser's own tag and working-tree validation, so a green snapshot is
  not evidence that a real release would start.

## Phase-1 review

The reviewer's twelve findings, and what happened to each. Findings 1, 3, 4, 5, 6, 7, 8 and 12
changed the design; 2 produced a new gate; 10 and 11 were partly accepted; 9 was already stale by
the time the review landed.

| # | Finding | Disposition |
|---|---|---|
| 1 | The release workflow would never run: release-please tags with `GITHUB_TOKEN` | **Fixed** three ways, and recorded as [#74](../debt.md#74) |
| 2 | The cosign identity is hardcoded in four places with nothing enforcing agreement | **Fixed**: `make release-identity-check`, and the regexps are now anchored |
| 3 | `go run module@version` in the job holding `id-token: write` | **Fixed**: own module, own `go.sum` |
| 4 | The version-stamp check ran in no CI job | **Fixed**: `make release-stamp` in `release-config` |
| 5 | Published-then-attested; a retry 422s on existing assets | **Fixed**: draft until attested, `replace_existing_artifacts` |
| 6 | `go install` binaries report `dev`, including to `doctor --json` | **Fixed**: `debug.ReadBuildInfo` fallback |
| 7 | "No unit tests" was wrong; the coverage ratchet would fail | **Fixed**: table test on the render |
| 8 | `sha256sum --ignore-missing` does not exist on macOS; `gh attestation verify` needs `--signer-workflow` | **Fixed** everywhere the instructions appear |
| 9 | The docs half of the change did not exist | Stale — the reviewer read the tree mid-implementation |
| 10 | The stated reason for rejecting `goreleaser-action` is contradicted by two other third-party actions in the same job | **Accepted**; the rejection is now argued on the surviving reason. The SHAs were resolved through the GitHub API, not typed |
| 11 | `release-config` compiles goreleaser on every PR, not just relevant ones | **Partly accepted**: cached on `tools/goreleaser/go.sum`, no path filter. A path filter needs another action and would skip the job on the PR that renames a file it watches |
| 12 | Workflow-level `permissions`; unvalidated commented YAML; reproducibility needs the toolchain | **Fixed**: job-level permissions, both facts stated where they matter |

One `CLAUDE.md` literal miss the reviewer caught: § Security asks for *checksums in release notes*,
which the first draft satisfied with a link. The workflow now appends the checksum file's contents
to the notes before publishing.

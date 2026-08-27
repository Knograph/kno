#!/bin/sh
#
# Install kno.
#
#   curl -sSfL https://raw.githubusercontent.com/knograph/kno/main/install.sh | sh
#
# What this does, in order: work out your platform, download the release
# archive and its checksum file, VERIFY the checksum, verify the cosign
# signature if you have cosign installed, and put the binary somewhere on your
# PATH. It never runs anything it downloaded except the final `kno --version`.
#
# Honest about its own trust boundary: this script is not itself signed. Piping
# it to a shell trusts GitHub's TLS and this repository — you are welcome to
# read it first, which is why it is short. What it installs IS verified: the
# checksum always, and the signature whenever cosign is present. If you want
# the stronger guarantee, install cosign first, or follow the manual steps in
# the release notes.
#
# Environment:
#   KNO_VERSION      version to install, e.g. v0.1.0 (default: latest)
#   KNO_INSTALL_DIR  where to put the binary (default: /usr/local/bin, or
#                    ~/.local/bin when /usr/local/bin is not writable)

set -eu

REPO="knograph/kno"

log()  { printf '%s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed."
}

need uname
need tar
need mktemp

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -sSfL -o "$2" "$1"; }
	fetch_stdout() { curl -sSfL "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO "$2" "$1"; }
	fetch_stdout() { wget -qO- "$1"; }
else
	die "need curl or wget."
fi

# ── Platform ────────────────────────────────────────────────────────────────

os=$(uname -s)
case "$os" in
	Darwin) os=darwin ;;
	Linux)  os=linux ;;
	# Windows archives are published (kno_<version>_windows_<arch>.zip), but a
	# POSIX shell installer is the wrong tool for placing them. Say so rather
	# than half-working under Git Bash.
	MINGW*|MSYS*|CYGWIN*)
		die "Windows: download kno_..._windows_<arch>.zip from https://github.com/$REPO/releases and unzip it onto your PATH." ;;
	*) die "unsupported OS: $os" ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64|amd64) arch=amd64 ;;
	arm64|aarch64) arch=arm64 ;;
	*) die "unsupported architecture: $arch (kno publishes amd64 and arm64)" ;;
esac

# ── Version ─────────────────────────────────────────────────────────────────

version="${KNO_VERSION:-}"
if [ -z "$version" ]; then
	log "Resolving the latest release..."
	# The tag_name from the /releases/latest API response, parsed with sed so
	# this script has no dependency a fresh machine might lack.
	version=$(fetch_stdout "https://api.github.com/repos/$REPO/releases/latest" \
		| sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
	[ -n "$version" ] || die "could not resolve the latest release; set KNO_VERSION."
fi

# The archive names carry the version WITHOUT its leading v; the tag has it.
bare=${version#v}
archive="kno_${bare}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$version"

# ── Download and verify ─────────────────────────────────────────────────────

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

log "Downloading $archive ($version)..."
fetch "$base/$archive" "$tmp/$archive" || die "no such release asset: $archive"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "release $version has no checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
	sumcmd="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
	sumcmd="shasum -a 256"
else
	die "need sha256sum or shasum to verify the download; refusing to install unverified."
fi

log "Verifying checksum..."
want=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
[ -n "$want" ] || die "$archive is not listed in checksums.txt."
got=$(cd "$tmp" && $sumcmd "$archive" | cut -d' ' -f1)
[ "$want" = "$got" ] || die "checksum mismatch for $archive — do not install this. Expected $want, got $got."

# Signature verification is best-effort by design: cosign is not something most
# people have installed, and refusing to install without it would push everyone
# to `go install`, which verifies less. When cosign IS present, the identity is
# pinned to this repository's release workflow — an unpinned verify-blob accepts
# a valid signature from anybody at all, which is worse than not checking.
if command -v cosign >/dev/null 2>&1; then
	log "Verifying cosign signature..."
	fetch "$base/checksums.txt.bundle" "$tmp/checksums.txt.bundle" || die "release $version has no signature bundle."
	# The identity is spelled out rather than built from $REPO, and single-quoted
	# so the shell expands none of it. `make release-identity-check` asserts this
	# exact string is byte-identical here, in .goreleaser.yaml, in SECURITY.md
	# and in README.md — a check that cannot work against an interpolated copy,
	# and the drift it exists to catch is a weakened regex nobody notices.
	#
	# Output is captured and shown on failure rather than discarded. cosign v2
	# verify-blob does an online Rekor lookup and a TUF root refresh, so behind
	# a proxy or during a Sigstore incident this fails for reasons that are not
	# an attack — and telling a user "verification FAILED, do not install" for a
	# network timeout trains them to ignore the one message here that must never
	# be ignored.
	if cosign verify-blob "$tmp/checksums.txt" \
		--new-bundle-format \
		--bundle "$tmp/checksums.txt.bundle" \
		--certificate-identity-regexp '^https://github\.com/[Kk]nograph/kno/\.github/workflows/release\.yml@refs/tags/.+$' \
		--certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
		>"$tmp/cosign.log" 2>&1
	then
		log "Signature OK."
	else
		cat "$tmp/cosign.log" >&2
		die "cosign could not verify $version — see the output above. If it names a network or transparency-log problem, retry; if it says the signature is invalid, do not install this."
	fi
else
	log "cosign not found — checksum verified, signature not. See the release notes to verify by hand."
fi

# ── Install ─────────────────────────────────────────────────────────────────

tar -xzf "$tmp/$archive" -C "$tmp" kno || die "archive did not contain a kno binary."

dir="${KNO_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
	if [ -w /usr/local/bin ]; then
		dir=/usr/local/bin
	else
		dir="$HOME/.local/bin"
	fi
fi
mkdir -p "$dir" || die "cannot create $dir"
[ -w "$dir" ] || die "$dir is not writable. Set KNO_INSTALL_DIR, or re-run with sudo."

# Staged inside the destination directory and renamed, because rename within
# one filesystem is atomic and a cross-device `mv` is not: $tmp comes from
# mktemp -d and $dir is usually /usr/local/bin, which on macOS and in most
# containers is a different filesystem. There the move is copy-then-unlink, so
# an interrupt or a full disk during an UPGRADE truncates the working install
# and leaves a broken executable on PATH. chmod before the rename also closes
# the window where the binary is reachable with whatever mode the tarball had.
staged="$dir/.kno.$$"
trap 'rm -rf "$tmp"; rm -f "$staged"' EXIT INT TERM
cp "$tmp/kno" "$staged" || die "cannot write to $dir"
chmod 0755 "$staged"
mv -f "$staged" "$dir/kno" || die "cannot install into $dir"

log "Installed $("$dir/kno" --version) to $dir/kno"

case ":$PATH:" in
	*":$dir:"*) ;;
	*) log "note: $dir is not on your PATH. Add it, or move the binary." ;;
esac

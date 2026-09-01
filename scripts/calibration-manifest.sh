#!/usr/bin/env bash
# calibration-manifest.sh — regenerate each calibration set's content_sha256.
#
# The manifest attests to the FILE, not to its meaning: the hash is taken over
# records.jsonl as committed, so a formatter that changes nothing semantically
# still changes it. That is deliberate. A gate that only noticed semantic edits
# could be routed around by reformatting, and the failure this hash exists to
# make visible — deleting the records a judge gets wrong — is a file edit.
#
# Run it in the SAME commit that edits a set. `make check` fails otherwise, and
# it names this script.

set -euo pipefail

cd "$(cd "$(dirname "$0")/.." && pwd)"

ROOT=judge/testdata/calibration

GREEN=$'\033[32m'
RED=$'\033[31m'
OFF=$'\033[0m'

if ! command -v python3 >/dev/null 2>&1; then
	printf '%s FAIL %s calibration-manifest: python3 is not installed.\n' "$RED" "$OFF" >&2
	exit 1
fi

count=0
for dir in "$ROOT"/*/; do
	[ -f "$dir/records.jsonl" ] || continue
	python3 - "$dir" <<-'PY'
		import hashlib, json, pathlib, sys

		d = pathlib.Path(sys.argv[1])
		digest = hashlib.sha256((d / "records.jsonl").read_bytes()).hexdigest()
		manifest_path = d / "manifest.json"
		manifest = json.loads(manifest_path.read_text())
		manifest["content_sha256"] = digest
		manifest_path.write_text(json.dumps(manifest, indent=2) + "\n")
		print(f"  {d.name}: {digest[:12]}")
	PY
	count=$((count + 1))
done

printf '%s  OK  %s calibration-manifest: %d set(s) re-attested\n' "$GREEN" "$OFF" "$count"

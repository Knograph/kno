#!/usr/bin/env python3
"""Fail a release when a Debt Ledger trigger has lapsed without a disposition.

CLAUDE.md: "CI fails a release tag if any ledger entry's trigger has lapsed
without a disposition." Nothing implemented that until 0.0.1's close-out, which
means the rule the whole ledger rests on was enforced by remembering.

What this checks, deliberately narrowly: every entry whose trigger names the
release being cut must have been repaid, partly repaid, or re-dated. Those are
exactly the dispositions the ledger rules allow, and each leaves a word in the
table. An entry that names the release and says none of them is a trigger that
lapsed while nobody looked.

It does NOT parse prose dates or evaluate arbitrary conditions. A checker that
guessed at "when a second Tuner lands" would be wrong often enough to get
switched off, and a gate people switch off is worse than no gate --- which is
what docs/debt.md#70 is about. This one asks a question with a mechanical
answer.

`--json` is the second caller, added for docs/status.json (see
docs/plans/2026-08-30-kno-status.md). It reports the ledger's size and how much
of it is open, over the SAME row scan and the SAME DISPOSITIONS tuple the
release gate uses. That sameness is the whole point: a second parser of a
hand-written 84-row Markdown table would drift from this one silently, because
the two consumers ask different questions and would disagree without either
failing. The release gate's exit-code behaviour is unchanged by its arrival.

The one place the two callers differ is strictness, and it is deliberate: this
gate skips a row it cannot parse, because it is narrow on purpose; `--json`
reports `skipped` so its caller can refuse to publish a count that silently
omitted a row. See docs/debt.md#87.
"""

import json
import re
import sys

LEDGER = "docs/debt.md"

# The dispositions the ledger rules permit.
DISPOSITIONS = ("REPAID", "Re-dated", "re-dated", "RE-DATED", "won't fix", "WON'T FIX")

# A row of the ledger table. Anything else in the file is prose.
_SEPARATOR = re.compile(r"^\|[\s:|-]+\|$")
_ANCHOR = re.compile(r'id="(\d+)"')


def scan(path=LEDGER):
    """Return (rows, skipped) for the ledger at path.

    A row is (entry_id, what_cell, trigger_cell) --- the same three values the
    release gate has always read, taken by the same rule: a line carrying an
    id="N" anchor and at least six pipe-delimited cells.

    skipped counts the lines that LOOK like table rows and are not the header
    or its separator, yet fail one of those two conditions. The release gate
    ignores it; a caller publishing a count must not, because a skipped row is
    an undercount presented as a fact.
    """
    rows, skipped = [], 0
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            stripped = line.strip()
            if not stripped.startswith("|"):
                continue
            if _SEPARATOR.match(stripped):
                continue
            cells = line.split("|")
            # The header's first cell is the column name "#". A malformed row
            # is not distinguishable from a header by shape alone, so the
            # header is identified by its content rather than its position.
            if len(cells) > 1 and cells[1].strip() == "#":
                continue
            m = _ANCHOR.search(line)
            if not m or len(cells) < 6:
                skipped += 1
                continue
            rows.append((m.group(1), cells[2], cells[4]))
    return rows, skipped


def disposed(what, trigger):
    """Whether a row carries one of the dispositions the ledger rules allow."""
    return any(d in what or d in trigger for d in DISPOSITIONS)


def report_json(path=LEDGER):
    """The ledger's shape, for docs/status.json."""
    rows, skipped = scan(path)
    entries = [
        {"id": int(entry), "open": not disposed(what, trigger)}
        for entry, what, trigger in rows
    ]
    return {
        "total": len(entries),
        "open": sum(1 for e in entries if e["open"]),
        "skipped": skipped,
        "entries": entries,
    }


def check_release(version, path=LEDGER):
    """Return the entries naming version that carry no disposition."""
    # "the first tagged release" for 0.0.1; the literal version otherwise. Both
    # spellings appear, because an entry written before any release existed
    # could not name a number.
    patterns = [re.escape(version)]
    if version == "0.0.1":
        patterns += ["first tagged release", "first release"]
    names_release = re.compile("|".join(patterns), re.IGNORECASE)

    lapsed = []
    rows, _ = scan(path)
    for entry, what, trigger in rows:
        if not names_release.search(trigger):
            continue
        if disposed(what, trigger):
            continue
        lapsed.append((entry, re.sub(r"[*~`]", "", what).strip()[:100]))
    return lapsed


def main(argv):
    if len(argv) == 2 and argv[1] == "--json":
        json.dump(report_json(), sys.stdout, indent=2, sort_keys=True)
        sys.stdout.write("\n")
        return 0

    if len(argv) != 2:
        print("usage: ledger-check.py <version-being-released>", file=sys.stderr)
        print("       ledger-check.py --json", file=sys.stderr)
        return 2
    version = argv[1].lstrip("v")

    lapsed = check_release(version)

    if lapsed:
        plural = "y" if len(lapsed) == 1 else "ies"
        print("\033[31m FAIL \033[0m %d ledger entr%s name release %s and carry "
              "no disposition:" % (len(lapsed), plural, version))
        for entry, what in lapsed:
            print("        #%s: %s" % (entry, what))
        print("        Repay it, re-date it with a written reason, or promote it")
        print("        to won't fix with the rationale in an ADR. Silent carryover")
        print("        is the failure this ledger exists to prevent.")
        return 1

    print("\033[32m  OK  \033[0m every ledger entry naming release %s carries a "
          "disposition" % version)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

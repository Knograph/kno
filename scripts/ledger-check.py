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
"""

import re
import sys

LEDGER = "docs/debt.md"

# The dispositions the ledger rules permit.
DISPOSITIONS = ("REPAID", "Re-dated", "re-dated", "won't fix", "WON'T FIX")


def main(argv):
    if len(argv) != 2:
        print("usage: ledger-check.py <version-being-released>", file=sys.stderr)
        return 2
    version = argv[1].lstrip("v")

    # "the first tagged release" for 0.0.1; the literal version otherwise. Both
    # spellings appear, because an entry written before any release existed
    # could not name a number.
    patterns = [re.escape(version)]
    if version == "0.0.1":
        patterns += ["first tagged release", "first release"]
    names_release = re.compile("|".join(patterns), re.IGNORECASE)

    lapsed = []
    with open(LEDGER, encoding="utf-8") as fh:
        for line in fh:
            m = re.search(r'id="(\d+)"', line)
            if not m:
                continue
            cells = line.split("|")
            if len(cells) < 6:
                continue
            entry, what, trigger = m.group(1), cells[2], cells[4]
            if not names_release.search(trigger):
                continue
            if any(d in what or d in trigger for d in DISPOSITIONS):
                continue
            lapsed.append((entry, re.sub(r"[*~`]", "", what).strip()[:100]))

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

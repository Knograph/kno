#!/usr/bin/env python3
"""Unit tests for scripts/ledger-check.py, run by `make status-check`.

Two things are under test. The `--json` mode is new surface and docs/status.json
publishes its numbers, so it gets fixture ledgers covering a clean row, a
disposed row, a row with no anchor and a row with too few cells. And the
release gate's exit-code path is a release blocker, so there is a regression
test that it still refuses and still accepts the inputs it always did --- the
`--json` mode was added by refactoring the row scan the gate uses, and a
refactor of a release gate is exactly the change that has to prove it changed
nothing.

Stdlib unittest, loaded by path because the module's filename has a hyphen and
is therefore not importable by name. No pytest: adding a dependency to a gate
is how a gate becomes optional.
"""

import importlib.util
import io
import os
import pathlib
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout

_HERE = pathlib.Path(__file__).resolve().parent
_SPEC = importlib.util.spec_from_file_location("ledger_check", _HERE / "ledger-check.py")
ledger = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(ledger)

HEADER = "| # | What was deferred | Why | Repayment trigger | Owner |\n|---|---|---|---|---|\n"


def row(entry, what="something deferred", why="a reason", trigger="before 1.0", owner="@nobody"):
    return '| <a id="%s"></a>%s | %s | %s | %s | %s |\n' % (
        entry, entry, what, why, trigger, owner)


def ledger_file(body):
    """Write a fixture ledger and return its path. Caller owns the temp dir."""
    tmp = tempfile.mkdtemp()
    path = os.path.join(tmp, "debt.md")
    with open(path, "w", encoding="utf-8") as fh:
        fh.write("# Debt Ledger\n\nProse that mentions | a pipe.\n\n" + HEADER + body)
    return path


class TestScan(unittest.TestCase):
    def test_a_clean_ledger_parses_every_row_and_skips_nothing(self):
        path = ledger_file(row(1) + row(2))
        rows, skipped = ledger.scan(path)
        self.assertEqual([r[0] for r in rows], ["1", "2"])
        self.assertEqual(skipped, 0)

    def test_the_header_and_its_separator_are_not_rows_and_are_not_skipped(self):
        path = ledger_file("")
        rows, skipped = ledger.scan(path)
        self.assertEqual(rows, [])
        self.assertEqual(skipped, 0)

    def test_prose_containing_a_pipe_is_not_a_row(self):
        # The fixture's prose line contains a pipe but does not start with one.
        path = ledger_file(row(1))
        rows, skipped = ledger.scan(path)
        self.assertEqual(len(rows), 1)
        self.assertEqual(skipped, 0)

    def test_a_row_with_no_anchor_is_counted_as_skipped(self):
        path = ledger_file(row(1) + "| 2 | no anchor | why | trigger | owner |\n")
        rows, skipped = ledger.scan(path)
        self.assertEqual(len(rows), 1)
        self.assertEqual(skipped, 1)

    def test_a_row_with_too_few_cells_is_counted_as_skipped(self):
        path = ledger_file(row(1) + '| <a id="2"></a>2 | truncated |\n')
        rows, skipped = ledger.scan(path)
        self.assertEqual(len(rows), 1)
        self.assertEqual(skipped, 1)


class TestReportJSON(unittest.TestCase):
    def test_a_disposed_row_is_not_open(self):
        path = ledger_file(row(1, what="**REPAID** in M1") + row(2))
        got = ledger.report_json(path)
        self.assertEqual(got["total"], 2)
        self.assertEqual(got["open"], 1)
        self.assertEqual(got["skipped"], 0)
        self.assertEqual(
            got["entries"],
            [
                {"id": 1, "open": False, "redated": False},
                {"id": 2, "open": True, "redated": False},
            ],
        )

    def test_a_disposition_in_the_trigger_cell_also_counts(self):
        # The release gate reads both cells; --json must read the same two, or
        # the two callers disagree about the same row.
        path = ledger_file(row(1, what="**REPAID** in M2"))
        self.assertEqual(ledger.report_json(path)["open"], 0)

    def test_a_redate_in_the_trigger_cell_leaves_the_row_open(self):
        # This test asserted the opposite until 2026-08-31, which is how eight
        # live entries became invisible to the release gate. A re-date moves a
        # trigger; it does not close a row.
        path = ledger_file(row(1, trigger="Re-dated to v0.3"))
        got = ledger.report_json(path)
        self.assertEqual(got["open"], 1)
        self.assertEqual(got["redated_open"], 1)

    def test_skipped_rows_are_reported_rather_than_folded_into_total(self):
        path = ledger_file(row(1) + "| 2 | no anchor | why | trigger | owner |\n")
        got = ledger.report_json(path)
        self.assertEqual((got["total"], got["open"], got["skipped"]), (1, 1, 1))

    def test_the_real_ledger_parses_completely(self):
        root = _HERE.parent
        got = ledger.report_json(str(root / "docs" / "debt.md"))
        self.assertEqual(got["skipped"], 0, "docs/debt.md has a row that does not parse")
        self.assertGreater(got["total"], 0)


class TestReleaseGateIsUnchanged(unittest.TestCase):
    """The exit-code path a release depends on. See release.yml's ledger step."""

    def _run(self, argv, cwd):
        here = os.getcwd()
        os.chdir(cwd)
        buf = io.StringIO()
        try:
            with redirect_stdout(buf), redirect_stderr(io.StringIO()):
                code = ledger.main(argv)
        finally:
            os.chdir(here)
        return code, buf.getvalue()

    def _tree(self, body):
        tmp = tempfile.mkdtemp()
        os.makedirs(os.path.join(tmp, "docs"))
        with open(os.path.join(tmp, "docs", "debt.md"), "w", encoding="utf-8") as fh:
            fh.write("# Debt Ledger\n\n" + HEADER + body)
        return tmp

    def test_an_entry_naming_the_release_with_no_disposition_fails(self):
        tree = self._tree(row(7, trigger="before 0.2.0"))
        code, out = self._run(["ledger-check.py", "0.2.0"], tree)
        self.assertEqual(code, 1)
        self.assertIn("#7", out)

    def test_the_same_entry_repaid_passes(self):
        tree = self._tree(row(7, what="**REPAID**", trigger="before 0.2.0"))
        code, _ = self._run(["ledger-check.py", "v0.2.0"], tree)
        self.assertEqual(code, 0)

    def test_an_entry_naming_another_release_is_not_the_gates_business(self):
        tree = self._tree(row(7, trigger="before 0.3.0"))
        code, _ = self._run(["ledger-check.py", "0.2.0"], tree)
        self.assertEqual(code, 0)

    def test_an_unparseable_row_is_still_tolerated_by_the_release_gate(self):
        # Deliberate asymmetry with --json: the gate stays narrow, and only the
        # new consumer refuses to publish over a skipped row. docs/debt.md#87.
        tree = self._tree("| 7 | no anchor | why | before 0.2.0 | owner |\n")
        code, _ = self._run(["ledger-check.py", "0.2.0"], tree)
        self.assertEqual(code, 0)

    def test_no_argument_is_a_usage_error(self):
        code, _ = self._run(["ledger-check.py"], self._tree(""))
        self.assertEqual(code, 2)


class DuplicateIds(unittest.TestCase):
    """Two rows sharing an id is an ambiguous citation and an overcount.

    Not hypothetical: two parallel workstreams each appended an id="131",
    and the collision surfaced only as a merge conflict in a file where
    conflicts are routine. Landing in either order without touching the same
    lines, nothing would have caught it.
    """

    def test_a_collision_is_reported(self):
        rows = [("7", "what", "trigger"), ("8", "w", "t"), ("7", "w2", "t2")]
        self.assertEqual(ledger.duplicate_ids(rows), ["7"])

    def test_each_duplicate_is_named_once(self):
        rows = [("3", "a", "b"), ("3", "c", "d"), ("3", "e", "f")]
        self.assertEqual(ledger.duplicate_ids(rows), ["3"])

    def test_a_clean_ledger_reports_nothing(self):
        rows = [(str(n), "w", "t") for n in range(1, 40)]
        self.assertEqual(ledger.duplicate_ids(rows), [])

    def test_the_real_ledger_has_no_duplicates(self):
        # Resolved from this file rather than the cwd: scan()'s default path
        # is repo-relative, and these tests are run from both the repo root
        # (make test) and from scripts/ (directly).
        real, _ = ledger.scan(_HERE.parent / "docs" / "debt.md")
        self.assertEqual(ledger.duplicate_ids(real), [])


class ReDateIsNotImmunity(unittest.TestCase):
    """A re-dated entry is live debt with a new trigger, not a closed one.

    Treating "Re-dated" as a disposition granted eight rows permanent
    immunity from the release gate and understated the published open count
    by 17%. #43's re-dated trigger then fired a second time and nothing
    reported it.
    """

    def test_a_redated_row_is_still_open(self):
        self.assertFalse(ledger.disposed("Re-dated (2026-08-29): moved", "before 1.0"))

    def test_every_spelling_of_redated_is_still_open(self):
        for spelling in ("Re-dated", "re-dated", "RE-DATED"):
            self.assertFalse(
                ledger.disposed("%s: moved" % spelling, "before 1.0"),
                "%s must not close a row" % spelling,
            )

    def test_repaid_still_closes_a_row(self):
        self.assertTrue(ledger.disposed("REPAID (2026-08-28) on the trigger", "x"))

    def test_wont_fix_still_closes_a_row(self):
        self.assertTrue(ledger.disposed("promoted to won't fix, see ADR-0004", "x"))

    def test_a_redated_row_whose_trigger_fires_is_reported(self):
        with tempfile.NamedTemporaryFile("w", suffix=".md", delete=False) as fh:
            fh.write("| # | What | Why | Trigger | Owner |\n")
            fh.write("|---|---|---|---|---|\n")
            fh.write('| <a id="7"></a>7 | **Re-dated** (2026): moved | why | '
                     "before 0.9.9 | @o |\n")
            path = fh.name
        try:
            rows, _ = ledger.scan(path)
            firing = [
                (e, w) for e, w, t in rows
                if not ledger.disposed(w, t) and "0.9.9" in t
            ]
            self.assertEqual(len(firing), 1, "a re-dated row must not be immune")
        finally:
            os.unlink(path)

    def test_redated_is_counted_separately(self):
        self.assertTrue(ledger.redated("Re-dated (2026): moved", "x"))
        self.assertFalse(ledger.redated("REPAID (2026)", "x"))


if __name__ == "__main__":
    unittest.main(verbosity=0, argv=[sys.argv[0], "-q"])

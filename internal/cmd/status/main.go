// Command status generates docs/status.json --- the machine-readable answer to
// "what does this release of Kno do, and how honest is it being about the parts
// it does not do yet?".
//
// `make status` writes the file; `make status-check`, which runs inside `make
// docs` and therefore inside `make check`, regenerates it and fails if the
// committed copy differs. The fix is always `make status` plus a commit.
//
// There is no `kno status` command and there must not be one: a command reports
// the binary in front of you, this file reports a release, and they disagree in
// exactly the cases that matter. cli/status.go's header carries the full
// argument; docs/plans/2026-08-30-kno-status.md section 2 carries the evidence.
//
// encoding/json here is the depguard exemption's own case, the same one
// cli/jsonreport.go and internal/cmd/pricingcheck argue: this decodes a
// hand-written shape from a build script, and no kno.v1 type is involved.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/knograph/kno/cli"
)

// artifact is the committed file every mode reads or writes.
const artifact = "docs/status.json"

// ledgerScript is the ONLY reader of docs/debt.md, by design. A Go parser of
// that table would be a second reader of an 84-row hand-written Markdown file,
// and it would drift from the Python one silently. See cli.StatusDebt.
const ledgerScript = "scripts/ledger-check.py"

func main() {
	var (
		out   = flag.String("o", artifact, "write the artifact here")
		check = flag.Bool("check", false, "regenerate and fail if the committed artifact differs")
	)
	flag.Parse()

	if err := run(*out, *check); err != nil {
		fmt.Fprintf(os.Stderr, "\033[31m FAIL \033[0m status: %v\n", err)
		os.Exit(1)
	}
}

// run generates the artifact, or compares it against the committed copy.
func run(out string, check bool) error {
	debt, err := readLedger()
	if err != nil {
		return err
	}
	got, err := render(debt)
	if err != nil {
		return err
	}

	if !check {
		if err := os.WriteFile(out, got, 0o644); err != nil { //nolint:gosec // a committed doc, world-readable like every other file in docs/.
			return fmt.Errorf("writing %s: %w", out, err)
		}
		fmt.Printf("\033[32m  OK  \033[0m wrote %s (%d ledger entries, %d open)\n",
			out, debt.Total, debt.Open)
		return nil
	}

	want, err := os.ReadFile(artifact)
	if err != nil {
		return fmt.Errorf("reading %s: %w — run `make status`", artifact, err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%s is stale — regenerating it changes it.\n"+
			"        Run `make status` and commit the result.\n"+
			"        If you got here from a merge conflict: never hand-resolve this file.\n"+
			"        Take either side (`git checkout --ours %s`), then run `make status`", artifact, artifact)
	}
	fmt.Printf("\033[32m  OK  \033[0m %s matches the tree\n", artifact)
	return nil
}

// render produces the artifact's exact bytes.
func render(debt cli.StatusDebt) ([]byte, error) {
	var buf bytes.Buffer
	if err := cli.WriteStatus(&buf, debt); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ledgerReport is scripts/ledger-check.py --json's shape.
type ledgerReport struct {
	Total   int `json:"total"`
	Open    int `json:"open"`
	Skipped int `json:"skipped"`
}

// readLedger asks scripts/ledger-check.py for the ledger's shape.
//
// python3 is already a hard dependency of this repo's gates: `make
// ledger-check` invokes it directly and the release workflow relies on it. The
// error says so, in the CLI's grammar, rather than failing as a bare exec
// error.
func readLedger() (cli.StatusDebt, error) {
	cmd := exec.Command("python3", ledgerScript, "--json")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return cli.StatusDebt{}, fmt.Errorf("%s --json failed: %w", ledgerScript, err)
		}
		return cli.StatusDebt{}, fmt.Errorf("running python3 %s: %w\n"+
			"        python3 is required: `make ledger-check` and the release workflow already need it",
			ledgerScript, err)
	}

	var rep ledgerReport
	if err := json.Unmarshal(stdout, &rep); err != nil {
		return cli.StatusDebt{}, fmt.Errorf("decoding %s --json: %w", ledgerScript, err)
	}
	// A skipped row is an undercount presented as a fact, so it is refused
	// here rather than published. The release gate deliberately keeps
	// tolerating it --- it is narrow on purpose, and this plan does not widen
	// it. See docs/debt.md#87.
	if rep.Skipped > 0 {
		return cli.StatusDebt{}, fmt.Errorf(
			"docs/debt.md has %d row(s) that do not parse, so the ledger counts would be wrong.\n"+
				"        Every row needs an id=\"N\" anchor and six pipe-delimited cells;\n"+
				"        a literal | inside a cell must be escaped as \\|", rep.Skipped)
	}
	return cli.StatusDebt{Total: rep.Total, Open: rep.Open, Skipped: rep.Skipped}, nil
}

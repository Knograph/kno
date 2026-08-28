package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// reportOut is the JSON report. Schema "pricingcheck/report/v1". Findings
// are split by what they do: failed_gated holds the findings that drive exit
// code 1, reported holds the report-only surface — the same split the text
// output draws with its FAIL and REPORT line prefixes.
type reportOut struct {
	Schema       string            `json:"schema"`
	TableVersion string            `json:"table_version"`
	MaxAgeDays   int               `json:"max_age_days"`
	Sources      map[string]string `json:"sources"`
	Checks       []checkResult     `json:"checks"`
	FailedGated  []finding         `json:"failed_gated"`
	Reported     []finding         `json:"reported"`
}

// buildReport assembles the report from check results and source statuses.
func buildReport(results []checkResult, sourcesMap map[string]sourceData, in checkInput) reportOut {
	rep := reportOut{
		Schema:       "pricingcheck/report/v1",
		TableVersion: in.tableVersion,
		MaxAgeDays:   in.maxAgeDays,
		Sources:      make(map[string]string, len(sources)),
	}
	for _, s := range sources {
		switch sd, ok := sourcesMap[s.name]; {
		case ok && sd.err == nil:
			rep.Sources[s.name] = "ok"
		case ok:
			rep.Sources[s.name] = "no data: " + sd.err.Error()
		default:
			rep.Sources[s.name] = "no data"
		}
	}
	for _, cr := range results {
		rep.Checks = append(rep.Checks, cr)
		for _, f := range cr.Findings {
			switch f.verdict {
			case verdictFail:
				rep.FailedGated = append(rep.FailedGated, f)
			case verdictReport:
				rep.Reported = append(rep.Reported, f)
			}
		}
	}
	return rep
}

// advisoryHeader opens the text report. The values in it are rounded for
// display; nobody should copy a rounded cent into table.go without checking
// the linked page.
const advisoryHeader = "values are advisory; verify against the linked page before editing the table"

// renderText writes the human report. Line contract: the advisory header is
// the one non-CHECK line; the CHECK summary lines are never prefixed with
// FAIL or REPORT; every detail line begins with exactly one of FAIL (gated
// finding) or REPORT (report-only finding); no other line begins with either
// word.
//
// The report is composed in a buffer and written once, so a broken pipe
// cannot split a report into a partial prefix that still looks like a run.
func renderText(out io.Writer, rep reportOut) error {
	var b strings.Builder
	fmt.Fprintln(&b, advisoryHeader)
	for _, cr := range rep.Checks {
		if len(cr.Findings) == 0 {
			fmt.Fprintf(&b, "CHECK %d: %s — %s", cr.Number, cr.Name, cr.Verdict)
			if cr.Summary != "" {
				fmt.Fprintf(&b, " (%s)", cr.Summary)
			}
			fmt.Fprintln(&b)
			continue
		}
		fmt.Fprintf(&b, "CHECK %d: %s — %s (%d finding%s)\n",
			cr.Number, cr.Name, cr.Verdict, len(cr.Findings), plural(len(cr.Findings)))
		for _, f := range cr.Findings {
			prefix := "REPORT"
			if f.verdict == verdictFail {
				prefix = "FAIL"
			}
			fmt.Fprintf(&b, "%s check %d: %s — %s\n", prefix, f.Check, f.Name, f.Detail)
		}
	}
	_, err := io.WriteString(out, b.String())
	return err
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// renderJSON writes the machine report.
func renderJSON(out io.Writer, rep reportOut) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

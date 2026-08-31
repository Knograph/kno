package cli

// Exported for tests, which assert on the --json contract without importing
// encoding/json themselves — the CLI's exemption is scoped to jsonreport.go.

// DecodeReport parses a rendered JSON report.
func DecodeReport(b []byte) (Report, error) { return decodeReport(b) }

// DecodeRaw parses a rendered JSON report into a map.
func DecodeRaw(b []byte) (map[string]any, error) { return decodeRaw(b) }

// Report is the --json shape, exposed for tests.
type Report = jsonReport

// DecodeReportJSON parses a rendered `kno report --json` document.
func DecodeReportJSON(b []byte) (ReportJSON, error) { return decodeReportJSON(b) }

// ReportJSON is the report's --json shape, exposed for tests.
type ReportJSON = reportJSON

// DecodeDemoReport parses a rendered `kno demo --json` document, refusing
// anything after it.
func DecodeDemoReport(b []byte) (DemoReport, error) { return decodeDemoReport(b) }

// DemoReport is `kno demo --json`'s envelope, exposed for tests.
type DemoReport = demoReport

// DecodeDemoBaseline parses the demo's embedded baseline stage document.
func DecodeDemoBaseline(raw []byte) (Report, error) { return decodeDemoBaseline(raw) }

// DecodeDemoValue parses the demo's embedded value stage document.
func DecodeDemoValue(raw []byte) (ValueReport, error) { return decodeDemoValue(raw) }

// ValueReport is `kno value --json`'s shape, exposed for tests.
type ValueReport = valueReport

// DecodeDemoSelect parses the demo's embedded select stage document.
func DecodeDemoSelect(raw []byte) (SelectReport, error) { return decodeDemoSelect(raw) }

// SelectReport is `kno select --json`'s shape, exposed for tests.
type SelectReport = selectReport

// DecodeEvalInspect parses a rendered `kno eval inspect --json` document,
// refusing anything after it.
func DecodeEvalInspect(b []byte) (EvalInspectReport, error) { return decodeEvalInspect(b) }

// EvalInspectReport is `kno eval inspect --json`'s shape, exposed for tests.
type EvalInspectReport = evalInspectReport

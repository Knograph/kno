package cli

// Exported for tests, which assert on the --json contract without importing
// encoding/json themselves — the CLI's exemption is scoped to jsonreport.go.

// DecodeReport parses a rendered JSON report.
func DecodeReport(b []byte) (Report, error) { return decodeReport(b) }

// DecodeRaw parses a rendered JSON report into a map.
func DecodeRaw(b []byte) (map[string]any, error) { return decodeRaw(b) }

// Report is the --json shape, exposed for tests.
type Report = jsonReport

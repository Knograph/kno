package cli

import (
	"fmt"
	"io"

	"github.com/knograph/kno/core"
)

// renderExport writes the Export report, human or machine-readable.
func renderExport(out io.Writer, jsonOut bool, res *core.ExportResult) error {
	if jsonOut {
		return renderExportJSON(out, res)
	}
	return renderExportHuman(out, res)
}

// renderExportHuman prints where the artifact went and what it holds, plus
// the idempotence contract that makes re-export safe.
func renderExportHuman(out io.Writer, res *core.ExportResult) error {
	if _, err := fmt.Fprintf(out, "Export run %s (completed)\n", res.RunID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  destination  %s\n", destinationName(res.Destination)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "  wrote        %s (%d assets, %d bytes)\n",
		res.Path, res.AssetCount, res.BytesWritten); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "  manifest     %s.manifest.md\n\n", res.Path)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "The artifact is a pure function of the Portfolio and the pool: "+
		"re-exporting is byte-identical, and export never mutates a destination.\n")
	return err
}

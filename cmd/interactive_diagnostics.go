package cmd

import (
	"fmt"
	"io"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

// startInteractiveDiagnostics must run before the renderer takes ownership.
// Opening a log is best effort, but overlapping UIs remain an error.
func startInteractiveDiagnostics(stderr io.Writer) (func(), error) {
	closeLog, warning, err := runtimeoutput.StartBestEffort(config.GetDiagnosticsDir())
	if err != nil {
		return nil, err
	}
	if warning != nil {
		fmt.Fprintf(stderr, "warning: interactive diagnostics unavailable (%v); discarding background diagnostics for this UI\n", warning)
	}
	return closeLog, nil
}

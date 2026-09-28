package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

func TestInteractiveDiagnosticsOpenFailureWarnsBeforeUIAndDiscards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", path)
	var startup bytes.Buffer
	closeLog, err := startInteractiveDiagnostics(&startup)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	if !strings.Contains(startup.String(), "discarding background diagnostics") || !runtimeoutput.Active() {
		t.Fatalf("startup warning = %q, active = %v", startup.String(), runtimeoutput.Active())
	}
	if _, err := startInteractiveDiagnostics(&startup); err != runtimeoutput.ErrAlreadyActive {
		t.Fatalf("overlap = %v", err)
	}
	runtimeoutput.Printf("hidden debug\n")
	if strings.Contains(startup.String(), "hidden debug") {
		t.Fatalf("leaked debug: %q", startup.String())
	}
	closeLog()
}

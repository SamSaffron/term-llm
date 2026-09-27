package llm

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

func TestDebugSectionsRemainRawAndAtomicInInteractiveDiagnostics(t *testing.T) {
	dir := t.TempDir()
	closeLog, err := runtimeoutput.Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 12
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); DebugRawSection(true, "Raw Request", "{\n  \"private\": \"value\"\n}") }()
	}
	wg.Wait()
	closeLog()
	data, err := os.ReadFile(filepath.Join(dir, "tui.log"))
	if err != nil {
		t.Fatal(err)
	}
	block := "Raw Request\n{\n  \"private\": \"value\"\n}\n["
	if got := strings.Count(string(data), block); got != workers {
		t.Fatalf("raw atomic blocks = %d, want %d: %s", got, workers, data)
	}
	if strings.Contains(string(data), `\\n`) || strings.Contains(string(data), `msg=`) {
		t.Fatalf("escaped or structured debug content: %s", data)
	}
}

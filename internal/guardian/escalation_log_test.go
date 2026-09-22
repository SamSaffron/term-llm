package guardian

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testEscalation() Escalation {
	return Escalation{
		SchemaVersion:       EscalationSchemaVersion,
		Timestamp:           time.Date(2026, 9, 22, 5, 0, 0, 0, time.UTC),
		ScopeID:             "session-1",
		ClassifyRequestSent: true,
		MinConfidence:       0.5,
		ClassifyStage:       classifyStageDecision,
		FallbackProvider:    "chatgpt",
		FallbackModel:       "gpt-5.6-luna-low",
	}
}

func readEscalationLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSuffix(string(raw), "\n")
	if text == "" {
		t.Fatal("escalation log is empty")
	}
	lines := strings.Split(text, "\n")
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line %q is not JSON: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func TestFileEscalationLoggerAppendsJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guardian", "nested", "escalations.jsonl")
	var warnings []string
	logger := NewFileEscalationLogger(path)
	logger.warn = func(message string) { warnings = append(warnings, message) }

	for i := 0; i < 3; i++ {
		if err := logger.LogEscalation(testEscalation()); err != nil {
			t.Fatalf("LogEscalation: %v", err)
		}
	}
	records := readEscalationLines(t, path)
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}
	for _, record := range records {
		if record["schema_version"] != float64(EscalationSchemaVersion) || record["classify_stage"] != classifyStageDecision {
			t.Fatalf("record = %+v", record)
		}
		if _, ok := record["fallback_decision"]; ok {
			t.Fatalf("empty verdicts must stay omitted: %+v", record)
		}
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions = %o, want 600", perm)
	}
	if dir, err := os.Stat(filepath.Dir(path)); err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatalf("parent directory: %v %v", dir, err)
	}
}

func TestFileEscalationLoggerRefusesNonRegularFiles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"symlink", func(t *testing.T, path string) {
			target := path + ".target"
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, path string) {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty path", func(*testing.T, string) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "escalations.jsonl")
			if tc.name == "empty path" {
				path = ""
			}
			tc.setup(t, path)
			var warnings []string
			logger := NewFileEscalationLogger(path)
			logger.warn = func(message string) { warnings = append(warnings, message) }
			for i := 0; i < 2; i++ {
				if err := logger.LogEscalation(testEscalation()); err == nil {
					t.Fatal("accepted an unwritable escalation path")
				}
			}
			if len(warnings) != 1 {
				t.Fatalf("warnings = %v, want exactly one", warnings)
			}
			if !strings.Contains(warnings[0], "guardian: escalation log") {
				t.Fatalf("warning = %q", warnings[0])
			}
			if tc.name == "symlink" {
				target, err := os.ReadFile(path + ".target")
				if err != nil {
					t.Fatal(err)
				}
				if len(target) != 0 {
					t.Fatalf("wrote through a symlink: %q", target)
				}
			}
			if tc.name == "directory" {
				entries, err := os.ReadDir(path)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Fatalf("wrote inside a directory used as the log path: %v", entries)
				}
			}
		})
	}
}

func TestFileEscalationLoggerConcurrentAppendsStayIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "escalations.jsonl")
	logger := NewFileEscalationLogger(path)
	logger.warn = func(message string) { t.Error(message) }

	const writers, perWriter = 8, 10
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				if err := logger.LogEscalation(testEscalation()); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("LogEscalation: %v", err)
	}
	records := readEscalationLines(t, path)
	if len(records) != writers*perWriter {
		t.Fatalf("records = %d, want %d", len(records), writers*perWriter)
	}
}

func TestFileEscalationLoggerNilSafe(t *testing.T) {
	var logger *FileEscalationLogger
	if err := logger.LogEscalation(testEscalation()); err != nil {
		t.Fatalf("nil logger: %v", err)
	}
}

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestDebugLogExportDefaultRedaction(t *testing.T) {
	flag := debugLogExportCmd.Flags().Lookup("redact")
	if flag == nil || flag.DefValue != "true" {
		t.Errorf("export --redact must default to true, got %v", flag)
	}
	setupDebugLogExportFixture(t)
	oldRedact, oldRaw, oldMarkdown := debugLogRedact, debugLogRaw, debugLogMarkdown
	defer func() { debugLogRedact, debugLogRaw, debugLogMarkdown = oldRedact, oldRaw, oldMarkdown }()
	oldOut, oldErr := debugLogExportCmd.OutOrStdout(), debugLogExportCmd.ErrOrStderr()
	defer func() { debugLogExportCmd.SetOut(oldOut); debugLogExportCmd.SetErr(oldErr) }()
	for _, format := range []string{"json", "markdown", "raw"} {
		t.Run(format, func(t *testing.T) {
			var warnings bytes.Buffer
			out, err := os.CreateTemp(t.TempDir(), "export-*")
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			oldStdout := os.Stdout
			os.Stdout = out
			defer func() { os.Stdout = oldStdout }()
			debugLogExportCmd.SetOut(out)
			debugLogExportCmd.SetErr(&warnings)
			debugLogRedact = flag.DefValue == "true"
			debugLogRaw, debugLogMarkdown = format == "raw", format == "markdown"
			if err := debugLogExportCmd.RunE(debugLogExportCmd, []string{"1"}); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(out.Name())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(content), "private CLI prompt") || !strings.Contains(string(content), "[REDACTED]") {
				t.Fatalf("unsafe default export: %s", content)
			}
		})
	}
}

func setupDebugLogExportFixture(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := config.GetDebugLogsDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	line := `{"timestamp":"2026-10-28T00:00:00Z","type":"request","provider":"OpenAI","model":"test","request":{"messages":[{"role":"user","content":"private CLI prompt"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "session-1.jsonl"), []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDebugLogExportUnsafeOptOut(t *testing.T) {
	setupDebugLogExportFixture(t)
	flags := debugLogExportCmd.Flags()
	redactFlag := flags.Lookup("redact")
	oldValue, oldChanged := redactFlag.Value.String(), redactFlag.Changed
	oldRaw, oldMarkdown := debugLogRaw, debugLogMarkdown
	oldOut, oldErr := debugLogExportCmd.OutOrStdout(), debugLogExportCmd.ErrOrStderr()
	defer func() {
		_ = flags.Set("redact", oldValue)
		redactFlag.Changed = oldChanged
		debugLogRaw, debugLogMarkdown = oldRaw, oldMarkdown
		debugLogExportCmd.SetOut(oldOut)
		debugLogExportCmd.SetErr(oldErr)
	}()
	// Exercise Cobra's actual bool flag parser, including explicit false.
	if err := debugLogExportCmd.ParseFlags([]string{"--redact=false"}); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"json", "markdown", "raw"} {
		t.Run(format, func(t *testing.T) {
			var out, warnings bytes.Buffer
			debugLogExportCmd.SetOut(&out)
			debugLogExportCmd.SetErr(&warnings)
			debugLogRaw, debugLogMarkdown = format == "raw", format == "markdown"
			if err := debugLogExportCmd.RunE(debugLogExportCmd, []string{"1"}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "private CLI prompt") {
				t.Fatal("explicit unsafe export lost diagnostic content")
			}
			for _, want := range []string{"WARNING", "redaction disabled", "credentials", "review before sharing"} {
				if !strings.Contains(warnings.String(), want) {
					t.Errorf("unsafe export warning missing %q: %s", want, warnings.String())
				}
			}
			if strings.Contains(out.String(), "WARNING") {
				t.Fatal("warning corrupted machine-readable stdout")
			}
		})
	}
}

func TestDebugLogExportHelpExplainsRedactionLimits(t *testing.T) {
	for _, want := range []string{"ON by default", "including --raw", "prompts, responses", "tool arguments/results/info", "--redact=false", "cannot be guaranteed", "show, tail, and search"} {
		if !strings.Contains(debugLogExportCmd.Long, want) {
			t.Errorf("export help missing %q", want)
		}
	}
}

func TestDebugLogShowKeepsLocalDiagnosticContent(t *testing.T) {
	setupDebugLogExportFixture(t)
	oldRaw, oldJSON := debugLogRaw, debugLogJSON
	defer func() { debugLogRaw, debugLogJSON = oldRaw, oldJSON }()
	for _, format := range []string{"json", "raw"} {
		t.Run(format, func(t *testing.T) {
			out, err := os.CreateTemp(t.TempDir(), "show-*")
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			oldStdout := os.Stdout
			os.Stdout = out
			defer func() { os.Stdout = oldStdout }()
			debugLogRaw, debugLogJSON = format == "raw", format == "json"
			if err := debugLogShowCmd.RunE(debugLogShowCmd, []string{"1"}); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(out.Name())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(content), "private CLI prompt") {
				t.Fatal("local show unexpectedly redacted diagnostic content")
			}
		})
	}
}

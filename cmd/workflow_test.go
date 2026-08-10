package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowCommandsAndCapabilityFlagsAreRegistered(t *testing.T) {
	if workflowCmd.Commands()[0] == nil || workflowRunCmd == nil || workflowValidateCmd == nil {
		t.Fatal("workflow commands are not registered")
	}
	for _, name := range []string{"input", "input-json", "agent", "provider", "concurrency", "agent-timeout", "timeout", "agent-tool", "agent-read-dir", "agent-write-dir", "agent-shell-allow", "workspace-root", "json"} {
		if workflowRunCmd.Flags().Lookup(name) == nil {
			t.Fatalf("workflow run flag --%s is not registered", name)
		}
	}
	if rootCmd.PersistentFlags().Lookup("workflow-db") != nil || rootCmd.PersistentFlags().Lookup("workflow-dir") != nil {
		t.Fatal("minimal workflow command must not register persistence flags")
	}
}

func TestParseWorkflowInputsMergesJSONAndRepeatablePairs(t *testing.T) {
	inputs, err := parseWorkflowInputs(`{"topic":"old","count":2,"nested":{"ok":true}}`, []string{"topic=new", "empty="})
	if err != nil {
		t.Fatalf("parseWorkflowInputs: %v", err)
	}
	if inputs["topic"] != "new" || inputs["empty"] != "" || inputs["count"] != float64(2) {
		t.Fatalf("inputs = %#v", inputs)
	}
	if _, err := parseWorkflowInputs(`{"ok":true} {"extra":true}`, nil); err == nil {
		t.Fatal("parseWorkflowInputs accepted trailing JSON")
	}
	if _, err := parseWorkflowInputs("", []string{"missing-separator"}); err == nil {
		t.Fatal("parseWorkflowInputs accepted malformed key=value")
	}
}

func TestWorkflowValidateReadsExplicitFileWithoutExecutingBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review.lua")
	source := `workflow { name = "review", description = "test" }
error("must not execute")
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	oldJSON := workflowValidateJSON
	workflowValidateJSON = false
	t.Cleanup(func() { workflowValidateJSON = oldJSON })
	oldOutput := workflowValidateCmd.OutOrStdout()
	workflowValidateCmd.SetOut(&output)
	t.Cleanup(func() { workflowValidateCmd.SetOut(oldOutput) })
	if err := runWorkflowValidate(workflowValidateCmd, []string{path}); err != nil {
		t.Fatalf("runWorkflowValidate: %v", err)
	}
	if !strings.Contains(output.String(), "review: valid") {
		t.Fatalf("output = %q", output.String())
	}
}

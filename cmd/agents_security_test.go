package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
)

func TestAgentsLocalListRequiresExplicitSelection(t *testing.T) {
	old := agentsLocal
	agentsLocal = true
	t.Cleanup(func() { agentsLocal = old })
	if err := runAgentsList(agentsCmd, nil); err == nil || !strings.Contains(err.Error(), "--agent ./term-llm-agents/<name>") {
		t.Fatalf("local listing error = %v, want explicit-path guidance", err)
	}
}

func TestAgentsLocalCreationPrintsExplicitPath(t *testing.T) {
	for _, command := range []string{"new", "copy"} {
		t.Run(command, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeServeContractConfig(t, "default_provider: debug\n")
			old := agentsLocal
			agentsLocal = true
			t.Cleanup(func() { agentsLocal = old })
			var err error
			output := captureStdout(t, func() {
				if command == "new" {
					err = runAgentsNew(agentsNewCmd, []string{"my-agent"})
				} else {
					err = runAgentsCopy(agentsCopyCmd, []string{"reviewer", "my-agent"})
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = appruntime.LoadAgent("./term-llm-agents/my-agent", &config.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output, fmt.Sprintf("--agent %q", "./term-llm-agents/my-agent")) {
				t.Errorf("missing explicit-path usage in %q", output)
			}
			if strings.Contains(output, "--agent my-agent") || strings.Contains(output, "agents edit my-agent") {
				t.Errorf("output suggests resolving local agent by name: %q", output)
			}
		})
	}
}

func TestAgentsPathReportsOnlyTrustedSearchDirectories(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	searchDir := t.TempDir()
	writeServeContractConfig(t, fmt.Sprintf("default_provider: debug\nagents:\n  search_paths: [%q]\n", searchDir))
	var err error
	output := captureStdout(t, func() { err = runAgentsPath(agentsPathCmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, filepath.Join(project, "term-llm-agents")) || !strings.Contains(output, "not auto-discovered") {
		t.Errorf("agent path output advertises implicit project discovery: %q", output)
	}
	if !strings.Contains(output, "configured: "+searchDir) || !strings.Contains(output, "builtin:") {
		t.Errorf("agent path output missing trusted sources: %q", output)
	}
}

func TestAgentsPathWorksWithoutUsableConfig(t *testing.T) {
	for _, contents := range []string{"", "agents: [invalid\n"} {
		t.Run(contents, func(t *testing.T) {
			writeServeContractConfig(t, contents)
			if contents == "" {
				path, err := config.GetConfigPath()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			var warnings bytes.Buffer
			oldStderr := agentsPathCmd.ErrOrStderr()
			agentsPathCmd.SetErr(&warnings)
			t.Cleanup(func() { agentsPathCmd.SetErr(oldStderr) })
			var err error
			output := captureStdout(t, func() { err = runAgentsPath(agentsPathCmd, nil) })
			if err != nil || !strings.Contains(output, "user:") || !strings.Contains(output, "not auto-discovered") {
				t.Fatalf("agent path diagnostic = %q, %v; want directories without setup", output, err)
			}
			if contents != "" && !strings.Contains(warnings.String(), "showing defaults") {
				t.Errorf("invalid config fallback missing warning: %q", warnings.String())
			}
			if contents == "" {
				path, err := config.GetConfigPath()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("path diagnostic created config: %v", err)
				}
			}
		})
	}
}

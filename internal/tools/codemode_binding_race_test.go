package tools

import (
	"context"
	"sync"
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/modelpolicy"
)

// TestCodeModeBindingCaptureConcurrentWithWiring exercises the documented
// synchronization contract: registry mutators take the registry lock, tool
// mutators take the tool lock, and admission snapshots both.
func TestCodeModeBindingCaptureConcurrentWithWiring(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultToolConfig()
	cfg.BaseDir = dir
	cfg.ReadDirs = []string{dir}
	cfg.Workspace = "none"
	cfg.Enabled = []string{SpawnAgentToolName, QueueAgentToolName, WaitForJobsToolName, UpdatePlanToolName, ReadFileToolName}
	manager, err := NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := manager.Registry
	live := registry.GetSpawnAgentTool()
	if live == nil {
		t.Fatal("missing spawn tool")
	}
	runner := &bindingAgentRunner{}
	live.SetRunner(runner)

	names := []string{ReadFileToolName, SpawnAgentToolName, WaitAgentToolName, QueueAgentToolName, WaitForJobsToolName, UpdatePlanToolName}
	state := func() (modelpolicy.Policy, modelpolicy.ParentModel) {
		return modelpolicy.Policy{}.With("developer", []string{"mock:*"}), modelpolicy.ParentModel{Provider: "mock", Model: "parent"}
	}
	check := func(context.Context, string, string, modelpolicy.Policy, modelpolicy.ParentModel) error { return nil }

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				authority, err := registry.captureCodeModeAuthority(context.Background(), core.CodeModeCellRef{SessionID: "race-parent", Ctx: context.Background()}, names, func(core.CodeModeCellRef) CodeModePrompts { return CodeModePrompts{} })
				if err != nil {
					t.Errorf("capture: %v", err)
					return
				}
				authority.Close()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 24; j++ {
			registry.SetJobsServer("http://127.0.0.1:9", "")
			registry.SetAgentModelAdmission(state, check)
			live.SetDepth(j % 3)
			live.SetRemainingDepth(2 - j%3)
			live.SetRunner(runner)
			if err := registry.SetBaseDir(dir); err != nil {
				t.Errorf("set base dir: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

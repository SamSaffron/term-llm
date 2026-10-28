package serve

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestDirectSkillScriptExecutionPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires /bin/sh")
	}
	for _, isolated := range []bool{false, true} {
		kind := "main"
		if isolated {
			kind = "isolated"
		}
		for _, tc := range []struct {
			name          string
			userInstalled bool
			optIn         bool
			yolo          bool
			allowed       bool
		}{
			{name: "project default"},
			{name: "project opt-in", optIn: true, allowed: true},
			{name: "project yolo", yolo: true, allowed: true},
			{name: "user installed", userInstalled: true, allowed: true},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				cfg := &config.Config{Skills: config.SkillsConfig{AllowProjectScriptTools: tc.optIn}}
				manager, err := tools.NewToolManager(&tools.ToolConfig{BaseDir: dir}, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if tc.yolo {
					manager.ApprovalMgr.SetApprovalMode(tools.ModeYolo)
				}
				eng := engine.NewEngine(nil, nil)
				defs := []skills.SkillToolDef{{Name: "check_build", Description: "Check build", Script: "check.sh", UserInstalled: tc.userInstalled}}
				if isolated {
					err = applyChildSkillRuntime(eng, manager, &runpkg.SkillRunMetadata{SourcePath: dir, ToolDefs: defs})
				} else {
					err = setupServeMainSkillRequest(&serveRuntime{engine: eng, toolMgr: manager}, &skills.Activation{BaseDir: dir, ToolDefs: defs}, &core.Request{})
				}
				if err != nil {
					t.Fatal(err)
				}
				tool, ok := eng.Tools().Get("check_build")
				if !ok {
					t.Fatal("direct skill tool was not registered")
				}
				output, err := tool.Execute(context.Background(), nil)
				if err != nil || output.IsError == tc.allowed {
					t.Fatalf("allowed=%v: output=%+v err=%v", tc.allowed, output, err)
				}
			})
		}
	}
}

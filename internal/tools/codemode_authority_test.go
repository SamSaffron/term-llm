package tools

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestCodeModeAuthorityKeepsOriginalDirectoryAndMode(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	for dir, value := range map[string]string{d1: "original", d2: "later"} {
		if err := os.WriteFile(filepath.Join(dir, "file"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	config := DefaultToolConfig()
	config.Enabled = []string{CodeExecToolName, ReadFileToolName}
	config.BaseDir = d1
	config.ReadDirs = []string{d1, d2}
	config.Workspace = "none"
	manager, err := NewToolManager(&config, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := manager.ApprovalMgr
	root.SetApprovalMode(ModePrompt)
	authority, err := manager.Registry.captureCodeModeAuthority(context.Background(), core.CodeModeCellRef{SessionID: "original", CellID: "cell", Ctx: context.Background()}, []string{ReadFileToolName}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := authority.Tool(ReadFileToolName)
	if !ok {
		t.Fatal("missing read")
	}
	manager.Registry.config.UpdateBaseDir(d2)
	root.SetApprovalMode(ModeYolo)
	out, err := tool.Execute(context.Background(), []byte(`{"path":"file"}`))
	if err != nil || !strings.Contains(out.Content, "original") {
		t.Fatalf("captured tool retargeted: %+v %v", out, err)
	}
	captured := tool.(*ReadFileTool).approval
	if captured.ApprovalMode() != ModePrompt {
		t.Fatal("later yolo widened original authority")
	}
	if captured.PromptLock() == root.PromptLock() {
		t.Fatal("cell prompt blocks next-turn prompts")
	}
	root.SetApprovalMode(ModeYolo)
	authority, err = manager.Registry.captureCodeModeAuthority(context.Background(), core.CodeModeCellRef{SessionID: "original", CellID: "other", Ctx: context.Background()}, []string{ReadFileToolName}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tool, _ = authority.Tool(ReadFileToolName)
	root.SetApprovalMode(ModePrompt)
	if tool.(*ReadFileTool).approval.ApprovalMode() != ModePrompt {
		t.Fatal("tightened mode not observed")
	}
}
func TestCodeModeRegistryFieldsClassified(t *testing.T) {
	// A future adapter must be consciously copied, rebuilt, or excluded.
	want := []string{"mu", "config", "permissions", "approval", "limits", "appConfig", "grepReranker", "memoryStore", "agent", "sessionID", "fileRecorder", "mediaPublisher", "collaborativeShellController", "shellRoutingMode", "tools"}
	var got []string
	typ := reflect.TypeOf(LocalToolRegistry{})
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("classify new registry fields: %v", got)
	}
}

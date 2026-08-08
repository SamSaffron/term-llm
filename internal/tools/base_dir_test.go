package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustToolArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal args: %v", err)
	}
	return data
}

func TestToolsResolveRelativePathsAgainstBaseDir(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	cfg := &ToolConfig{BaseDir: base}
	ctx := context.Background()

	writeOut, err := NewWriteFileTool(nil, cfg).Execute(ctx, mustToolArgs(t, WriteFileArgs{Path: "notes/todo.txt", Content: "alpha\nneedle\n"}))
	if err != nil {
		t.Fatalf("write Execute: %v", err)
	}
	if writeOut.IsError {
		t.Fatalf("write returned tool error: %s", writeOut.Content)
	}
	if got, err := os.ReadFile(filepath.Join(base, "notes", "todo.txt")); err != nil || string(got) != "alpha\nneedle\n" {
		t.Fatalf("written file = %q, %v", string(got), err)
	}

	readOut, err := NewReadFileTool(nil, DefaultOutputLimits(), cfg).Execute(ctx, mustToolArgs(t, ReadFileArgs{Path: "notes/todo.txt"}))
	if err != nil {
		t.Fatalf("read Execute: %v", err)
	}
	if !strings.Contains(readOut.Content, "2: needle") {
		t.Fatalf("read output = %q, want line from BaseDir file", readOut.Content)
	}

	globOut, err := NewGlobTool(nil, cfg).Execute(ctx, mustToolArgs(t, GlobArgs{Pattern: "notes/*.txt"}))
	if err != nil {
		t.Fatalf("glob Execute: %v", err)
	}
	if !strings.Contains(globOut.Content, filepath.Join(base, "notes", "todo.txt")) {
		t.Fatalf("glob output = %q, want BaseDir path", globOut.Content)
	}

	grepOut, err := NewGrepTool(nil, DefaultOutputLimits(), cfg).Execute(ctx, mustToolArgs(t, GrepArgs{Pattern: "needle", Path: "notes"}))
	if err != nil {
		t.Fatalf("grep Execute: %v", err)
	}
	if !strings.Contains(grepOut.Content, "needle") || !strings.Contains(grepOut.Content, filepath.Join(base, "notes", "todo.txt")) {
		t.Fatalf("grep output = %q, want BaseDir match", grepOut.Content)
	}

	shellOut, err := NewShellTool(nil, cfg, DefaultOutputLimits()).Execute(ctx, mustToolArgs(t, ShellArgs{Command: "pwd"}))
	if err != nil {
		t.Fatalf("shell Execute: %v", err)
	}
	if !strings.Contains(shellOut.Content, base) {
		t.Fatalf("shell output = %q, want pwd in BaseDir %q", shellOut.Content, base)
	}
}

func TestUnboundDaemonToolsRejectAmbientCWD(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "marker.txt"), []byte("bound marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &ToolConfig{RequireExplicitWorkingDir: true}
	ctx := context.Background()
	if errs := (&ToolConfig{RequireExplicitWorkingDir: true, ReadDirs: []string{"relative-read"}, WriteDirs: []string{"relative-write"}}).Validate(); len(errs) != 2 {
		t.Fatalf("relative unbound allowlist validation = %v", errs)
	}
	mgr, err := NewToolManager(&ToolConfig{Enabled: []string{ReadFileToolName}, RequireExplicitWorkingDir: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetBaseDir("relative-binding"); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("relative unbound binding = %v", err)
	}
	if err := mgr.SetBaseDir(base); err != nil {
		t.Fatalf("absolute binding = %v", err)
	}

	readOut, err := NewReadFileTool(nil, DefaultOutputLimits(), cfg).Execute(ctx, mustToolArgs(t, ReadFileArgs{Path: "marker.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readOut.Content, "relative path requires") {
		t.Fatalf("unbound relative read = %#v", readOut)
	}
	globOut, err := NewGlobTool(nil, cfg).Execute(ctx, mustToolArgs(t, GlobArgs{Pattern: "*.go"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(globOut.Content, "relative path requires") {
		t.Fatalf("unbound default glob = %#v", globOut)
	}
	shellOut, err := NewShellTool(nil, cfg, DefaultOutputLimits()).Execute(ctx, mustToolArgs(t, ShellArgs{Command: "pwd"}))
	if err != nil {
		t.Fatal(err)
	}
	if !shellOut.IsError || !strings.Contains(shellOut.Content, "working_dir is required") {
		t.Fatalf("unbound default shell = %#v", shellOut)
	}
	shellOut, err = NewShellTool(nil, cfg, DefaultOutputLimits()).Execute(ctx, mustToolArgs(t, ShellArgs{Command: "pwd", WorkingDir: "relative"}))
	if err != nil {
		t.Fatal(err)
	}
	if !shellOut.IsError || !strings.Contains(shellOut.Content, "relative working_dir requires") {
		t.Fatalf("unbound relative shell dir = %#v", shellOut)
	}

	readOut, err = NewReadFileTool(nil, DefaultOutputLimits(), cfg).Execute(ctx, mustToolArgs(t, ReadFileArgs{Path: filepath.Join(base, "marker.txt")}))
	if err != nil || readOut.IsError || !strings.Contains(readOut.Content, "bound marker") {
		t.Fatalf("absolute read = %#v, %v", readOut, err)
	}
	shellOut, err = NewShellTool(nil, cfg, DefaultOutputLimits()).Execute(ctx, mustToolArgs(t, ShellArgs{Command: "pwd", WorkingDir: base}))
	if err != nil || shellOut.IsError || !strings.Contains(shellOut.Content, base) {
		t.Fatalf("absolute shell dir = %#v, %v", shellOut, err)
	}

	cfg.UpdateBaseDir(base)
	readOut, err = NewReadFileTool(nil, DefaultOutputLimits(), cfg).Execute(ctx, mustToolArgs(t, ReadFileArgs{Path: "marker.txt"}))
	if err != nil || readOut.IsError || !strings.Contains(readOut.Content, "bound marker") {
		t.Fatalf("bound relative read = %#v, %v", readOut, err)
	}
	shellOut, err = NewShellTool(nil, cfg, DefaultOutputLimits()).Execute(ctx, mustToolArgs(t, ShellArgs{Command: "pwd"}))
	if err != nil || shellOut.IsError || !strings.Contains(shellOut.Content, base) {
		t.Fatalf("bound default shell = %#v, %v", shellOut, err)
	}
}

func TestToolManagerSetBaseDirUpdatesRegisteredTools(t *testing.T) {
	t.Parallel()

	first := t.TempDir()
	second := t.TempDir()
	cfg := &ToolConfig{Enabled: []string{ReadFileToolName}, BaseDir: first}
	mgr, err := NewToolManager(cfg, nil)
	if err != nil {
		t.Fatalf("NewToolManager: %v", err)
	}
	if err := os.WriteFile(filepath.Join(second, "marker.txt"), []byte("from second\n"), 0o644); err != nil {
		t.Fatalf("WriteFile marker: %v", err)
	}
	if err := mgr.SetBaseDir(second); err != nil {
		t.Fatalf("SetBaseDir: %v", err)
	}
	mgr.ApprovalMgr.WorkspacePromptFunc = func(workspace string) (WorkspaceApprovalResult, error) {
		if workspace != second {
			t.Fatalf("workspace = %q, want %q", workspace, second)
		}
		return WorkspaceApprovalResult{Approved: true}, nil
	}
	tool, ok := mgr.Registry.tools[ReadFileToolName].(*ReadFileTool)
	if !ok {
		t.Fatalf("read_file tool missing from registry")
	}
	out, err := tool.Execute(context.Background(), mustToolArgs(t, ReadFileArgs{Path: "marker.txt"}))
	if err != nil {
		t.Fatalf("read Execute: %v", err)
	}
	if !strings.Contains(out.Content, "from second") {
		t.Fatalf("read output = %q, want file from updated BaseDir", out.Content)
	}
}

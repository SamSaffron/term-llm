package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/app/ask"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestSetupToolManagerKeepsAutoToolsScopedAndMergesExplicitTools(t *testing.T) {
	cfg := &config.Config{Tools: config.ToolsConfig{Enabled: []string{tools.GrepToolName}}}
	defaultsEngine := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	defaults := SessionSettings{AutoTools: []string{tools.ReadFileToolName}}
	defaultsMgr, err := defaults.SetupToolManager(cfg, defaultsEngine)
	if err != nil {
		t.Fatalf("SetupToolManager defaults: %v", err)
	}
	defer defaultsMgr.ApprovalMgr.Close()
	if _, ok := defaultsMgr.Registry.Get(tools.ReadFileToolName); !ok {
		t.Fatalf("auto tool %q was not registered", tools.ReadFileToolName)
	}
	if _, ok := defaultsMgr.Registry.Get(tools.GrepToolName); ok {
		t.Fatal("auto tools unexpectedly activated the global default tool set")
	}

	engine := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	settings := SessionSettings{
		Tools:     tools.ShellToolName,
		AutoTools: []string{tools.ReadFileToolName, tools.ViewImageToolName},
	}
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		t.Fatalf("SetupToolManager: %v", err)
	}
	defer toolMgr.ApprovalMgr.Close()
	for _, name := range []string{tools.ShellToolName, tools.ReadFileToolName, tools.ViewImageToolName} {
		if _, ok := toolMgr.Registry.Get(name); !ok {
			t.Fatalf("tool %q was not registered", name)
		}
	}
	if _, ok := toolMgr.Registry.Get(tools.GrepToolName); ok {
		t.Fatal("CLI tool selection should still replace config defaults")
	}
	if settings.Tools != tools.ShellToolName {
		t.Fatalf("raw configured Tools changed to %q", settings.Tools)
	}
}

func TestDeriveAskResumeAutoToolsFromMissingStructuredAttachment(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sess := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock", Mode: session.ModeAsk}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	message := core.Message{Role: core.RoleUser, Parts: []core.Part{{
		Type: core.PartImage, ImagePath: filepath.Join(t.TempDir(), "missing.png"), ImageData: &core.ToolImageData{MediaType: "image/png"},
	}}}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, message, -1)); err != nil {
		t.Fatal(err)
	}
	settings := SessionSettings{Tools: tools.ShellToolName}
	deriveAskResumeAutoTools(ctx, store, sess, &settings)
	if settings.Tools != tools.ShellToolName {
		t.Fatalf("configured Tools changed to %q", settings.Tools)
	}
	if strings.Join(settings.AutoTools, ",") != tools.ReadFileToolName+","+tools.ViewImageToolName {
		t.Fatalf("AutoTools = %v", settings.AutoTools)
	}
}

func TestAskAttachmentGrantsAreExactAndStructured(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := uploadsReadDir()
	attached := filepath.Join(root, "attached.bin")
	sibling := filepath.Join(root, "sibling.bin")
	for _, path := range []string{attached, sibling} {
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escaped.bin")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cfg := &config.Config{}
	engine := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	settings := SessionSettings{AutoTools: []string{tools.ReadFileToolName, tools.ViewImageToolName}}
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		t.Fatalf("SetupToolManager: %v", err)
	}
	defer toolMgr.ApprovalMgr.Close()
	messages := []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{
			{Type: core.PartImage, ImagePath: attached},
			{Type: core.PartFile, FilePath: link},
			{Type: core.PartText, Text: sibling},
		}},
		{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartFile, FilePath: sibling}}},
	}
	ask.GrantUploadedFileReads(toolMgr, messages)

	if outcome, err := toolMgr.ApprovalMgr.CheckPathApproval(tools.ReadFileToolName, attached, attached, false); err != nil || outcome != tools.ProceedOnce {
		t.Fatalf("attached read approval = %v, %v", outcome, err)
	}
	if outcome, err := toolMgr.ApprovalMgr.CheckPathApproval(tools.ViewImageToolName, attached, attached, false); err != nil || outcome != tools.ProceedOnce {
		t.Fatalf("attached image approval = %v, %v", outcome, err)
	}
	for _, path := range []string{sibling, outside, link} {
		if outcome, err := toolMgr.ApprovalMgr.CheckPathApproval(tools.ReadFileToolName, path, path, false); err == nil || outcome != tools.Cancel {
			t.Fatalf("unexpected approval for %q = %v, %v", path, outcome, err)
		}
	}
}

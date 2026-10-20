package ask

import (
	"os"
	"path/filepath"
	"testing"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/app/uploads"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestAskAttachmentGrantsAreExactAndStructured(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := uploads.ReadDir()
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
	settings := appserve.SessionSettings{AutoTools: []string{tools.ReadFileToolName, tools.ViewImageToolName}}
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
	GrantUploadedFileReads(toolMgr, messages)

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

package chat

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	"github.com/term-llm/term-llm/internal/tools"
	tuichat "github.com/term-llm/term-llm/internal/tui/chat"
)

func TestCodeModeTerminalPromptsSerializeAndCancelWithTheirCells(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("not preapproved"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := tools.DefaultToolConfig()
	cfg.Enabled = []string{"spawn_script", "read_file"}
	cfg.BaseDir = dir
	cfg.Workspace = "none"
	manager, err := tools.NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := engine.NewEngine(mockprovider.NewMockProvider("terminal-cell"), nil)
	defer e.CloseCodeMode()
	manager.SetupEngine(e)
	messages := make(chan tea.Msg, 16)
	bindChatCodeModePrompts(manager, func(msg tea.Msg) { messages <- msg })
	ctx := core.ContextWithSessionID(context.Background(), "terminal")
	first, err := e.ExecCodeMode(ctx, `text('ready');await tools.read_file({path:'file'});store('bad',1)`, time.Second, 0)
	if err != nil || first.IsError {
		t.Fatalf("first %+v %v", first, err)
	}
	var request tuichat.ApprovalRequestMsg
	select {
	case msg := <-messages:
		var ok bool
		request, ok = msg.(tuichat.ApprovalRequestMsg)
		if !ok {
			t.Fatalf("request %T", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("no terminal prompt")
	}
	if request.Context == nil || !request.NoAutoApprove {
		t.Fatal("terminal prompt lost original cell authority/context")
	}
	second, err := e.ExecCodeMode(ctx, `text('ready');await tools.read_file({path:'file'})`, time.Second, 0)
	if err != nil || second.IsError {
		t.Fatalf("second %+v %v", second, err)
	}
	select {
	case msg := <-messages:
		t.Fatalf("single-foreground terminal received concurrent prompt %T", msg)
	default:
	}
	// A later yolo toggle must not widen either admitted prompt-mode cell.
	manager.ApprovalMgr.SetApprovalMode(tools.ModeYolo)
	cancelled, err := e.CancelCodeMode(ctx, first.CellID)
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatalf("cancel %+v %v", cancelled, err)
	}
	deadline := time.After(3 * time.Second)
	dismissed, next := false, false
	for !dismissed || !next {
		select {
		case msg := <-messages:
			switch msg := msg.(type) {
			case tuichat.DismissApprovalMsg:
				dismissed = dismissed || msg.DoneCh == request.DoneCh
			case tuichat.ApprovalRequestMsg:
				next = true
				if !msg.NoAutoApprove {
					t.Fatal("live yolo widened sibling authority")
				}
			}
		case <-deadline:
			t.Fatal("cell cancellation did not release terminal transport")
		}
	}
	e.InterruptCodeModeSession("terminal")
	last, err := e.WaitCodeMode(ctx, second.CellID, time.Second, 0)
	if err != nil || last.Status != "cancelled" {
		t.Fatalf("Stop %+v %v", last, err)
	}
	deadline = time.After(3 * time.Second)
	for e.HasAnyCodeModeCells() {
		select {
		case <-deadline:
			t.Fatal("terminal approval kept actual lease/admission place")
		default:
			runtime.Gosched()
		}
	}
}

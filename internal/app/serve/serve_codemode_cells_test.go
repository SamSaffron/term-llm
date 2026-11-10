package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/tools"
)

func cellTestRuntime(t *testing.T, id, dir string, allow bool) *serveRuntime {
	t.Helper()
	cfg := tools.DefaultToolConfig()
	cfg.Enabled = []string{"spawn_script", "read_file"}
	cfg.BaseDir = dir
	cfg.Workspace = "none"
	if allow {
		cfg.ReadDirs = []string{dir}
	}
	manager, err := tools.NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := engine.NewEngine(mockprovider.NewMockProvider("cells"), nil)
	rt := &serveRuntime{engine: e, toolMgr: manager}
	manager.CodeModePromptBinder = rt.codeModeCellPrompts
	manager.SetupEngine(e)
	t.Cleanup(func() { e.CloseCodeMode(); rt.clearPendingApprovals(); manager.ApprovalMgr.Close() })
	return rt
}
func waitScriptPrompt(t *testing.T, rt *serveRuntime) serveApprovalPrompt {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		p := rt.pendingApprovalPrompts()
		if len(p) > 0 {
			return p[0]
		}
		select {
		case <-deadline:
			t.Fatal("idle approval not published")
		default:
			runtime.Gosched()
		}
	}
}
func TestCodeModeIdleApprovalsParkPermitsAndReconnect(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	var runtimes []*serveRuntime
	var ids []string
	// More unanswered prompts than the process executing-tool cap must not freeze
	// unrelated read-only work or a later turn's own approval prompts.
	for i := range 17 {
		id := fmt.Sprintf("approval-%d", i)
		rt := cellTestRuntime(t, id, dir, false)
		runtimes = append(runtimes, rt)
		ids = append(ids, id)
		ctx := core.ContextWithSessionID(context.Background(), id)
		r, err := rt.engine.ExecCodeMode(ctx, `text('ready');await tools.read_file({path:'file'});text('approved')`, time.Second, 0)
		if err != nil || r.IsError {
			t.Fatalf("spawn %+v %v", r, err)
		}
		p := waitScriptPrompt(t, rt)
		if p.CellID != r.CellID {
			t.Fatalf("approval lost cell owner %+v", p)
		}
	}
	free := cellTestRuntime(t, "free", dir, true)
	r, err := free.engine.ExecCodeMode(core.ContextWithSessionID(context.Background(), "free"), `text(await tools.read_file({path:'file'}))`, 2*time.Second, 0)
	if err != nil || r.Status != "completed" {
		t.Fatalf("unanswered approvals exhausted process permits: %+v %v", r, err)
	}
	rt := runtimes[0]
	p := waitScriptPrompt(t, rt)
	var rebound map[string]any
	rt.rebindCellApprovals(func(event string, data map[string]any) error { rebound = data; return nil })
	if rebound["approval_id"] != p.ApprovalID || rebound["cell_id"] != p.CellID {
		t.Fatalf("rebind changed identity: %+v", rebound)
	}
	nextCtx, cancel := context.WithCancel(context.Background())
	cancel()
	rt.approvalMu.Lock()
	rt.approvalCtx = nextCtx
	rt.approvalMu.Unlock()
	// Cancelling a new turn/transport cannot cancel this earlier cell's prompt.
	if len(rt.pendingApprovalPrompts()) != 1 {
		t.Fatal("new transport cancelled old prompt")
	}
	once := -1
	for _, opt := range p.Options {
		if opt.Choice == "once" {
			once = opt.Index
		}
	}
	if once < 0 {
		t.Fatal("missing once option")
	}
	if err := rt.submitApproval(p.ApprovalID, once, false, false); err != nil {
		t.Fatal(err)
	}
	result, err := rt.engine.WaitCodeMode(core.ContextWithSessionID(context.Background(), ids[0]), p.CellID, 2*time.Second, 0)
	if err != nil || result.Status != "completed" {
		t.Fatalf("approved idle cell: %+v %v", result, err)
	}
	for i := 1; i < len(runtimes); i++ {
		p := waitScriptPrompt(t, runtimes[i])
		runtimes[i].engine.InterruptCodeModeSession(ids[i])
		result, err := runtimes[i].engine.WaitCodeMode(core.ContextWithSessionID(context.Background(), ids[i]), p.CellID, time.Second, 0)
		if err != nil || result.Status != "cancelled" {
			t.Fatalf("Stop left idle prompt: %+v %v", result, err)
		}
	}
}
func TestCodeModeIdleStopEndpointCancelsAllAndProtectsEviction(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "file"), []byte("content"), 0600)
	rt := cellTestRuntime(t, "idle", dir, false)
	mgr := newServeSessionManager(time.Hour, 5, nil)
	defer mgr.Close()
	putTestSession(mgr, "idle", rt)
	server := &serveServer{sessionMgr: mgr}
	var cells []string
	for range 2 {
		r, err := rt.engine.ExecCodeMode(core.ContextWithSessionID(context.Background(), "idle"), `text('ready');await tools.read_file({path:'file'});store('bad',true)`, time.Second, 0)
		if err != nil || r.IsError {
			t.Fatalf("spawn %+v %v", r, err)
		}
		cells = append(cells, r.CellID)
	}
	waitScriptPrompt(t, rt)
	if !rt.hasActiveActivity() {
		t.Fatal("idle runtime with live cell became evictable")
	}
	for _, body := range []string{fmt.Sprintf(`{"cell_id":%q}`, cells[0]), `{}`} {
		request := httptest.NewRequest(http.MethodPost, "/v1/sessions/idle/code-cells/cancel", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.routeSessionCore(response, request, "idle", "idle", "code-cells/cancel")
		if response.Code != 200 {
			t.Fatalf("idle cancel %d %s", response.Code, response.Body.String())
		}
	}
	for _, id := range cells {
		result, err := rt.engine.WaitCodeMode(core.ContextWithSessionID(context.Background(), "idle"), id, time.Second, 0)
		if err != nil || result.Status != "cancelled" || result.StateCommitted {
			t.Fatalf("Stop %+v %v", result, err)
		}
	}
	response := httptest.NewRecorder()
	server.handleCodeCells(response, httptest.NewRequest("GET", "/v1/sessions/idle/code-cells", nil), "idle", "code-cells")
	var state struct {
		Cells []core.CodeModeCellSummary `json:"cells"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil || len(state.Cells) != 2 {
		t.Fatalf("terminal outcomes missing: %s %v", response.Body.String(), err)
	}
}

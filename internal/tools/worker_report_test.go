package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

func workerReportToolStore(t *testing.T) (*session.SQLiteStore, session.WorkerEdge) {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now()
	coordinator := &session.Session{ID: "main", Provider: "mock", Model: "mock", Mode: session.ModeChat, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}
	if err := store.Create(context.Background(), coordinator); err != nil {
		t.Fatal(err)
	}
	edge, err := store.CreateWorker(context.Background(), coordinator.ID, "test reporting")
	if err != nil {
		t.Fatal(err)
	}
	return store, edge
}

func TestWorkerReportToolPersistsStructuredMailboxEvent(t *testing.T) {
	store, edge := workerReportToolStore(t)
	tool := NewWorkerReportTool(store, edge.ChildSessionID, edge.CoordinatorSessionID)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{
		"kind":"progress","title":"Tests running","body":"Focused tests are green.","metadata":{"count":12}
	}`))
	if err != nil || !strings.Contains(out.Content, "saved") {
		t.Fatalf("Execute = %#v, %v", out, err)
	}
	reports, err := store.ListWorkerReports(context.Background(), edge.ChildSessionID)
	if err != nil || len(reports) != 1 {
		t.Fatalf("reports = %#v, %v", reports, err)
	}
	if reports[0].Kind != session.WorkerReportProgress || reports[0].Title != "Tests running" || reports[0].Body != "Focused tests are green." || reports[0].Origin != "worker_tool" {
		t.Fatalf("report = %#v", reports[0])
	}
	loaded, _ := store.GetWorker(context.Background(), edge.ChildSessionID)
	if loaded.Status != session.WorkerRunning {
		t.Fatalf("worker status = %s", loaded.Status)
	}
}

func TestWorkerReportToolBlockerAndValidation(t *testing.T) {
	store, edge := workerReportToolStore(t)
	tool := NewWorkerReportTool(store, edge.ChildSessionID, edge.CoordinatorSessionID)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"kind":"blocker","title":"Need input","body":"Missing fixture."}`))
	if err != nil || !strings.Contains(out.Content, "blocker") {
		t.Fatalf("blocker = %#v, %v", out, err)
	}
	loaded, _ := store.GetWorker(context.Background(), edge.ChildSessionID)
	if loaded.Status != session.WorkerBlocked {
		t.Fatalf("worker status = %s", loaded.Status)
	}
	out, err = tool.Execute(context.Background(), json.RawMessage(`{"kind":"developer","title":"bad","body":"bad"}`))
	if err != nil || !strings.Contains(out.Content, "not saved") {
		t.Fatalf("invalid kind = %#v, %v", out, err)
	}
}

func TestDefaultRegistryDoesNotExposeWorkerReport(t *testing.T) {
	cfg := DefaultToolConfig()
	registry, err := NewLocalToolRegistry(&cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Get(WorkerReportToolName); ok {
		t.Fatal("ordinary registry exposed worker report tool")
	}
}

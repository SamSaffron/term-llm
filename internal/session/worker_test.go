package session

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func newWorkerStoreTest(t *testing.T) (*SQLiteStore, *Session) {
	t.Helper()
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	coordinator := createWorkspaceTestSession(t, store, "coordinator")
	coordinator.ApprovalMode = ApprovalModeAuto
	coordinator.CWD = t.TempDir()
	if err := store.Update(context.Background(), coordinator); err != nil {
		t.Fatal(err)
	}
	return store, coordinator
}

func TestWorkerMigration47CreatesSeparateEdgesAndMailbox(t *testing.T) {
	if schemaVersion != 47 {
		t.Fatalf("schemaVersion = %d, want 47", schemaVersion)
	}
	found := false
	for _, migration := range migrations {
		found = found || migration.version == 47
	}
	if !found {
		t.Fatal("migration 47 missing")
	}

	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"session_worker_reports", "session_workers"} {
		if _, err := store.db.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec("UPDATE schema_version SET version = 46"); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	upgraded, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	for _, table := range []string{"session_workers", "session_worker_reports"} {
		var name string
		if err := upgraded.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name); err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
}

func TestWorkerIdentityDoesNotUseBranchOrIsSubagentPersistence(t *testing.T) {
	store, coordinator := newWorkerStoreTest(t)
	edge, err := store.CreateWorker(context.Background(), coordinator.ID, "inspect the parser")
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Get(context.Background(), edge.ChildSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentID != "" || child.IsSubagent {
		t.Fatalf("worker leaked into transient/branch identity: %#v", child)
	}
	var branches int
	if err := store.db.QueryRow("SELECT COUNT(1) FROM session_branches WHERE child_session_id = ?", child.ID).Scan(&branches); err != nil {
		t.Fatal(err)
	}
	if branches != 0 {
		t.Fatalf("worker created %d branch edges", branches)
	}
	loaded, err := store.GetWorker(context.Background(), child.ID)
	if err != nil || loaded.CoordinatorSessionID != coordinator.ID || loaded.Task != "inspect the parser" {
		t.Fatalf("durable worker edge = %#v, %v", loaded, err)
	}
}

func TestWorkerPreventsConflictingActiveSharedCWD(t *testing.T) {
	store, coordinator := newWorkerStoreTest(t)
	if _, err := store.CreateWorker(context.Background(), coordinator.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorker(context.Background(), coordinator.ID, "second"); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("second worker error = %v", err)
	}

	sameWorkspace := *coordinator
	sameWorkspace.ID = "same-workspace"
	sameWorkspace.Name = "same workspace"
	sameWorkspace.CreatedAt = time.Now()
	sameWorkspace.UpdatedAt = sameWorkspace.CreatedAt
	if err := store.Create(context.Background(), &sameWorkspace); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorker(context.Background(), sameWorkspace.ID, "conflicting coordinator"); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("shared-workspace worker error = %v", err)
	}

	isolated := sameWorkspace
	isolated.ID = "isolated-workspace"
	isolated.Name = "isolated workspace"
	isolated.CWD = t.TempDir()
	isolated.CreatedAt = time.Now()
	isolated.UpdatedAt = isolated.CreatedAt
	if err := store.Create(context.Background(), &isolated); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorker(context.Background(), isolated.ID, "isolated coordinator"); err != nil {
		t.Fatalf("isolated workspace worker blocked: %v", err)
	}
}

func TestWorkerReportsAreBoundedImmutableAndImportedAsUser(t *testing.T) {
	store, coordinator := newWorkerStoreTest(t)
	edge, err := store.CreateWorker(context.Background(), coordinator.ID, "summarize tests")
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.AddWorkerReport(context.Background(), WorkerReport{
		ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
		DestinationSessionID: coordinator.ID, Kind: WorkerReportResult,
		Title: "Test summary", Body: "All focused tests passed.", Metadata: json.RawMessage(`{"suite":"focused"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Sequence != 0 || report.ID == 0 {
		t.Fatalf("report identity = %#v", report)
	}
	if unread, err := store.CountUnreadWorkerReports(context.Background(), coordinator.ID); err != nil || unread != 1 {
		t.Fatalf("unread = %d, %v", unread, err)
	}
	if _, err := store.db.Exec("UPDATE session_worker_reports SET body = 'laundered' WHERE id = ?", report.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("direct content mutation error = %v", err)
	}

	imported, err := store.ImportWorkerReport(context.Background(), report.ID)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Role != llm.RoleUser || !strings.Contains(imported.TextContent, "[Imported worker report]") || !strings.Contains(imported.TextContent, report.Body) {
		t.Fatalf("imported message = %#v", imported)
	}
	for _, part := range imported.Parts {
		if part.Type == llm.PartPathNote {
			t.Fatalf("worker import used trusted developer path-note provenance: %#v", imported.Parts)
		}
	}
	messages, err := store.GetMessages(context.Background(), coordinator.ID, 0, 0)
	if err != nil || len(messages) != 1 || messages[0].Role != llm.RoleUser {
		t.Fatalf("coordinator transcript = %#v, %v", messages, err)
	}
	second, err := store.ImportWorkerReport(context.Background(), report.ID)
	if err != nil || second.ID != imported.ID {
		t.Fatalf("idempotent import = %#v, %v", second, err)
	}
	messages, _ = store.GetMessages(context.Background(), coordinator.ID, 0, 0)
	if len(messages) != 1 {
		t.Fatalf("idempotent import duplicated transcript: %#v", messages)
	}
	reports, err := store.ListWorkerReports(context.Background(), edge.ChildSessionID)
	if err != nil || reports[0].ImportedMessageID != imported.ID || reports[0].ReadAt == nil {
		t.Fatalf("report import bookkeeping = %#v, %v", reports, err)
	}
	if _, err := store.db.Exec("DELETE FROM messages WHERE id = ?", imported.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportWorkerReport(context.Background(), report.ID); err == nil || !strings.Contains(err.Error(), "already been imported") {
		t.Fatalf("reimport after attributed message deletion error = %v", err)
	}
	messages, _ = store.GetMessages(context.Background(), coordinator.ID, 0, 0)
	if len(messages) != 0 {
		t.Fatalf("reimport after deletion duplicated transcript: %#v", messages)
	}
}

func TestWorkerReportValidationAndTerminalStatus(t *testing.T) {
	store, coordinator := newWorkerStoreTest(t)
	edge, err := store.CreateWorker(context.Background(), coordinator.ID, "bounded")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AddWorkerReport(context.Background(), WorkerReport{
		ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
		DestinationSessionID: coordinator.ID, Kind: WorkerReportResult,
		Title: "too much metadata", Body: "body", Metadata: json.RawMessage(`"` + strings.Repeat("x", MaxWorkerMetadataBytes) + `"`),
	})
	if err == nil {
		t.Fatal("oversized metadata accepted")
	}
	if err := store.UpdateWorkerStatus(context.Background(), edge.ChildSessionID, WorkerDone); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetWorker(context.Background(), edge.ChildSessionID)
	if err != nil || loaded.Status != WorkerDone || loaded.FinishedAt == nil {
		t.Fatalf("terminal worker = %#v, %v", loaded, err)
	}
	if _, err := store.CreateWorker(context.Background(), coordinator.ID, "next after terminal"); err != nil {
		t.Fatalf("terminal worker still blocked next launch: %v", err)
	}
}

func TestWorkerCascadeDeletesReports(t *testing.T) {
	store, coordinator := newWorkerStoreTest(t)
	edge, _ := store.CreateWorker(context.Background(), coordinator.ID, "cascade")
	report, _ := store.AddWorkerReport(context.Background(), WorkerReport{
		ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
		DestinationSessionID: coordinator.ID, Kind: WorkerReportProgress, Title: "Started", Body: "Working.",
	})
	if err := store.Delete(context.Background(), edge.ChildSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetWorker(context.Background(), edge.ChildSessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("worker after cascade = %v", err)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(1) FROM session_worker_reports WHERE id = ?", report.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("report cascade count = %d, %v", count, err)
	}
}

func TestWorkerReportReadMetadataMayMutateWithoutContentChange(t *testing.T) {
	store, coordinator := newWorkerStoreTest(t)
	edge, _ := store.CreateWorker(context.Background(), coordinator.ID, "read")
	report, _ := store.AddWorkerReport(context.Background(), WorkerReport{
		ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
		DestinationSessionID: coordinator.ID, Kind: WorkerReportProgress, Title: "Halfway", Body: "50 percent", Origin: "worker_tool",
	})
	if err := store.MarkWorkerReportRead(context.Background(), report.ID); err != nil {
		t.Fatal(err)
	}
	reports, _ := store.ListWorkerReports(context.Background(), edge.ChildSessionID)
	if reports[0].ReadAt == nil || reports[0].Title != report.Title || reports[0].Body != report.Body || reports[0].CreatedAt.Before(time.Time{}) {
		t.Fatalf("read mutation changed content: %#v", reports[0])
	}
}

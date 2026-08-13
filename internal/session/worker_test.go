package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestWorkerMigrationsCreateSeparateEdgesMailboxAndOwnership(t *testing.T) {
	if schemaVersion != 48 {
		t.Fatalf("schemaVersion = %d, want 48", schemaVersion)
	}
	found47 := false
	found48 := false
	for _, migration := range migrations {
		found47 = found47 || migration.version == 47
		found48 = found48 || migration.version == 48
	}
	if !found47 || !found48 {
		t.Fatalf("worker migrations missing: v47=%v v48=%v", found47, found48)
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
	var ownerColumn string
	if err := upgraded.db.QueryRow(`SELECT name FROM pragma_table_info('session_workers') WHERE name = 'owner_id'`).Scan(&ownerColumn); err != nil {
		t.Fatalf("worker owner column missing: %v", err)
	}
}

func TestWorkerMigration48AddsOwnerToVersion47Table(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version47.db")
	store, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE session_worker_reports`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE session_workers`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TABLE session_workers (
		child_session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
		coordinator_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		task TEXT NOT NULL, status TEXT NOT NULL, job_id TEXT, run_id TEXT,
		created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL, finished_at TIMESTAMP
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE schema_version SET version = 47`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var ownerColumn string
	if err := upgraded.db.QueryRow(`SELECT name FROM pragma_table_info('session_workers') WHERE name = 'owner_id'`).Scan(&ownerColumn); err != nil {
		t.Fatalf("migration 48 owner column missing: %v", err)
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

	linkedRoot := t.TempDir()
	linkedCWD := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(linkedRoot, linkedCWD); err == nil {
		linked := sameWorkspace
		linked.ID = "linked-workspace"
		linked.Name = "linked workspace"
		linked.CWD = linkedCWD
		linked.CreatedAt = time.Now()
		linked.UpdatedAt = linked.CreatedAt
		if err := store.Create(context.Background(), &linked); err != nil {
			t.Fatal(err)
		}
		real := sameWorkspace
		real.ID = "real-linked-workspace"
		real.Name = "real linked workspace"
		real.CWD = linkedRoot
		real.CreatedAt = time.Now()
		real.UpdatedAt = real.CreatedAt
		if err := store.Create(context.Background(), &real); err != nil {
			t.Fatal(err)
		}
		linkedEdge, err := store.CreateWorker(context.Background(), linked.ID, "linked owner")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateWorkerStatus(context.Background(), linkedEdge.ChildSessionID, WorkerRunning); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateWorker(context.Background(), real.ID, "canonical conflict"); err == nil || !strings.Contains(err.Error(), "workspace") {
			t.Fatalf("symlink-equivalent workspace error = %v", err)
		}
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

func TestInteractiveWorkerResultAppendsAfterCanonicalTerminalResult(t *testing.T) {
	store, coordinator := newWorkerStoreTest(t)
	edge, err := store.CreateWorker(context.Background(), coordinator.ID, "continue after completion")
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := store.AddWorkerReport(context.Background(), WorkerReport{
		ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
		DestinationSessionID: edge.CoordinatorSessionID, Kind: WorkerReportResult,
		Title: "Terminal", Body: "background complete", Origin: "terminal_synthesis",
	})
	if err != nil {
		t.Fatal(err)
	}
	interactive, err := store.AddWorkerReport(context.Background(), WorkerReport{
		ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
		DestinationSessionID: edge.CoordinatorSessionID, Kind: WorkerReportResult,
		Title: "Interactive", Body: "continued result", Origin: WorkerReportOriginInteractive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if interactive.ID == terminal.ID || interactive.Sequence != terminal.Sequence+1 {
		t.Fatalf("interactive result collapsed into terminal result: terminal=%#v interactive=%#v", terminal, interactive)
	}
	reports, err := store.ListWorkerReports(context.Background(), edge.ChildSessionID)
	if err != nil || len(reports) != 2 || reports[1].Origin != WorkerReportOriginInteractive {
		t.Fatalf("reports = %#v, %v", reports, err)
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
	duplicate, err := store.AddWorkerReport(context.Background(), WorkerReport{
		ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
		DestinationSessionID: coordinator.ID, Kind: WorkerReportResult,
		Title: "Duplicate terminal", Body: "must not be appended",
	})
	if err != nil || duplicate.ID != report.ID {
		t.Fatalf("duplicate terminal result = %#v, %v", duplicate, err)
	}
	terminalReports, err := store.ListWorkerReports(context.Background(), edge.ChildSessionID)
	if err != nil || len(terminalReports) != 1 {
		t.Fatalf("terminal result cardinality = %#v, %v", terminalReports, err)
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

func TestWorkerReportImportJoinsCompactedCoordinatorActiveContext(t *testing.T) {
	ctx := context.Background()
	store, coordinator := newWorkerStoreTest(t)
	if err := store.AddMessage(ctx, coordinator.ID, NewMessage(coordinator.ID, llm.UserText("old question"), -1)); err != nil {
		t.Fatal(err)
	}
	summary := NewMessage(coordinator.ID, llm.UserText("[Context Compaction]\nsummary"), -1)
	ack := NewMessage(coordinator.ID, llm.AssistantText("I have the compacted context."), -1)
	ack.CompactionTail = true
	if err := store.CompactMessages(ctx, coordinator.ID, []Message{*summary, *ack}); err != nil {
		t.Fatal(err)
	}
	compacted, err := store.Get(ctx, coordinator.ID)
	if err != nil {
		t.Fatal(err)
	}
	boundary := compacted.CompactionSeq

	edge, err := store.CreateWorker(ctx, coordinator.ID, "compacted report")
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.AddWorkerReport(ctx, WorkerReport{
		ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
		DestinationSessionID: coordinator.ID, Kind: WorkerReportResult,
		Title: "After compaction", Body: "new worker evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	imported, err := store.ImportWorkerReport(ctx, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := store.Get(ctx, coordinator.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.CompactionSeq != boundary || imported.Sequence < boundary {
		t.Fatalf("import changed compaction boundary: boundary=%d session=%d import=%d", boundary, refreshed.CompactionSeq, imported.Sequence)
	}
	active, err := LoadActiveMessages(ctx, store, refreshed)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) == 0 || active[len(active)-1].ID != imported.ID || !strings.Contains(active[len(active)-1].TextContent, report.Body) {
		t.Fatalf("compacted active context missing import: %#v", active)
	}
}

func TestWorkerConcurrentTerminalResultsCollapseAndTerminalStateDoesNotReopen(t *testing.T) {
	store, coordinator := newWorkerStoreTest(t)
	edge, err := store.CreateWorker(context.Background(), coordinator.ID, "concurrent terminal")
	if err != nil {
		t.Fatal(err)
	}
	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.AddWorkerReport(context.Background(), WorkerReport{
				ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
				DestinationSessionID: coordinator.ID, Kind: WorkerReportResult,
				Title: fmt.Sprintf("Result %d", i), Body: "terminal",
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	reports, err := store.ListWorkerReports(context.Background(), edge.ChildSessionID)
	if err != nil || len(reports) != 1 {
		t.Fatalf("terminal reports = %#v, %v", reports, err)
	}
	if err := store.UpdateWorkerStatus(context.Background(), edge.ChildSessionID, WorkerCancelled); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateWorkerStatus(context.Background(), edge.ChildSessionID, WorkerRunning); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateWorkerStatus(context.Background(), edge.ChildSessionID, WorkerFailed); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetWorker(context.Background(), edge.ChildSessionID)
	if err != nil || loaded.Status != WorkerCancelled {
		t.Fatalf("terminal worker reopened/changed = %#v, %v", loaded, err)
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

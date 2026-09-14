package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStaleCheckOrphanedJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.db-wal")
	if err := os.WriteFile(path, []byte("orphaned"), 0o600); err != nil {
		t.Fatalf("write orphaned journal: %v", err)
	}

	check := StaleCheck{DataDir: dir}
	findings := check.Run(context.Background())
	finding := staleFindingContaining(t, findings, "orphaned journal file foo.db-wal")
	if finding.Severity != SeverityWarn {
		t.Fatalf("severity = %s, want %s", finding.Severity, SeverityWarn)
	}
	if finding.Fix == nil {
		t.Fatal("orphaned journal finding has no fix")
	}
	if err := finding.Fix(context.Background()); err != nil {
		t.Fatalf("remove orphaned journal: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("journal still exists after fix; stat error = %v", err)
	}
}

func TestStaleCheckLargeWAL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "bar.db")
	staleTempDB(t, ctx, dbPath, `CREATE TABLE t(x)`)
	walPath := dbPath + "-wal"
	if err := os.WriteFile(walPath, make([]byte, 2048), 0o600); err != nil {
		t.Fatalf("write large WAL: %v", err)
	}

	check := StaleCheck{DataDir: dir, WALWarnBytes: 1024}
	finding := staleFindingContaining(t, check.Run(ctx), "bar.db write-ahead log")
	if finding.Severity != SeverityInfo {
		t.Fatalf("severity = %s, want %s", finding.Severity, SeverityInfo)
	}
	if finding.Fix == nil {
		t.Fatal("large WAL finding has no fix")
	}
}

func TestStaleCheckLeftoverInstallLock(t *testing.T) {
	configDir := t.TempDir()
	path := filepath.Join(configDir, "services", "web", "install.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create service directory: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write install lock: %v", err)
	}

	check := StaleCheck{ConfigDir: configDir}
	finding := staleFindingContaining(t, check.Run(context.Background()), "leftover install lock")
	if finding.Severity != SeverityInfo {
		t.Fatalf("severity = %s, want %s", finding.Severity, SeverityInfo)
	}
	if finding.Fix == nil {
		t.Fatal("leftover lock finding has no fix")
	}
	if err := finding.Fix(context.Background()); err != nil {
		t.Fatalf("remove install lock: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("install lock still exists after fix; stat error = %v", err)
	}
}

func TestStaleCheckOrphanRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "orphans.db")
	staleTempDB(t, ctx, path,
		`CREATE TABLE parent(id TEXT PRIMARY KEY)`,
		`CREATE TABLE child(parent_id TEXT)`,
		`INSERT INTO child(parent_id) VALUES ('missing')`,
	)
	check := StaleCheck{Orphans: []OrphanTarget{{
		Name: "test database",
		Path: path,
		Rules: []OrphanRule{{
			Label:     "children without a parent",
			CountSQL:  `SELECT COUNT(*) FROM child LEFT JOIN parent ON parent.id = child.parent_id WHERE parent.id IS NULL`,
			DeleteSQL: `DELETE FROM child WHERE NOT EXISTS (SELECT 1 FROM parent WHERE parent.id = child.parent_id)`,
		}},
	}}}

	findings := check.Run(ctx)
	finding := staleFindingContaining(t, findings, "1 orphaned row (children without a parent)")
	if finding.Severity != SeverityWarn {
		t.Fatalf("severity = %s, want %s", finding.Severity, SeverityWarn)
	}
	if finding.Fix == nil {
		t.Fatal("orphan-row finding has no fix")
	}
	if err := finding.Fix(ctx); err != nil {
		t.Fatalf("delete orphan rows: %v", err)
	}

	for _, finding := range check.Run(ctx) {
		if strings.Contains(finding.Title, "children without a parent") {
			t.Fatalf("orphan finding remains after fix: %+v", finding)
		}
	}
}

func staleTempDB(t *testing.T, ctx context.Context, path string, statements ...string) {
	t.Helper()
	db, err := openReadWrite(ctx, path)
	if err != nil {
		t.Fatalf("open temporary database: %v", err)
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			t.Fatalf("execute %q: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close temporary database: %v", err)
	}
}

func staleFindingContaining(t *testing.T, findings []Finding, text string) Finding {
	t.Helper()
	for _, finding := range findings {
		if strings.Contains(finding.Title, text) {
			return finding
		}
	}
	t.Fatalf("no finding containing %q in %+v", text, findings)
	return Finding{}
}

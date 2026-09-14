package doctor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// databaseFixture creates a database at path and runs the supplied statements.
func databaseFixture(t *testing.T, path string, statements ...string) {
	t.Helper()
	ctx := context.Background()
	db, err := openReadWrite(ctx, path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
}

func databaseReference(statements ...string) func(context.Context, string) error {
	return func(ctx context.Context, path string) error {
		db, err := openReadWrite(ctx, path)
		if err != nil {
			return err
		}
		defer db.Close()
		for _, statement := range statements {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	}
}

func runDatabaseCheck(t *testing.T, target DatabaseTarget) []Finding {
	t.Helper()
	check := &DatabaseCheck{Targets: []DatabaseTarget{target}}
	return check.Run(context.Background())
}

func findingWithTitle(findings []Finding, substring string) (Finding, bool) {
	for _, finding := range findings {
		if strings.Contains(finding.Title, substring) {
			return finding, true
		}
	}
	return Finding{}, false
}

func TestDatabaseCheckReportsMissingDatabaseAsInfo(t *testing.T) {
	findings := runDatabaseCheck(t, DatabaseTarget{
		Name: "sessions.db",
		Path: filepath.Join(t.TempDir(), "sessions.db"),
	})

	if len(findings) != 1 || findings[0].Severity != SeverityInfo {
		t.Fatalf("expected one info finding for a database that does not exist, got %+v", findings)
	}
}

func TestDatabaseCheckAcceptsSchemaMatchingMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	schema := []string{
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL)`,
		`CREATE INDEX idx_notes_body ON notes(body)`,
	}
	databaseFixture(t, path, schema...)

	findings := runDatabaseCheck(t, DatabaseTarget{
		Name:           "sessions.db",
		Path:           path,
		BuildReference: databaseReference(schema...),
	})

	if len(findings) != 0 {
		t.Fatalf("expected no findings for a schema that matches its migrations, got %+v", findings)
	}
}

// A migration added as an edit to the baseline schema reaches new installs but
// never runs against existing databases. That is the drift this check exists
// to catch, so it must be reported as an error rather than a note.
func TestDatabaseCheckDetectsColumnMissingFromExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	databaseFixture(t, path, `CREATE TABLE notes (id INTEGER PRIMARY KEY)`)

	findings := runDatabaseCheck(t, DatabaseTarget{
		Name:           "sessions.db",
		Path:           path,
		BuildReference: databaseReference(`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL)`),
	})

	finding, ok := findingWithTitle(findings, "missing")
	if !ok {
		t.Fatalf("expected a missing-object finding, got %+v", findings)
	}
	if finding.Severity != SeverityError {
		t.Fatalf("expected schema drift to be an error, got %s", finding.Severity)
	}
	if !strings.Contains(finding.Detail, "column notes.body") {
		t.Fatalf("expected the missing column to be named, got %q", finding.Detail)
	}
}

func TestDatabaseCheckReportsLeftoverObjectsAsWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	databaseFixture(t, path,
		`CREATE TABLE notes (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE notes_old (id INTEGER PRIMARY KEY)`,
	)

	findings := runDatabaseCheck(t, DatabaseTarget{
		Name:           "sessions.db",
		Path:           path,
		BuildReference: databaseReference(`CREATE TABLE notes (id INTEGER PRIMARY KEY)`),
	})

	finding, ok := findingWithTitle(findings, "would not have")
	if !ok {
		t.Fatalf("expected an extra-object finding, got %+v", findings)
	}
	if finding.Severity != SeverityWarn {
		t.Fatalf("expected leftover objects to warn, got %s", finding.Severity)
	}
	if !strings.Contains(finding.Detail, "notes_old") {
		t.Fatalf("expected the leftover table to be named, got %q", finding.Detail)
	}
}

// ANALYZE creates sqlite_stat1, which only ever exists on the user's database.
// It must not be mistaken for drift.
func TestDatabaseCheckIgnoresSQLiteOwnedObjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	schema := []string{`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`}
	databaseFixture(t, path, append(schema, `INSERT INTO notes(body) VALUES ('x')`, `ANALYZE`)...)

	findings := runDatabaseCheck(t, DatabaseTarget{
		Name:           "sessions.db",
		Path:           path,
		BuildReference: databaseReference(schema...),
	})

	if len(findings) != 0 {
		t.Fatalf("expected ANALYZE artifacts to be ignored, got %+v", findings)
	}
}

func TestDatabaseCheckDetectsAndRebuildsDriftedFTSIndex(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "memory.db")
	// Rows are inserted without the usual sync trigger, which is exactly what an
	// interrupted rebuild or a direct sqlite3 edit leaves behind.
	databaseFixture(t, path,
		`CREATE TABLE fragments (id INTEGER PRIMARY KEY, content TEXT)`,
		`CREATE VIRTUAL TABLE fragments_fts USING fts5(content, content='fragments', content_rowid='id')`,
		`INSERT INTO fragments(content) VALUES ('alpha'), ('beta')`,
	)

	target := DatabaseTarget{
		Name: "memory.db",
		Path: path,
		FTS:  []FTSIndex{{Name: "fragments_fts", Source: "fragments"}},
	}

	findings := runDatabaseCheck(t, target)
	finding, ok := findingWithTitle(findings, "out of sync")
	if !ok {
		t.Fatalf("expected an FTS drift finding, got %+v", findings)
	}
	if finding.Severity != SeverityWarn || finding.Fix == nil {
		t.Fatalf("expected a fixable warning, got %+v", finding)
	}

	if err := finding.Fix(ctx); err != nil {
		t.Fatalf("rebuild index: %v", err)
	}

	if remaining := runDatabaseCheck(t, target); len(remaining) != 0 {
		t.Fatalf("expected the rebuild to resolve the drift, got %+v", remaining)
	}
}

func TestOpenReadOnlyRejectsWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	databaseFixture(t, path, `CREATE TABLE notes (id INTEGER PRIMARY KEY)`)

	db, err := openReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `INSERT INTO notes(id) VALUES (1)`); err == nil {
		t.Fatal("expected a read-only connection to reject writes")
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes`).Scan(&count); err != nil {
		t.Fatalf("read-only query: %v", err)
	}
}

func TestDiffSignaturesDescribesBothDirections(t *testing.T) {
	reference := []string{
		"master|table|notes|notes|CREATE TABLE notes(id INTEGER)",
		"column|notes|body|TEXT|0|false:|0",
	}
	actual := []string{
		"master|table|notes|notes|CREATE TABLE notes(id INTEGER)",
		"master|table|notes_old|notes_old|CREATE TABLE notes_old(id INTEGER)",
	}

	missing, extra := diffSignatures(reference, actual)

	if len(missing) != 1 || missing[0] != "column notes.body (TEXT)" {
		t.Fatalf("unexpected missing entries %v", missing)
	}
	if len(extra) != 1 || extra[0] != "table notes_old" {
		t.Fatalf("unexpected extra entries %v", extra)
	}
}

// Listing every column of a table that is missing entirely buries the one line
// that matters, so those entries are folded into the table entry.
func TestDiffSignaturesFoldsMembersOfAbsentTables(t *testing.T) {
	reference := []string{
		"master|table|notes|notes|CREATE TABLE notes(id INTEGER)",
		"master|index|idx_notes|notes|CREATE INDEX idx_notes ON notes(id)",
		"column|notes|id|INTEGER|0|false:|1",
		"column|notes|body|TEXT|0|false:|0",
		"foreign-key|notes|0|0|authors|author_id|id|NO ACTION|CASCADE|NONE",
	}

	missing, extra := diffSignatures(reference, nil)

	if len(missing) != 1 || missing[0] != "table notes" {
		t.Fatalf("expected a single table entry, got %v", missing)
	}
	if len(extra) != 0 {
		t.Fatalf("expected no extra entries, got %v", extra)
	}
}

func TestIsInternalSignatureEntry(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		want  bool
	}{
		{name: "user table", entry: "master|table|notes|notes|CREATE TABLE notes(id)", want: false},
		{name: "sqlite table", entry: "master|table|sqlite_stat1|sqlite_stat1|CREATE TABLE sqlite_stat1(tbl)", want: true},
		{name: "sqlite column", entry: "column|sqlite_stat1|tbl|TEXT|0|false:|0", want: true},
		{name: "user column", entry: "column|notes|body|TEXT|0|false:|0", want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isInternalSignatureEntry(testCase.entry); got != testCase.want {
				t.Fatalf("got %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestCountRowsRejectsUnsafeTableNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	databaseFixture(t, path, `CREATE TABLE notes (id INTEGER PRIMARY KEY)`)

	db, err := openReadOnly(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := countRows(context.Background(), db, `notes"; DROP TABLE notes; --`); err == nil {
		t.Fatal("expected an invalid identifier to be rejected")
	}
}

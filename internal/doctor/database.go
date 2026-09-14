package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/sqliteutil"

	_ "modernc.org/sqlite"
)

// FTSIndex pairs a full-text index with the table it indexes. Both tables use
// SQLite external-content FTS5, so the index can silently drift from its source
// when a write path bypasses the sync triggers or a rebuild is interrupted.
type FTSIndex struct {
	Name   string
	Source string
}

// DatabaseTarget describes one SQLite database that doctor can inspect.
type DatabaseTarget struct {
	// Name is a short label such as "sessions".
	Name string
	// Path is the database file location.
	Path string
	// FTS lists full-text indexes to compare against their source tables.
	FTS []FTSIndex
	// BuildReference creates a pristine database at the supplied path by
	// running the owning store's full migration chain. A nil builder disables
	// golden-schema comparison for this target.
	BuildReference func(ctx context.Context, path string) error
}

// DatabaseCheck compares each database against the schema its migrations
// produce from scratch, and verifies SQLite-level integrity.
type DatabaseCheck struct {
	Targets []DatabaseTarget
}

// ID implements Check.
func (c *DatabaseCheck) ID() string { return "db" }

// Title implements Check.
func (c *DatabaseCheck) Title() string { return "Databases" }

// Run implements Check.
func (c *DatabaseCheck) Run(ctx context.Context) []Finding {
	var findings []Finding
	for _, target := range c.Targets {
		findings = append(findings, c.runTarget(ctx, target)...)
	}
	return findings
}

func (c *DatabaseCheck) runTarget(ctx context.Context, target DatabaseTarget) []Finding {
	if _, err := os.Stat(target.Path); err != nil {
		if os.IsNotExist(err) {
			return []Finding{{
				Title:    fmt.Sprintf("%s: not created yet", target.Name),
				Severity: SeverityInfo,
				Detail:   target.Path,
			}}
		}
		return []Finding{{
			Title:    fmt.Sprintf("%s: cannot stat database", target.Name),
			Severity: SeverityError,
			Detail:   err.Error(),
		}}
	}

	db, err := openReadOnly(ctx, target.Path)
	if err != nil {
		return []Finding{{
			Title:    fmt.Sprintf("%s: cannot open database", target.Name),
			Severity: SeverityError,
			Detail:   err.Error(),
			Remedy:   "another process may hold an exclusive lock, or the file is corrupt",
		}}
	}
	defer db.Close()

	findings := integrityFindings(ctx, target, db)
	findings = append(findings, schemaFindings(ctx, target, db)...)
	findings = append(findings, ftsFindings(ctx, target, db)...)
	return findings
}

func integrityFindings(ctx context.Context, target DatabaseTarget, db *sql.DB) []Finding {
	var findings []Finding

	problems, err := queryStrings(ctx, db, `PRAGMA integrity_check`)
	switch {
	case err != nil:
		findings = append(findings, Finding{
			Title:    fmt.Sprintf("%s: integrity check failed to run", target.Name),
			Severity: SeverityError,
			Detail:   err.Error(),
		})
	case len(problems) == 1 && problems[0] == "ok":
		// Healthy.
	default:
		findings = append(findings, Finding{
			Title:    fmt.Sprintf("%s: integrity check reported %d problem(s)", target.Name, len(problems)),
			Severity: SeverityError,
			Detail:   strings.Join(truncateList(problems, 10), "\n"),
			Remedy:   "back up the file, then recover with: sqlite3 " + target.Path + " .recover",
		})
	}

	var violations int
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		findings = append(findings, Finding{
			Title:    fmt.Sprintf("%s: foreign key check failed to run", target.Name),
			Severity: SeverityWarn,
			Detail:   err.Error(),
		})
	} else {
		for rows.Next() {
			violations++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			findings = append(findings, Finding{
				Title:    fmt.Sprintf("%s: foreign key check failed", target.Name),
				Severity: SeverityWarn,
				Detail:   err.Error(),
			})
		} else if violations > 0 {
			findings = append(findings, Finding{
				Title:    fmt.Sprintf("%s: %d foreign key violation(s)", target.Name, violations),
				Severity: SeverityError,
				Remedy:   "run: term-llm doctor --only stale --fix to drop orphaned rows",
			})
		}
	}

	return findings
}

func schemaFindings(ctx context.Context, target DatabaseTarget, db *sql.DB) []Finding {
	if target.BuildReference == nil {
		return nil
	}

	actual, err := schemaSignature(ctx, db)
	if err != nil {
		return []Finding{{
			Title:    fmt.Sprintf("%s: cannot read schema", target.Name),
			Severity: SeverityError,
			Detail:   err.Error(),
		}}
	}

	reference, err := referenceSignature(ctx, target)
	if err != nil {
		return []Finding{{
			Title:    fmt.Sprintf("%s: cannot build reference schema", target.Name),
			Severity: SeverityError,
			Detail:   err.Error(),
			Remedy:   "this is a term-llm bug: migrations failed on a fresh database",
		}}
	}

	missing, extra := diffSignatures(reference, actual)
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}

	var findings []Finding
	if len(missing) > 0 {
		findings = append(findings, Finding{
			Title:    fmt.Sprintf("%s: schema is missing %d object(s) a fresh install would have", target.Name, len(missing)),
			Severity: SeverityError,
			Detail:   strings.Join(truncateList(missing, 20), "\n"),
			Remedy:   "a migration likely edited the baseline schema instead of adding a migration step; report this with the detail above",
		})
	}
	if len(extra) > 0 {
		findings = append(findings, Finding{
			Title:    fmt.Sprintf("%s: schema has %d object(s) a fresh install would not have", target.Name, len(extra)),
			Severity: SeverityWarn,
			Detail:   strings.Join(truncateList(extra, 20), "\n"),
			Remedy:   "usually leftovers from an older version; harmless unless queries fail",
		})
	}
	return findings
}

func referenceSignature(ctx context.Context, target DatabaseTarget) ([]string, error) {
	dir, err := os.MkdirTemp("", "term-llm-doctor-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, filepath.Base(target.Path))
	if err := target.BuildReference(ctx, path); err != nil {
		return nil, fmt.Errorf("migrate fresh database: %w", err)
	}

	db, err := openReadOnly(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open fresh database: %w", err)
	}
	defer db.Close()
	return schemaSignature(ctx, db)
}

func ftsFindings(ctx context.Context, target DatabaseTarget, db *sql.DB) []Finding {
	var findings []Finding
	for _, index := range target.FTS {
		if !validIdentifier(index.Name) || !validIdentifier(index.Source) {
			continue
		}
		exists, err := tableExists(ctx, db, index.Name)
		if err != nil || !exists {
			continue
		}
		// The docsize shadow table holds exactly one row per indexed document,
		// so it counts index contents without consulting the content table.
		sizeTable := index.Name + "_docsize"
		if exists, err := tableExists(ctx, db, sizeTable); err != nil || !exists {
			continue
		}
		indexed, err := countRows(ctx, db, sizeTable)
		if err != nil {
			continue
		}
		source, err := countRows(ctx, db, index.Source)
		if err != nil {
			continue
		}
		if indexed == source {
			continue
		}
		path := target.Path
		name := index.Name
		findings = append(findings, Finding{
			Title:    fmt.Sprintf("%s: %s is out of sync with %s", target.Name, index.Name, index.Source),
			Severity: SeverityWarn,
			Detail:   fmt.Sprintf("%d indexed rows vs %d source rows; search results will be incomplete", indexed, source),
			Remedy:   "rebuild the index with --fix",
			Fix: func(ctx context.Context) error {
				return rebuildFTS(ctx, path, name)
			},
		})
	}
	return findings
}

func rebuildFTS(ctx context.Context, path, index string) error {
	if !validIdentifier(index) {
		return fmt.Errorf("invalid index name %q", index)
	}
	db, err := openReadWrite(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close()
	statement := fmt.Sprintf(`INSERT INTO %q(%q) VALUES('rebuild')`, index, index)
	if _, err := db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("rebuild %s: %w", index, err)
	}
	return nil
}

// schemaSignature returns the normalized structural signature of a database,
// excluding SQLite's own bookkeeping objects such as sqlite_stat1, which appear
// only after ANALYZE and would otherwise read as schema drift.
func schemaSignature(ctx context.Context, db *sql.DB) ([]string, error) {
	signature, err := sqliteutil.SchemaSignature(ctx, db)
	if err != nil {
		return nil, err
	}
	filtered := make([]string, 0, len(signature))
	for _, entry := range signature {
		if isInternalSignatureEntry(entry) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered, nil
}

// isInternalSignatureEntry reports whether a signature entry describes a
// SQLite-owned object. Entries are pipe-delimited and start with their kind.
func isInternalSignatureEntry(entry string) bool {
	fields := strings.Split(entry, "|")
	switch {
	case len(fields) >= 3 && fields[0] == "master":
		return strings.HasPrefix(fields[2], "sqlite_")
	case len(fields) >= 2:
		return strings.HasPrefix(fields[1], "sqlite_")
	default:
		return false
	}
}

// diffSignatures returns entries present only in reference (missing) and only
// in actual (extra), rendered for humans.
func diffSignatures(reference, actual []string) (missing, extra []string) {
	referenceSet := make(map[string]bool, len(reference))
	for _, entry := range reference {
		referenceSet[entry] = true
	}
	actualSet := make(map[string]bool, len(actual))
	for _, entry := range actual {
		actualSet[entry] = true
	}
	referenceTables := signatureTableNames(reference)
	actualTables := signatureTableNames(actual)
	return summarizeDiff(reference, actualSet, actualTables), summarizeDiff(actual, referenceSet, referenceTables)
}

// signatureTableNames collects the table names a signature defines.
func signatureTableNames(entries []string) map[string]bool {
	names := map[string]bool{}
	for _, entry := range entries {
		fields := strings.Split(entry, "|")
		if len(fields) >= 3 && fields[0] == "master" && fields[1] == "table" {
			names[fields[2]] = true
		}
	}
	return names
}

// summarizeDiff describes the entries of one side that the other side lacks.
// Columns and foreign keys of a table that is itself absent are folded into the
// table entry: listing every column of a missing table buries the useful line.
func summarizeDiff(entries []string, other map[string]bool, otherTables map[string]bool) []string {
	var out []string
	for _, entry := range entries {
		if other[entry] {
			continue
		}
		fields := strings.Split(entry, "|")
		switch {
		case len(fields) >= 3 && fields[0] == "master" && fields[1] == "table":
			if otherTables[fields[2]] {
				// The table exists on both sides with a different definition;
				// the column entries below say what actually changed.
				out = append(out, fmt.Sprintf("table %s (definition differs)", fields[2]))
				continue
			}
		case len(fields) >= 2 && (fields[0] == "column" || fields[0] == "foreign-key"):
			if !otherTables[fields[1]] {
				continue
			}
		case len(fields) >= 4 && fields[0] == "master":
			if !otherTables[fields[3]] {
				continue
			}
		}
		out = append(out, describeSignatureEntry(entry))
	}
	sort.Strings(out)
	return dedupe(out)
}

// describeSignatureEntry turns a raw signature entry into a short description.
func describeSignatureEntry(entry string) string {
	fields := strings.Split(entry, "|")
	switch {
	case fields[0] == "master" && len(fields) >= 3:
		return fmt.Sprintf("%s %s", fields[1], fields[2])
	case fields[0] == "column" && len(fields) >= 4:
		return fmt.Sprintf("column %s.%s (%s)", fields[1], fields[2], fields[3])
	case fields[0] == "foreign-key" && len(fields) >= 5:
		return fmt.Sprintf("foreign key on %s -> %s", fields[1], fields[4])
	default:
		return entry
	}
}

func dedupe(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := values[:0]
	var previous string
	for i, value := range values {
		if i > 0 && value == previous {
			continue
		}
		out = append(out, value)
		previous = value
	}
	return out
}

func truncateList(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	out := make([]string, 0, limit+1)
	out = append(out, values[:limit]...)
	return append(out, fmt.Sprintf("... and %d more", len(values)-limit))
}

var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validIdentifier(name string) bool { return identifierPattern.MatchString(name) }

func openReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	dsn := sqliteutil.FileURI(path) + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	return openSQLite(ctx, dsn)
}

func openReadWrite(ctx context.Context, path string) (*sql.DB, error) {
	dsn := sqliteutil.FileURI(path) + "?_pragma=busy_timeout(5000)"
	return openSQLite(ctx, dsn)
}

func openSQLite(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func queryStrings(ctx context.Context, db *sql.DB, query string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

func tableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var exists int
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)`, table).Scan(&exists)
	return exists != 0, err
}

func countRows(ctx context.Context, db *sql.DB, table string) (int64, error) {
	if !validIdentifier(table) {
		return 0, fmt.Errorf("invalid table name %q", table)
	}
	var count int64
	err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %q`, table)).Scan(&count)
	return count, err
}

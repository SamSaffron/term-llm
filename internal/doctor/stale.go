package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/filelock"
)

// DefaultWALWarnBytes is the write-ahead log size above which doctor suggests a
// checkpoint. WAL files this large usually mean a process exited without
// checkpointing rather than ongoing write pressure.
const DefaultWALWarnBytes = 64 << 20

// OrphanRule counts and optionally deletes rows left behind by interrupted work.
type OrphanRule struct {
	// Label describes the rows, for example "messages without a session".
	Label string
	// CountSQL must select a single integer.
	CountSQL string
	// DeleteSQL removes the rows. Empty makes the finding report-only.
	DeleteSQL string
}

// OrphanTarget binds orphan rules to a database file.
type OrphanTarget struct {
	Name  string
	Path  string
	Rules []OrphanRule
}

// StaleCheck finds leftover files and rows that no longer belong to anything:
// journal files without a database, oversized write-ahead logs, unheld lock
// files, and orphaned rows.
type StaleCheck struct {
	// DataDir is the term-llm data directory. Empty skips file scanning.
	DataDir string
	// ConfigDir is the term-llm config directory. Empty skips lock scanning.
	ConfigDir string
	// Orphans lists databases with orphan-row rules.
	Orphans []OrphanTarget
	// WALWarnBytes overrides DefaultWALWarnBytes.
	WALWarnBytes int64
}

// ID implements Check.
func (c *StaleCheck) ID() string { return "stale" }

// Title implements Check.
func (c *StaleCheck) Title() string { return "Stale state" }

// Run implements Check.
func (c *StaleCheck) Run(ctx context.Context) []Finding {
	var findings []Finding
	findings = append(findings, c.journalFindings()...)
	findings = append(findings, c.lockFindings()...)
	findings = append(findings, c.orphanFindings(ctx)...)
	return findings
}

// journalFindings reports -wal/-shm files whose database is gone, and write
// ahead logs that have grown past the checkpoint threshold.
func (c *StaleCheck) journalFindings() []Finding {
	if c.DataDir == "" {
		return nil
	}
	limit := c.WALWarnBytes
	if limit <= 0 {
		limit = DefaultWALWarnBytes
	}

	var findings []Finding
	for _, dir := range scanDirs(c.DataDir) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			suffix := ""
			switch {
			case strings.HasSuffix(name, "-wal"):
				suffix = "-wal"
			case strings.HasSuffix(name, "-shm"):
				suffix = "-shm"
			default:
				continue
			}
			journalPath := filepath.Join(dir, name)
			dbPath := strings.TrimSuffix(journalPath, suffix)
			if _, err := os.Stat(dbPath); err != nil {
				if !os.IsNotExist(err) {
					continue
				}
				findings = append(findings, Finding{
					Title:    fmt.Sprintf("orphaned journal file %s", name),
					Severity: SeverityWarn,
					Detail:   journalPath + " has no matching database",
					Remedy:   "delete it with --fix",
					Fix: func(ctx context.Context) error {
						return os.Remove(journalPath)
					},
				})
				continue
			}
			if suffix != "-wal" {
				continue
			}
			info, err := entry.Info()
			if err != nil || info.Size() < limit {
				continue
			}
			findings = append(findings, Finding{
				Title:    fmt.Sprintf("%s write-ahead log is %s", filepath.Base(dbPath), humanBytes(info.Size())),
				Severity: SeverityInfo,
				Detail:   journalPath + " was probably left by a process that exited without checkpointing",
				Remedy:   "checkpoint it with --fix to reclaim the space",
				Fix: func(ctx context.Context) error {
					return checkpointWAL(ctx, dbPath)
				},
			})
		}
	}
	sortFindings(findings)
	return findings
}

// lockFindings reports service install locks that no process holds. The lock
// itself is advisory and released when its owner exits, so a leftover file is
// harmless but confusing.
func (c *StaleCheck) lockFindings() []Finding {
	if c.ConfigDir == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(c.ConfigDir, "services", "*", "install.lock"))
	if err != nil {
		return nil
	}
	sort.Strings(matches)

	var findings []Finding
	for _, match := range matches {
		unlock, err := filelock.TryLock(match)
		if err != nil {
			findings = append(findings, Finding{
				Title:    fmt.Sprintf("service install lock is held: %s", filepath.Base(filepath.Dir(match))),
				Severity: SeverityInfo,
				Detail:   match + " is locked by a running install or update",
			})
			continue
		}
		_ = unlock()
		lockPath := match
		findings = append(findings, Finding{
			Title:    fmt.Sprintf("leftover install lock file for the %s service", filepath.Base(filepath.Dir(match))),
			Severity: SeverityInfo,
			Detail:   lockPath + " is not held by any process",
			Remedy:   "remove it with --fix",
			Fix: func(ctx context.Context) error {
				return os.Remove(lockPath)
			},
		})
	}
	return findings
}

func (c *StaleCheck) orphanFindings(ctx context.Context) []Finding {
	var findings []Finding
	for _, target := range c.Orphans {
		if _, err := os.Stat(target.Path); err != nil {
			continue
		}
		db, err := openReadOnly(ctx, target.Path)
		if err != nil {
			continue
		}
		for _, rule := range target.Rules {
			var count int64
			if err := db.QueryRowContext(ctx, rule.CountSQL).Scan(&count); err != nil || count == 0 {
				continue
			}
			noun := "rows"
			if count == 1 {
				noun = "row"
			}
			finding := Finding{
				Title:    fmt.Sprintf("%s: %d orphaned %s (%s)", target.Name, count, noun, rule.Label),
				Severity: SeverityWarn,
				Detail:   "usually left behind by an interrupted run",
			}
			if rule.DeleteSQL != "" {
				path := target.Path
				statement := rule.DeleteSQL
				finding.Remedy = "delete them with --fix"
				finding.Fix = func(ctx context.Context) error {
					return execWrite(ctx, path, statement)
				}
			}
			findings = append(findings, finding)
		}
		db.Close()
	}
	return findings
}

func checkpointWAL(ctx context.Context, path string) error {
	db, err := openReadWrite(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint %s: %w", filepath.Base(path), err)
	}
	return nil
}

func execWrite(ctx context.Context, path, statement string) error {
	db, err := openReadWrite(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

// scanDirs returns root plus its immediate subdirectories, which is where every
// term-llm database lives.
func scanDirs(root string) []string {
	dirs := []string{root}
	entries, err := os.ReadDir(root)
	if err != nil {
		return dirs
	}
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(root, entry.Name()))
		}
	}
	return dirs
}

func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Title < findings[j].Title })
}

func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	index := -1
	for value >= unit && index < len(units)-1 {
		value /= unit
		index++
	}
	return fmt.Sprintf("%.1f %s", value, units[index])
}

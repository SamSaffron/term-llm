package evalfixture

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// InitializeWorkspace creates deterministic JSON/files, a Git repository, and
// SQLite data used by the federation. It is safe to rerun between evaluations.
func InitializeWorkspace(root string) error {
	for _, dir := range []string{"state", "records", "documents", "repos", "databases"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(root, "records", "federation.json"), []byte(`{
  "pull_request": {"number": 42, "mergeable": true, "reviews": "approved", "checks": "passed"},
  "incident": {"id": "INC-7", "status": "open"},
  "customer": {"id": "C-104", "name": "Alex Chen"},
  "order": {"id": "ORD-77", "status": "unfulfilled", "paid": true}
}
`), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "documents", "DOC-5.md"), []byte("# Fixture operations\n\nDeployment and incident response handbook.\n"), 0644); err != nil {
		return err
	}
	if err := initializeSQLite(filepath.Join(root, "databases", "evaluation.sqlite")); err != nil {
		return err
	}
	if err := initializeGit(filepath.Join(root, "repos", "acme-app")); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "calls.jsonl"), nil, 0644)
}

func initializeSQLite(path string) error {
	_ = os.Remove(path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	statements := []string{
		`CREATE TABLE customers (id TEXT PRIMARY KEY, name TEXT, email TEXT)`,
		`CREATE TABLE orders (id TEXT PRIMARY KEY, customer_id TEXT, status TEXT, paid INTEGER)`,
		`CREATE TABLE incidents (id TEXT PRIMARY KEY, status TEXT, summary TEXT)`,
		`INSERT INTO customers VALUES ('C-104','Alex Chen','alex@example.test')`,
		`INSERT INTO orders VALUES ('ORD-77','C-104','unfulfilled',1)`,
		`INSERT INTO incidents VALUES ('INC-7','open','Payment timeout errors')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("initialize SQLite fixture: %w", err)
		}
	}
	return nil
}

func initializeGit(path string) error {
	_ = os.RemoveAll(path)
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	commands := [][]string{{"init", "-q"}, {"config", "user.email", "fixture@example.test"}, {"config", "user.name", "Fixture Bot"}}
	for _, args := range commands {
		if output, err := exec.Command("git", append([]string{"-C", path}, args...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("# acme-app fixture\n"), 0644); err != nil {
		return err
	}
	cmd := exec.Command("git", "-C", path, "add", "README.md")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w: %s", err, output)
	}
	cmd = exec.Command("git", "-C", path, "commit", "-q", "-m", "Initial fixture")
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, output)
	}
	return nil
}

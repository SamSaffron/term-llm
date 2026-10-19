package testutil

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/session"
)

// NewSessionInputStore opens a real SQLite store and closes it at test cleanup.
// Both CLI and host input tests use it so their namespace/refresh behavior is real.
func NewSessionInputStore(t testing.TB, path string) session.Store {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

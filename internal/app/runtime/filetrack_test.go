package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestFileTrackingStoreReusesSwitchesAndClosesCache(t *testing.T) {
	CloseFileTrackingStore()
	t.Cleanup(CloseFileTrackingStore)
	cfg := &config.Config{FileTracking: config.FileTrackingConfig{Enabled: true, Path: filepath.Join(t.TempDir(), "first.db")}}
	cfg.Sessions.Path = filepath.Join(t.TempDir(), "sessions.db")
	first := FileTrackingStore(cfg)
	if first == nil {
		t.Fatal("first cache store unavailable")
	}
	if got := FileTrackingStore(cfg); got != first {
		t.Fatal("same configuration did not reuse cache")
	}
	cfg.FileTracking.Path = filepath.Join(t.TempDir(), "second.db")
	second := FileTrackingStore(cfg)
	if second == nil || second == first {
		t.Fatal("path change did not replace cache")
	}
	if _, err := first.ListSessionChanges(context.Background(), "session"); err == nil {
		t.Fatal("replaced store remained open")
	}
	CloseFileTrackingStore()
	if _, err := second.ListSessionChanges(context.Background(), "session"); err == nil {
		t.Fatal("explicit cache close left the store open")
	}
	third := FileTrackingStore(cfg)
	if third == nil || third == second {
		t.Fatal("closed cache was reused")
	}
	cfg.FileTracking.Enabled = false
	if FileTrackingStore(cfg) != nil {
		t.Fatal("disabled configuration retained a store")
	}
	if _, err := third.ListSessionChanges(context.Background(), "session"); err == nil {
		t.Fatal("disabled store remained open")
	}
	if FileTrackingStore(nil) != nil {
		t.Fatal("nil configuration opened a store")
	}
}

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestParseLiveDecisionsSince(t *testing.T) {
	now := time.Date(2026, 9, 19, 1, 2, 3, 0, time.UTC)
	got, err := parseLiveDecisionsSince("24h", now)
	if err != nil || !got.Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("duration since = %v, %v", got, err)
	}
	got, err = parseLiveDecisionsSince("2026-09-18T00:00:00Z", now)
	if err != nil || got.Format(time.RFC3339) != "2026-09-18T00:00:00Z" {
		t.Fatalf("timestamp since = %v, %v", got, err)
	}
	if _, err := parseLiveDecisionsSince("yesterday", now); err == nil {
		t.Fatal("invalid --since succeeded")
	}
}

func TestLiveDecisionsDoesNotCreateDatabaseWhenAbsent(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	oldSince := liveDecisionsSince
	liveDecisionsSince = ""
	t.Cleanup(func() { liveDecisionsSince = oldSince })
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runLiveDecisions(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No decisions recorded") {
		t.Fatalf("output = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(dataHome, "term-llm", "diagnostics")); !os.IsNotExist(err) {
		t.Fatalf("diagnostics directory was created: %v", err)
	}
}

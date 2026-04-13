package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordModelUse(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	// First record
	if err := RecordModelUse("anthropic:claude-sonnet-4-20250514"); err != nil {
		t.Fatalf("RecordModelUse: %v", err)
	}

	entries, err := LoadModelHistory()
	if err != nil {
		t.Fatalf("LoadModelHistory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Model != "anthropic:claude-sonnet-4-20250514" {
		t.Fatalf("unexpected model: %s", entries[0].Model)
	}

	// Second record bumps to top
	if err := RecordModelUse("openai:gpt-4o"); err != nil {
		t.Fatalf("RecordModelUse: %v", err)
	}
	if err := RecordModelUse("anthropic:claude-sonnet-4-20250514"); err != nil {
		t.Fatalf("RecordModelUse: %v", err)
	}

	entries, err = LoadModelHistory()
	if err != nil {
		t.Fatalf("LoadModelHistory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Model != "anthropic:claude-sonnet-4-20250514" {
		t.Fatalf("expected claude first, got %s", entries[0].Model)
	}
	if entries[1].Model != "openai:gpt-4o" {
		t.Fatalf("expected gpt-4o second, got %s", entries[1].Model)
	}
}

func TestRecordModelUse_Cap(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	for i := 0; i < 25; i++ {
		if err := RecordModelUse("provider:model-" + string(rune('a'+i))); err != nil {
			t.Fatalf("RecordModelUse: %v", err)
		}
	}

	entries, err := LoadModelHistory()
	if err != nil {
		t.Fatalf("LoadModelHistory: %v", err)
	}
	if len(entries) != maxModelHistoryEntries {
		t.Fatalf("expected %d entries, got %d", maxModelHistoryEntries, len(entries))
	}
}

func TestLoadModelHistory_Missing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	entries, err := LoadModelHistory()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries != nil {
		t.Fatalf("expected nil, got %v", entries)
	}
}

func TestLoadModelHistory_Corrupt(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	dir := filepath.Join(tmp, "term-llm")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "model_history.json"), []byte("not json"), 0o600)

	entries, err := LoadModelHistory()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries != nil {
		t.Fatalf("expected nil for corrupt file, got %v", entries)
	}
}

func TestModelHistoryOrder(t *testing.T) {
	entries := []ModelHistoryEntry{
		{Model: "a:b", UsedAt: time.Now()},
		{Model: "c:d", UsedAt: time.Now()},
	}
	order := ModelHistoryOrder(entries)
	if len(order) != 2 || order[0] != "a:b" || order[1] != "c:d" {
		t.Fatalf("unexpected order: %v", order)
	}
}

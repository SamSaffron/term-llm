package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/tools"
)

type countingImageRecordStore struct {
	recordCalls int
	closeCalls  int
}

func (s *countingImageRecordStore) RecordImage(_ context.Context, _ *memorydb.ImageRecord) error {
	s.recordCalls++
	return nil
}

func (s *countingImageRecordStore) Close() error {
	s.closeCalls++
	return nil
}

func TestWireImageRecorderLazilyOpensAndClosesStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	oldMemoryDBPath := memoryDBPath
	memoryDBPath = dbPath
	t.Cleanup(func() { memoryDBPath = oldMemoryDBPath })

	for range 100 {
		toolConfig := tools.DefaultToolConfig()
		toolConfig.Enabled = nil
		toolMgr, err := tools.NewToolManager(&toolConfig, &config.Config{})
		if err != nil {
			t.Fatalf("NewToolManager: %v", err)
		}
		wireImageRecorder(toolMgr.Registry, "jarvis", "session-id")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("memory store opened during recorder wiring: stat error = %v", err)
	}

	store := &countingImageRecordStore{}
	openCalls := 0
	recorder := &lazyImageRecorder{open: func() (imageRecordStore, error) {
		openCalls++
		return store, nil
	}}
	if err := recorder.RecordImage(context.Background(), &memorydb.ImageRecord{}); err != nil {
		t.Fatalf("RecordImage: %v", err)
	}
	if openCalls != 1 {
		t.Fatalf("store open calls = %d, want 1", openCalls)
	}
	if store.recordCalls != 1 {
		t.Fatalf("store RecordImage calls = %d, want 1", store.recordCalls)
	}
	if store.closeCalls != 1 {
		t.Fatalf("store Close calls = %d, want 1", store.closeCalls)
	}
}

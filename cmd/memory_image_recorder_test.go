package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/tools"
)

type trackingImageRecordStore struct {
	records   []*memorydb.ImageRecord
	closes    int
	recordErr error
}

func (s *trackingImageRecordStore) RecordImage(_ context.Context, record *memorydb.ImageRecord) error {
	clone := *record
	s.records = append(s.records, &clone)
	return s.recordErr
}

func (s *trackingImageRecordStore) Close() error {
	s.closes++
	return nil
}

func TestWireImageRecorderIsLazyAndRecordsWithAttribution(t *testing.T) {
	outputDir := t.TempDir()
	cfg := &config.Config{
		Image: config.ImageConfig{
			Provider:  "debug",
			OutputDir: outputDir,
		},
	}
	toolConfig := &tools.ToolConfig{
		Enabled:       []string{tools.ImageGenerateToolName},
		WriteDirs:     []string{outputDir},
		ImageProvider: "debug",
	}
	registry, err := tools.NewLocalToolRegistry(toolConfig, cfg, nil)
	if err != nil {
		t.Fatalf("NewLocalToolRegistry: %v", err)
	}

	oldMemoryDBPath := memoryDBPath
	memoryDBPath = filepath.Join(t.TempDir(), "memory.db")
	t.Cleanup(func() { memoryDBPath = oldMemoryDBPath })
	t.Setenv("TERM_LLM_MEMORY_DB", "")

	store := &trackingImageRecordStore{}
	opens := 0
	wireImageRecorderWithOpener(registry, "jarvis", "session-123", func(path string) (imageRecordStore, error) {
		opens++
		if path != memoryDBPath {
			t.Errorf("opener path = %q, want %q", path, memoryDBPath)
		}
		return store, nil
	})
	if opens != 0 {
		t.Fatalf("wiring opened the memory store %d times, want 0", opens)
	}

	tool, ok := registry.Get(tools.ImageGenerateToolName)
	if !ok {
		t.Fatal("image_generate tool not found")
	}
	args, err := json.Marshal(tools.ImageGenerateArgs{
		Prompt:          "a reliable robot",
		ShowImage:       boolPointer(false),
		CopyToClipboard: boolPointer(false),
	})
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if opens != 1 {
		t.Fatalf("memory store opens = %d, want 1", opens)
	}
	if store.closes != 1 {
		t.Fatalf("memory store closes = %d, want 1", store.closes)
	}
	if len(store.records) != 1 {
		t.Fatalf("recorded images = %d, want 1", len(store.records))
	}
	record := store.records[0]
	if record.Agent != "jarvis" || record.SessionID != "session-123" || record.Prompt != "a reliable robot" {
		t.Fatalf("record attribution = agent %q, session %q, prompt %q", record.Agent, record.SessionID, record.Prompt)
	}
	if record.OutputPath == "" {
		t.Fatal("record output path is empty")
	}
}

func TestLazyImageRecorderOpensAndClosesPerAttempt(t *testing.T) {
	recordErr := errors.New("record failed")
	var stores []*trackingImageRecordStore
	recorder := &lazyImageRecorder{
		path: "memory.db",
		open: func(path string) (imageRecordStore, error) {
			if path != "memory.db" {
				t.Fatalf("opener path = %q, want memory.db", path)
			}
			store := &trackingImageRecordStore{recordErr: recordErr}
			stores = append(stores, store)
			return store, nil
		},
	}

	for i := 0; i < 2; i++ {
		err := recorder.RecordImage(context.Background(), &memorydb.ImageRecord{Prompt: "test"})
		if !errors.Is(err, recordErr) {
			t.Fatalf("RecordImage attempt %d error = %v, want %v", i+1, err, recordErr)
		}
	}
	if len(stores) != 2 {
		t.Fatalf("opened stores = %d, want 2", len(stores))
	}
	for i, store := range stores {
		if store.closes != 1 {
			t.Fatalf("store %d closes = %d, want 1", i+1, store.closes)
		}
	}
}

func boolPointer(value bool) *bool {
	return &value
}

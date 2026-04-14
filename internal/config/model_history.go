package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxModelHistoryEntries = 20

var (
	modelHistoryMu        sync.Mutex
	modelHistoryAsyncOnce sync.Once
	modelHistoryAsyncCh   chan modelHistoryAsyncReq
)

type modelHistoryAsyncReq struct {
	providerModel string
	path          string
	done          chan struct{}
}

// ModelHistoryEntry records a single model usage event.
type ModelHistoryEntry struct {
	Model  string    `json:"model"`
	UsedAt time.Time `json:"used_at"`
}

// LoadModelHistory reads the model history file.
// Returns nil (no error) when the file doesn't exist yet.
func LoadModelHistory() ([]ModelHistoryEntry, error) {
	path, err := modelHistoryPath()
	if err != nil {
		return nil, err
	}

	modelHistoryMu.Lock()
	defer modelHistoryMu.Unlock()
	return loadModelHistoryLocked(path)
}

// RecordModelUse adds or bumps a "provider:model" entry to the front of the
// history list, deduplicating and capping at maxModelHistoryEntries.
func RecordModelUse(providerModel string) error {
	providerModel = strings.TrimSpace(providerModel)
	if providerModel == "" {
		return nil
	}

	path, err := modelHistoryPath()
	if err != nil {
		return err
	}

	return recordModelUseAtPath(path, providerModel)
}

func recordModelUseAtPath(path, providerModel string) error {
	modelHistoryMu.Lock()
	defer modelHistoryMu.Unlock()

	entries, _ := loadModelHistoryLocked(path) // ignore read errors; start fresh

	now := time.Now().UTC()
	updated := []ModelHistoryEntry{{Model: providerModel, UsedAt: now}}
	for _, e := range entries {
		if e.Model != providerModel {
			updated = append(updated, e)
		}
	}
	if len(updated) > maxModelHistoryEntries {
		updated = updated[:maxModelHistoryEntries]
	}

	return saveModelHistoryLocked(path, updated)
}

// RecordModelUseAsync queues a best-effort background MRU update.
// Updates are serialized through a single worker to preserve ordering.
func RecordModelUseAsync(providerModel string) {
	providerModel = strings.TrimSpace(providerModel)
	if providerModel == "" {
		return
	}

	path, err := modelHistoryPath()
	if err != nil {
		return
	}

	modelHistoryAsyncOnce.Do(func() {
		modelHistoryAsyncCh = make(chan modelHistoryAsyncReq, 64)
		go func() {
			for req := range modelHistoryAsyncCh {
				if req.done != nil {
					close(req.done)
					continue
				}
				_ = recordModelUseAtPath(req.path, req.providerModel)
			}
		}()
	})

	modelHistoryAsyncCh <- modelHistoryAsyncReq{providerModel: providerModel, path: path}
}

// FlushModelHistoryAsync waits for queued async history writes to complete.
// Intended for tests and orderly shutdown paths.
func FlushModelHistoryAsync() {
	if modelHistoryAsyncCh == nil {
		return
	}
	done := make(chan struct{})
	modelHistoryAsyncCh <- modelHistoryAsyncReq{done: done}
	<-done
}

// ModelHistoryOrder returns model IDs ordered by most-recently-used.
// The returned slice contains only the "provider:model" strings.
func ModelHistoryOrder(entries []ModelHistoryEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Model
	}
	return out
}

func modelHistoryPath() (string, error) {
	dir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "model_history.json"), nil
}

func loadModelHistoryLocked(path string) ([]ModelHistoryEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var entries []ModelHistoryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, nil // treat corrupt file as empty
	}
	return entries, nil
}

func saveModelHistoryLocked(path string, entries []ModelHistoryEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

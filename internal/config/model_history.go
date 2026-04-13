package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const maxModelHistoryEntries = 20

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

// RecordModelUse adds or bumps a "provider:model" entry to the front of the
// history list, deduplicating and capping at maxModelHistoryEntries.
func RecordModelUse(providerModel string) error {
	entries, _ := LoadModelHistory() // ignore read errors; start fresh

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

	return saveModelHistory(updated)
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

func saveModelHistory(entries []ModelHistoryEntry) error {
	path, err := modelHistoryPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

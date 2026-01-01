package update

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
)

const updateStateFilename = "update-state.json"

type State struct {
	LastChecked     time.Time `json:"lastChecked"`
	LatestVersion   string    `json:"latestVersion"`
	LastError       string    `json:"lastError,omitempty"`
	NotifiedVersion string    `json:"notifiedVersion,omitempty"`
	LastNotified    time.Time `json:"lastNotified,omitempty"`
}

func LoadState() (*State, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return nil, err
	}
	return loadStateFromDir(configDir)
}

func loadStateFromDir(configDir string) (*State, error) {
	path := filepath.Join(configDir, updateStateFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &State{}, nil
		}
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func SaveState(state *State) error {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return err
	}
	return saveStateToDir(configDir, state)
}

func saveStateToDir(configDir string, state *State) error {
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write via temp file + rename
	tmp, err := os.CreateTemp(configDir, "update-state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	path := filepath.Join(configDir, updateStateFilename)
	return os.Rename(tmp.Name(), path)
}

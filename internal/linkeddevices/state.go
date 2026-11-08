package linkeddevices

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/samsaffron/term-llm/internal/fileprivacy"
)

const stateVersion = 1

type targetState struct {
	Version         int    `json:"version"`
	Target          string `json:"target"`
	StoreInstanceID string `json:"store_instance_id"`
	BrokerURL       string `json:"broker_url"`
}

type settingsState struct {
	Version int  `json:"version"`
	Enabled bool `json:"enabled"`
}

func loadJSON(path string, dst any) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if err := fileprivacy.CheckFile(info); err != nil {
		return false, fmt.Errorf("linked devices state %s is not private: %w", path, err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return false, fmt.Errorf("decode %s: %w", path, err)
	}
	return true, nil
}

func writeJSONPrivate(path string, value any) error {
	dir := filepath.Dir(path)
	if err := fileprivacy.MkdirAll(dir); err != nil {
		return err
	}
	if err := fileprivacy.ChmodDir(dir, fileprivacy.DirMode); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return fileprivacy.WriteFileAtomic(path, data)
}

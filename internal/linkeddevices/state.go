package linkeddevices

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	if info.Mode().Perm()&0o077 != 0 {
		return false, fmt.Errorf("linked devices state %s is not private", path)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return false, fmt.Errorf("decode %s: %w", path, err)
	}
	return true, nil
}

func writeJSONPrivate(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".linked-devices-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

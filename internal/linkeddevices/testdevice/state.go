package testdevice

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/samsaffron/term-llm/internal/fileprivacy"
	"path/filepath"

	"github.com/samsaffron/term-llm/internal/pairing"
)

const stateVersion = 1

type persistedState struct {
	Version             int         `json:"version"`
	DeviceName          string      `json:"device_name"`
	IdentitySeed        pairing.B64 `json:"identity_seed"`
	Broker              string      `json:"broker,omitempty"`
	HostKey             pairing.B64 `json:"host_key,omitempty"`
	DeviceID            pairing.B64 `json:"device_id,omitempty"`
	ReconnectCapability pairing.B64 `json:"reconnect_capability,omitempty"`
	Scope               string      `json:"scope,omitempty"`
	BasePath            string      `json:"base_path,omitempty"`
	HostLabel           string      `json:"host_label,omitempty"`
}

func newPersistedState(deviceName string) (persistedState, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return persistedState{}, fmt.Errorf("testdevice: generate identity: %w", err)
	}
	return persistedState{Version: stateVersion, DeviceName: deviceName, IdentitySeed: seed}, nil
}

func loadState(path string) (persistedState, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return persistedState{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return persistedState{}, errors.New("testdevice: state path is not a regular file")
	}
	if err := fileprivacy.CheckFile(info); err != nil {
		return persistedState{}, fmt.Errorf("testdevice: state file %s must not be accessible by group or others: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return persistedState{}, fmt.Errorf("testdevice: read state: %w", err)
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return persistedState{}, fmt.Errorf("testdevice: decode state: %w", err)
	}
	if err := state.validate(); err != nil {
		return persistedState{}, err
	}
	return state, nil
}

func (s persistedState) validate() error {
	if s.Version != stateVersion || len(s.IdentitySeed) != ed25519.SeedSize {
		return errors.New("testdevice: invalid or unsupported state file")
	}
	if err := pairing.ValidateDeviceName(s.DeviceName); err != nil {
		return fmt.Errorf("testdevice: state device name: %w", err)
	}
	if !s.paired() {
		if s.Broker != "" || len(s.HostKey) != 0 || len(s.DeviceID) != 0 || len(s.ReconnectCapability) != 0 || s.Scope != "" || s.BasePath != "" || s.HostLabel != "" {
			return errors.New("testdevice: incomplete pairing state")
		}
		return nil
	}
	if s.Broker == "" || len(s.HostKey) != ed25519.PublicKeySize || len(s.DeviceID) != pairing.DeviceIDSize || len(s.ReconnectCapability) != pairing.SecretSize || s.Scope != pairing.ScopeWebV1 || s.BasePath == "" {
		return errors.New("testdevice: incomplete pairing state")
	}
	return nil
}

func (s persistedState) paired() bool {
	return s.Broker != "" && len(s.DeviceID) != 0
}

func writeState(path string, state persistedState) error {
	if err := state.validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("testdevice: encode state: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := fileprivacy.MkdirAll(dir); err != nil {
		return fmt.Errorf("testdevice: create state directory: %w", err)
	}
	if err := fileprivacy.ChmodDir(dir, fileprivacy.DirMode); err != nil {
		return fmt.Errorf("testdevice: protect state directory: %w", err)
	}
	if err := fileprivacy.WriteFileAtomic(path, data); err != nil {
		return fmt.Errorf("testdevice: install state: %w", err)
	}
	return nil
}

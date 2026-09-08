package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const stateEnv = "TERM_LLM_RELOAD_STATE"
const maxReloadState = 16 << 20

// Only completed conversation/UI state belongs here, never a running operation.
// The hint is scrubbed before tools can start. A private file avoids argv/env size
// limits and is consumed once by the same OS process's next executable instance.
var incomingState = func() string { v := os.Getenv(stateEnv); _ = os.Unsetenv(stateEnv); return v }()
var stateMu sync.Mutex
var outgoingStates = make(map[string]string)

type stateEnvelope struct {
	PID      int             `json:"pid"`
	Start    string          `json:"start"`
	Instance string          `json:"instance"`
	Kind     string          `json:"kind"`
	Payload  json.RawMessage `json:"payload"`
}

func SaveState(kind string, value any) (func(context.Context), error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	start, err := selfStart()
	if err != nil {
		return nil, err
	}
	instance := Instance()
	if instance == "" {
		return nil, errors.New("reload state requires process identity")
	}
	raw, err := json.Marshal(stateEnvelope{os.Getpid(), start, instance, kind, payload})
	if err != nil {
		return nil, err
	}
	if len(raw) > maxReloadState {
		return nil, errors.New("reload UI state is too large")
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	if outgoingStates[kind] != "" {
		return nil, errors.New("reload already has state prepared")
	}
	dir, err := os.MkdirTemp("", fmt.Sprintf("term-llm-reload-%d-", os.Getpid()))
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		_ = os.Remove(dir)
		return nil, err
	}
	outgoingStates[kind] = path
	var once sync.Once
	return func(context.Context) {
		once.Do(func() {
			stateMu.Lock()
			defer stateMu.Unlock()
			_ = os.Remove(path)
			_ = os.Remove(dir)
			if outgoingStates[kind] == path {
				delete(outgoingStates, kind)
			}
		})
	}, nil
}

func HandoffEnviron() []string {
	stateMu.Lock()
	defer stateMu.Unlock()
	if len(outgoingStates) == 0 {
		return nil
	}
	if len(outgoingStates) == 1 {
		for _, path := range outgoingStates {
			return []string{stateEnv + "=" + path}
		}
	}
	raw, _ := json.Marshal(outgoingStates)
	return []string{stateEnv + "=" + string(raw)}
}

func RestoreState(kind string, value any) (bool, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	if incomingState == "" {
		return false, nil
	}
	path := incomingState
	var bundle map[string]string
	if strings.HasPrefix(path, "{") {
		if err := json.Unmarshal([]byte(path), &bundle); err != nil {
			return false, err
		}
		path = bundle[kind]
		if path == "" {
			return false, nil
		}
	}
	dir := filepath.Dir(path)
	if !filepath.IsAbs(path) || filepath.Base(path) != "state.json" || !strings.HasPrefix(filepath.Base(dir), fmt.Sprintf("term-llm-reload-%d-", os.Getpid())) {
		return false, errors.New("invalid reload state path")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ownedState(info) {
		return false, errors.New("reload state directory is not owner-private")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, err
	}
	defer root.Close()
	before, err := root.Lstat("state.json")
	if err != nil {
		return false, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm() != 0600 || !ownedState(before) {
		return false, errors.New("reload state is not a private regular file")
	}
	f, err := root.Open("state.json")
	if err != nil {
		return false, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !os.SameFile(before, after) || after.Size() > maxReloadState {
		return false, errors.New("reload state identity/size changed")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxReloadState+1))
	if err != nil {
		return false, err
	}
	var state stateEnvelope
	if err = json.Unmarshal(raw, &state); err != nil {
		return false, err
	}
	start, err := selfStart()
	if err != nil {
		return false, err
	}
	if state.PID != os.Getpid() || state.Start != start || state.Instance == "" || state.Instance == Instance() {
		return false, errors.New("reload state belongs to a different process or mode")
	}
	// Combined modes may probe a legacy single-section hint for another component.
	// Validate process identity first and leave that section for its owner.
	if state.Kind != kind {
		return false, nil
	}
	if err = json.Unmarshal(state.Payload, value); err != nil {
		return false, err
	}
	incomingState = ""
	if len(bundle) > 1 {
		delete(bundle, kind)
		remaining, _ := json.Marshal(bundle)
		incomingState = string(remaining)
	}
	_ = f.Close()
	_ = root.Remove("state.json")
	_ = root.Close()
	_ = os.Remove(dir)
	return true, nil
}

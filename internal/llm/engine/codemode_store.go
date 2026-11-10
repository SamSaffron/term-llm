package engine

import (
	"encoding/json"
	"fmt"
	"github.com/samsaffron/term-llm/internal/codemode"
	"sort"
	"strings"
	"sync"
)

// codeModeStore is engine/session memory, not durable conversation storage.
// It is keyed by session ID so a reused Engine cannot leak values into another
// provider request's session.
type codeModeStore struct {
	mu       sync.Mutex
	sessions map[string]map[string]json.RawMessage
	versions map[string]map[string]uint64
	clocks   map[string]uint64
}

func newCodeModeStore() *codeModeStore {
	return &codeModeStore{sessions: map[string]map[string]json.RawMessage{}, versions: map[string]map[string]uint64{}, clocks: map[string]uint64{}}
}

func (s *codeModeStore) State(sessionID string) json.RawMessage {
	raw, _ := s.Snapshot(sessionID)
	return raw
}

func (s *codeModeStore) Clear(sessionID string) {
	s.mu.Lock()
	delete(s.sessions, sessionID)
	delete(s.versions, sessionID)
	delete(s.clocks, sessionID)
	s.mu.Unlock()
}

func (s *codeModeStore) ClearAll() {
	s.mu.Lock()
	s.sessions = map[string]map[string]json.RawMessage{}
	s.versions = map[string]map[string]uint64{}
	s.clocks = map[string]uint64{}
	s.mu.Unlock()
}

func (s *codeModeStore) Snapshot(id string) (json.RawMessage, map[string]uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s.sessions[id])
	if string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	versions := map[string]uint64{}
	for k, v := range s.versions[id] {
		versions[k] = v
	}
	return raw, versions
}
func (s *codeModeStore) Apply(id string, raw json.RawMessage, snapshot map[string]uint64) (int, []string, error) {
	var delta codemode.StateDelta
	if err := json.Unmarshal(raw, &delta); err != nil || delta.Set == nil {
		return 0, nil, fmt.Errorf("invalid_state: expected write set")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := map[string]json.RawMessage{}
	for k, v := range s.sessions[id] {
		next[k] = v
	}
	versions := s.versions[id]
	if versions == nil {
		versions = map[string]uint64{}
	}
	keys := map[string]bool{}
	for k, v := range delta.Set {
		next[k] = append(json.RawMessage(nil), v...)
		keys[k] = true
	}
	for _, k := range delta.Delete {
		delete(next, k)
		keys[k] = true
	}
	merged, err := json.Marshal(next)
	if err != nil {
		return 0, nil, err
	}
	if len(merged) > codemode.SessionLimits.StateTotal {
		return 0, nil, fmt.Errorf("state_total_limit: merged state exceeds limit")
	}
	for _, v := range next {
		if len(v) > codemode.SessionLimits.StateValue || strings.Contains(string(v), "cell-image:") {
			return 0, nil, fmt.Errorf("state_value_limit: invalid merged value")
		}
	}
	var overwritten []string
	for k := range keys {
		if versions[k] != snapshot[k] {
			overwritten = append(overwritten, k)
		}
		s.clocks[id]++
		if _, exists := next[k]; exists {
			versions[k] = s.clocks[id]
		} else {
			delete(versions, k)
		}
	}
	sort.Strings(overwritten)
	s.sessions[id] = next
	s.versions[id] = versions
	return len(merged), overwritten, nil
}

package hub

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// delegationPromptLimit bounds the prompt excerpt stored in the ledger.
	delegationPromptLimit = 2000
	// delegationResponseLimit bounds the terminal response stored in the ledger.
	delegationResponseLimit = 64 * 1024
	// delegationRetention is how long terminal records are kept.
	delegationRetention = 7 * 24 * time.Hour
	// delegationMaxRecords caps the ledger size; oldest terminal records are
	// dropped first, active records are never pruned.
	delegationMaxRecords = 1000
)

// DelegationStore persists delegation records in a local JSON ledger. The
// file is written 0600 like the node store: the records hold no tokens, but
// prompts and responses are still private session content.
type DelegationStore struct {
	mu   sync.Mutex
	path string
	// now is a hook for prune tests.
	now func() time.Time
}

// NewDelegationStore returns a store backed by the given JSON file path. The
// file is created lazily on first Add.
func NewDelegationStore(path string) *DelegationStore {
	return &DelegationStore{path: path, now: time.Now}
}

// Path returns the backing file path.
func (s *DelegationStore) Path() string { return s.path }

// delegationFile is the on-disk JSON shape.
type delegationFile struct {
	Delegations []Delegation `json:"delegations"`
}

func (s *DelegationStore) readLocked() ([]Delegation, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read hub delegation ledger: %w", err)
	}
	var f delegationFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse hub delegation ledger %s: %w", s.path, err)
	}
	return f.Delegations, nil
}

func (s *DelegationStore) writeLocked(records []Delegation) error {
	records = s.pruneLocked(records)
	data, err := json.MarshalIndent(delegationFile{Delegations: records}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode hub delegation ledger: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create hub delegation ledger dir: %w", err)
	}
	// 0600: prompts/responses are private content. Chmod first so an existing
	// overly-permissive file is corrected as well as newly created files.
	if err := os.Chmod(s.path, 0o600); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("secure hub delegation ledger permissions: %w", err)
	}
	if err := os.WriteFile(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write hub delegation ledger: %w", err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("secure hub delegation ledger permissions: %w", err)
	}
	return nil
}

// pruneLocked drops terminal records older than the retention window, then
// enforces the record cap by dropping the oldest terminal records. Active
// records always survive pruning so in-flight caps stay accurate.
func (s *DelegationStore) pruneLocked(records []Delegation) []Delegation {
	cutoff := s.now().Add(-delegationRetention)
	kept := records[:0]
	for _, d := range records {
		if DelegationStatusTerminal(d.Status) && d.UpdatedAt.Before(cutoff) {
			continue
		}
		kept = append(kept, d)
	}
	if len(kept) <= delegationMaxRecords {
		return kept
	}
	// Over cap: drop oldest terminal records first.
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].CreatedAt.Before(kept[j].CreatedAt) })
	overflow := len(kept) - delegationMaxRecords
	trimmed := make([]Delegation, 0, delegationMaxRecords)
	for _, d := range kept {
		if overflow > 0 && DelegationStatusTerminal(d.Status) {
			overflow--
			continue
		}
		trimmed = append(trimmed, d)
	}
	return trimmed
}

func truncateForLedger(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "… [truncated]"
}

// Add persists a new delegation record. Prompt and response excerpts are
// truncated to keep the ledger bounded.
func (s *DelegationStore) Add(d Delegation) error {
	if d.ID == "" {
		return fmt.Errorf("delegation id must not be empty")
	}
	d.Prompt = truncateForLedger(d.Prompt, delegationPromptLimit)
	d.Response = truncateForLedger(d.Response, delegationResponseLimit)
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readLocked()
	if err != nil {
		return err
	}
	for _, existing := range records {
		if existing.ID == d.ID {
			return fmt.Errorf("delegation %q already exists", d.ID)
		}
	}
	records = append(records, d)
	return s.writeLocked(records)
}

// Get returns a delegation by id.
func (s *DelegationStore) Get(id string) (Delegation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readLocked()
	if err != nil {
		return Delegation{}, false, err
	}
	for _, d := range records {
		if d.ID == id {
			return d, true, nil
		}
	}
	return Delegation{}, false, nil
}

// Update applies fn to the stored record and persists the result. The
// record's UpdatedAt is stamped and the response excerpt re-truncated.
func (s *DelegationStore) Update(id string, fn func(*Delegation)) (Delegation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readLocked()
	if err != nil {
		return Delegation{}, err
	}
	for i := range records {
		if records[i].ID != id {
			continue
		}
		fn(&records[i])
		records[i].UpdatedAt = s.now().UTC()
		records[i].Response = truncateForLedger(records[i].Response, delegationResponseLimit)
		updated := records[i]
		if err := s.writeLocked(records); err != nil {
			return Delegation{}, err
		}
		return updated, nil
	}
	return Delegation{}, fmt.Errorf("delegation %q not found", id)
}

// List returns all records, newest first.
func (s *DelegationStore) List() ([]Delegation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readLocked()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].CreatedAt.After(records[j].CreatedAt) })
	return records, nil
}

// ActiveCounts tallies non-terminal delegations hub-wide, per origin node,
// and per target node, for in-flight cap enforcement.
func (s *DelegationStore) ActiveCounts() (total int, byOrigin, byTarget map[string]int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readLocked()
	if err != nil {
		return 0, nil, nil, err
	}
	byOrigin = map[string]int{}
	byTarget = map[string]int{}
	for _, d := range records {
		if DelegationStatusTerminal(d.Status) {
			continue
		}
		total++
		byOrigin[d.OriginNode]++
		byTarget[d.TargetNode]++
	}
	return total, byOrigin, byTarget, nil
}

package passkeyauth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/filelock"
)

const (
	SessionIdleLifetime          = 12 * time.Hour
	SessionAbsoluteLifetime      = 7 * 24 * time.Hour
	SessionActivityWriteInterval = 5 * time.Minute
	RecentAuthLifetime           = 5 * time.Minute
	MaxSessions                  = 1024
	sessionStoreVersion          = 1
)

var (
	ErrInvalidSession     = errors.New("invalid or expired session")
	ErrSessionCapacity    = errors.New("session capacity reached")
	ErrRecentAuthRequired = errors.New("recent authentication required")
)

type SessionInfo struct {
	ID                 string    `json:"-"`
	CredentialRecordID string    `json:"-"`
	CreatedAt          time.Time `json:"created_at"`
	LastSeenAt         time.Time `json:"last_seen_at"`
	IdleExpiresAt      time.Time `json:"idle_expires_at"`
	AbsoluteExpiresAt  time.Time `json:"absolute_expires_at"`
}

type IssuedSession struct {
	Token string
	Info  SessionInfo
}
type Principal struct{ SessionID, CredentialRecordID string }
type sessionRecord struct {
	hash              [32]byte
	info              SessionInfo
	persistedLastSeen time.Time
	recentUntil       time.Time
	recentAvailable   bool
}

type sessionStoreFile struct {
	Version  int                      `json:"version"`
	RPID     string                   `json:"rp_id"`
	UserID   string                   `json:"user_id"`
	Sessions []persistedSessionRecord `json:"sessions"`
}

type persistedSessionRecord struct {
	Hash               string    `json:"token_hash"`
	ID                 string    `json:"id"`
	CredentialRecordID string    `json:"credential_record_id"`
	CreatedAt          time.Time `json:"created_at"`
	LastSeenAt         time.Time `json:"last_seen_at"`
	IdleExpiresAt      time.Time `json:"idle_expires_at"`
	AbsoluteExpiresAt  time.Time `json:"absolute_expires_at"`
}

type SessionsOptions struct {
	Path            string
	RPID            string
	UserID          string
	Now             func() time.Time
	Random          io.Reader
	ValidCredential func(string) bool
	Warnf           func(string, ...any)
	WriteFile       func(string, []byte, os.FileMode) error
}

type Sessions struct {
	mu                   sync.Mutex
	now                  func() time.Time
	random               io.Reader
	byHash               map[[32]byte]*sessionRecord
	byID                 map[string]*sessionRecord
	max                  int
	path                 string
	rpID                 string
	userID               string
	validCredential      func(string) bool // called with mu held; must not call back into Sessions
	warnf                func(string, ...any)
	writeFile            func(string, []byte, os.FileMode) error
	unlockFile           func() error
	degraded             bool
	checkpointRetryAfter time.Time
	lastCheckpointAt     time.Time
}

// NewSessions returns an in-memory session registry. Hub runtimes should use
// OpenSessions so authenticated browser sessions survive process restarts.
func NewSessions(now func() time.Time, random io.Reader) *Sessions {
	return newSessions(SessionsOptions{Now: now, Random: random})
}

func newSessions(opts SessionsOptions) *Sessions {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	if opts.Warnf == nil {
		opts.Warnf = func(string, ...any) {}
	}
	if opts.WriteFile == nil {
		opts.WriteFile = config.WriteFileAtomicallyNoFollow
	}
	return &Sessions{
		now:             opts.Now,
		random:          opts.Random,
		byHash:          map[[32]byte]*sessionRecord{},
		byID:            map[string]*sessionRecord{},
		max:             MaxSessions,
		path:            opts.Path,
		rpID:            opts.RPID,
		userID:          opts.UserID,
		validCredential: opts.ValidCredential,
		warnf:           opts.Warnf,
		writeFile:       opts.WriteFile,
	}
}

// OpenSessions opens or creates a private durable session registry. Only token
// hashes are persisted; raw browser cookie tokens remain known only to clients.
func OpenSessions(opts SessionsOptions) (*Sessions, error) {
	if opts.Path == "" || opts.RPID == "" || opts.UserID == "" || opts.ValidCredential == nil {
		return nil, fmt.Errorf("session store path, RP ID, user ID, and credential validator are required")
	}
	s := newSessions(opts)
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, fmt.Errorf("create passkey session directory: %w", err)
	}
	if runtime.GOOS != "windows" {
		if d, err := os.Stat(filepath.Dir(s.path)); err != nil || d.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("passkey session directory must have private permissions (0700)")
		}
	}
	unlock, err := filelock.TryLock(s.path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("lock passkey session store %s (another Hub process may be using it): %w", s.path, err)
	}
	s.unlockFile = unlock
	if err := s.open(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Sessions) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unlockFile == nil {
		return nil
	}
	unlock := s.unlockFile
	s.unlockFile = nil
	if err := unlock(); err != nil {
		return fmt.Errorf("unlock passkey session store: %w", err)
	}
	return nil
}

func (s *Sessions) open() error {
	info, err := os.Lstat(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect passkey session store: %w", err)
		}
		return s.writeLocked()
	}
	if runtime.GOOS != "windows" {
		if parent, err := os.Stat(filepath.Dir(s.path)); err != nil || parent.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("passkey session directory must have private permissions (0700)")
		}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("passkey session store must be a regular non-symlink file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("passkey session store %s has insecure permissions %04o; require 0600", s.path, info.Mode().Perm())
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read passkey session store: %w", err)
	}
	var persisted sessionStoreFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&persisted); err != nil {
		return s.resetCorruptStore(fmt.Errorf("parse passkey session store: %w", err))
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return s.resetCorruptStore(fmt.Errorf("parse passkey session store: trailing JSON data"))
	}
	if persisted.Version != sessionStoreVersion {
		return s.resetCorruptStore(fmt.Errorf("unsupported passkey session store version %d", persisted.Version))
	}
	if persisted.RPID != s.rpID || persisted.UserID != s.userID {
		return fmt.Errorf("passkey session store %s belongs to a different Hub identity; remove it to reset browser sessions", s.path)
	}
	if len(persisted.Sessions) > s.max {
		return s.resetCorruptStore(fmt.Errorf("passkey session store exceeds session capacity"))
	}
	now := s.now().UTC()
	changed := false
	for i, saved := range persisted.Sessions {
		r, err := decodePersistedSession(saved)
		if err != nil {
			s.warnf("dropping invalid passkey session at index %d: %v", i, err)
			changed = true
			continue
		}
		if _, exists := s.byHash[r.hash]; exists {
			s.warnf("dropping passkey session with duplicate token hash at index %d", i)
			changed = true
			continue
		}
		if _, exists := s.byID[r.info.ID]; exists {
			s.warnf("dropping passkey session with duplicate ID at index %d", i)
			changed = true
			continue
		}
		if !now.Before(r.info.IdleExpiresAt) || !now.Before(r.info.AbsoluteExpiresAt) || (s.validCredential != nil && !s.validCredential(r.info.CredentialRecordID)) {
			changed = true
			continue
		}
		s.byHash[r.hash], s.byID[r.info.ID] = r, r
	}
	if changed {
		if err := s.writeLocked(); err != nil {
			return fmt.Errorf("prune passkey session store: %w", err)
		}
	}
	return nil
}

func (s *Sessions) resetCorruptStore(cause error) error {
	quarantine := fmt.Sprintf("%s.corrupt-%d-%d", s.path, os.Getpid(), time.Now().UnixNano())
	if err := os.Rename(s.path, quarantine); err != nil {
		return fmt.Errorf("quarantine corrupt passkey session store: %w", err)
	}
	s.warnf("reset corrupt passkey session store %s; all browser sessions must reauthenticate: %v", quarantine, cause)
	if err := s.writeLocked(); err != nil {
		return fmt.Errorf("replace corrupt passkey session store: %w", err)
	}
	return nil
}

func decodePersistedSession(saved persistedSessionRecord) (*sessionRecord, error) {
	hash, err := base64.RawURLEncoding.DecodeString(saved.Hash)
	if err != nil || len(hash) != sha256.Size {
		return nil, fmt.Errorf("invalid token hash")
	}
	id, err := base64.RawURLEncoding.DecodeString(saved.ID)
	if err != nil || len(id) != 16 {
		return nil, fmt.Errorf("invalid session ID")
	}
	if saved.CredentialRecordID == "" {
		return nil, fmt.Errorf("credential record ID is required")
	}
	if saved.CreatedAt.IsZero() || saved.LastSeenAt.Before(saved.CreatedAt) || !saved.AbsoluteExpiresAt.Equal(saved.CreatedAt.Add(SessionAbsoluteLifetime)) {
		return nil, fmt.Errorf("invalid session timestamps")
	}
	wantIdle := minTime(saved.LastSeenAt.Add(SessionIdleLifetime), saved.AbsoluteExpiresAt)
	if !saved.IdleExpiresAt.Equal(wantIdle) {
		return nil, fmt.Errorf("invalid session idle expiry")
	}
	var h [sha256.Size]byte
	copy(h[:], hash)
	info := SessionInfo{
		ID:                 saved.ID,
		CredentialRecordID: saved.CredentialRecordID,
		CreatedAt:          saved.CreatedAt.UTC(),
		LastSeenAt:         saved.LastSeenAt.UTC(),
		IdleExpiresAt:      saved.IdleExpiresAt.UTC(),
		AbsoluteExpiresAt:  saved.AbsoluteExpiresAt.UTC(),
	}
	return &sessionRecord{hash: h, info: info, persistedLastSeen: info.LastSeenAt}, nil
}

func (s *Sessions) Create(credentialRecordID string) (IssuedSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.pruneLocked(now)
	if credentialRecordID == "" || (s.validCredential != nil && !s.validCredential(credentialRecordID)) {
		return IssuedSession{}, fmt.Errorf("create session for unknown credential")
	}
	if len(s.byHash) >= s.max {
		return IssuedSession{}, ErrSessionCapacity
	}
	token, err := randomToken(s.random, 32)
	if err != nil {
		return IssuedSession{}, fmt.Errorf("generate session token: %w", err)
	}
	id, err := randomToken(s.random, 16)
	if err != nil {
		return IssuedSession{}, fmt.Errorf("generate session ID: %w", err)
	}
	h := sha256.Sum256([]byte(token))
	if s.byHash[h] != nil || s.byID[id] != nil {
		return IssuedSession{}, fmt.Errorf("generated duplicate session identifier")
	}
	info := SessionInfo{ID: id, CredentialRecordID: credentialRecordID, CreatedAt: now, LastSeenAt: now, IdleExpiresAt: now.Add(SessionIdleLifetime), AbsoluteExpiresAt: now.Add(SessionAbsoluteLifetime)}
	r := &sessionRecord{hash: h, info: info, persistedLastSeen: now}
	s.byHash[h], s.byID[id] = r, r
	if err := s.writeLocked(); err != nil {
		s.deleteLocked(r)
		return IssuedSession{}, fmt.Errorf("persist session: %w", err)
	}
	return IssuedSession{Token: token, Info: info}, nil
}

func (s *Sessions) Authenticate(token string) (Principal, error) {
	if token == "" {
		return Principal{}, ErrInvalidSession
	}
	h := sha256.Sum256([]byte(token))
	s.mu.Lock()
	now := s.now().UTC()
	var checkpointErr error
	if s.degraded {
		if now.Before(s.checkpointRetryAfter) {
			s.mu.Unlock()
			return Principal{}, ErrInvalidSession
		}
		if err := s.writeLocked(); err != nil {
			s.checkpointRetryAfter = now.Add(time.Minute)
			checkpointErr = err
			s.mu.Unlock()
			s.warnf("could not recover degraded passkey session store: %v", checkpointErr)
			return Principal{}, ErrInvalidSession
		}
	}
	s.pruneLocked(now)
	r := s.byHash[h]
	if r == nil || (s.validCredential != nil && !s.validCredential(r.info.CredentialRecordID)) {
		s.mu.Unlock()
		return Principal{}, ErrInvalidSession
	}
	activityAt := now
	if activityAt.Before(r.info.LastSeenAt) {
		activityAt = r.info.LastSeenAt
	}
	r.info.LastSeenAt = activityAt
	r.info.IdleExpiresAt = minTime(activityAt.Add(SessionIdleLifetime), r.info.AbsoluteExpiresAt)
	if s.path != "" && activityAt.Sub(r.persistedLastSeen) >= SessionActivityWriteInterval && activityAt.Sub(s.lastCheckpointAt) >= SessionActivityWriteInterval && !now.Before(s.checkpointRetryAfter) {
		if err := s.writeLocked(); err != nil {
			s.checkpointRetryAfter = now.Add(time.Minute)
			checkpointErr = err
		}
	}
	principal := Principal{SessionID: r.info.ID, CredentialRecordID: r.info.CredentialRecordID}
	s.mu.Unlock()
	if checkpointErr != nil {
		s.warnf("could not checkpoint passkey session activity: %v", checkpointErr)
	}
	return principal, nil
}

func (s *Sessions) Info(p Principal) (SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now().UTC())
	r := s.recordForPrincipalLocked(p)
	if r == nil {
		return SessionInfo{}, ErrInvalidSession
	}
	return r.info, nil
}
func (s *Sessions) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now().UTC())
	return len(s.byHash)
}
func (s *Sessions) Logout(p Principal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recordForPrincipalLocked(p)
	if r == nil {
		return ErrInvalidSession
	}
	s.deleteLocked(r)
	if err := s.writeLocked(); err != nil {
		return fmt.Errorf("persist session logout: %w", s.failSecurityWriteLocked(err))
	}
	return nil
}
func (s *Sessions) RevokeOthers(p Principal) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.recordForPrincipalLocked(p)
	if keep == nil {
		return 0, ErrInvalidSession
	}
	var removed []*sessionRecord
	for _, r := range s.byID {
		if r != keep {
			removed = append(removed, r)
			s.deleteLocked(r)
		}
	}
	if err := s.writeLocked(); err != nil {
		return 0, fmt.Errorf("persist session revocation: %w", s.failSecurityWriteLocked(err))
	}
	return len(removed), nil
}
func (s *Sessions) RevokeCredential(recordID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []*sessionRecord
	for _, r := range s.byID {
		if r.info.CredentialRecordID == recordID {
			removed = append(removed, r)
			s.deleteLocked(r)
		}
	}
	if err := s.writeLocked(); err != nil {
		return 0, fmt.Errorf("persist credential session revocation: %w", s.failSecurityWriteLocked(err))
	}
	return len(removed), nil
}
func (s *Sessions) GrantRecentAuth(p Principal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recordForPrincipalLocked(p)
	if r == nil {
		return ErrInvalidSession
	}
	r.recentUntil = s.now().UTC().Add(RecentAuthLifetime)
	r.recentAvailable = true
	return nil
}
func (s *Sessions) HasRecentAuth(p Principal) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recordForPrincipalLocked(p)
	return r != nil && r.recentAvailable && s.now().UTC().Before(r.recentUntil)
}
func (s *Sessions) ConsumeRecentAuth(p Principal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.recordForPrincipalLocked(p)
	if r == nil {
		return ErrInvalidSession
	}
	if !r.recentAvailable || !s.now().UTC().Before(r.recentUntil) {
		r.recentAvailable = false
		return ErrRecentAuthRequired
	}
	r.recentAvailable = false
	return nil
}
func (s *Sessions) recordForPrincipalLocked(p Principal) *sessionRecord {
	r := s.byID[p.SessionID]
	if r == nil || r.info.CredentialRecordID != p.CredentialRecordID {
		return nil
	}
	return r
}
func (s *Sessions) deleteLocked(r *sessionRecord) {
	delete(s.byHash, r.hash)
	delete(s.byID, r.info.ID)
}
func (s *Sessions) failSecurityWriteLocked(cause error) error {
	// If the write failed before replacement, removing the stale file prevents a
	// revoked session from returning after restart. If replacement succeeded but
	// directory sync failed, removal safely invalidates all durable sessions.
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		s.degraded = true
		s.checkpointRetryAfter = s.now().UTC().Add(time.Minute)
		return fmt.Errorf("%w; could not remove stale session store: %v", cause, err)
	}
	// The stale durable state is gone, so unaffected in-memory sessions remain
	// safe to use. Force the next authenticated request to recreate the store.
	s.degraded = false
	s.checkpointRetryAfter = time.Time{}
	s.lastCheckpointAt = time.Time{}
	for _, r := range s.byID {
		r.persistedLastSeen = time.Time{}
	}
	return cause
}
func (s *Sessions) pruneLocked(now time.Time) {
	for _, r := range s.byID {
		if !now.Before(r.info.IdleExpiresAt) || !now.Before(r.info.AbsoluteExpiresAt) {
			s.deleteLocked(r)
		}
	}
}
func (s *Sessions) writeLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create passkey session directory: %w", err)
	}
	if runtime.GOOS != "windows" {
		if d, err := os.Stat(filepath.Dir(s.path)); err != nil || d.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("passkey session directory must have private permissions (0700)")
		}
	}
	if info, err := os.Lstat(s.path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return fmt.Errorf("refusing to write passkey session store through a symlink")
	}
	ids := make([]string, 0, len(s.byID))
	for id := range s.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	persisted := sessionStoreFile{Version: sessionStoreVersion, RPID: s.rpID, UserID: s.userID, Sessions: make([]persistedSessionRecord, 0, len(ids))}
	for _, id := range ids {
		r := s.byID[id]
		persisted.Sessions = append(persisted.Sessions, persistedSessionRecord{
			Hash:               base64.RawURLEncoding.EncodeToString(r.hash[:]),
			ID:                 r.info.ID,
			CredentialRecordID: r.info.CredentialRecordID,
			CreatedAt:          r.info.CreatedAt,
			LastSeenAt:         r.info.LastSeenAt,
			IdleExpiresAt:      r.info.IdleExpiresAt,
			AbsoluteExpiresAt:  r.info.AbsoluteExpiresAt,
		})
	}
	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return fmt.Errorf("encode passkey session store: %w", err)
	}
	if err := s.writeFile(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write passkey session store: %w", err)
	}
	for _, r := range s.byID {
		r.persistedLastSeen = r.info.LastSeenAt
	}
	s.degraded = false
	s.checkpointRetryAfter = time.Time{}
	s.lastCheckpointAt = s.now().UTC()
	return nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

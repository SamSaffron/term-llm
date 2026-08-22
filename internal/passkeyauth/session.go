package passkeyauth

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	SessionIdleLifetime     = 12 * time.Hour
	SessionAbsoluteLifetime = 7 * 24 * time.Hour
	RecentAuthLifetime      = 5 * time.Minute
	MaxSessions             = 1024
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
	hash            [32]byte
	info            SessionInfo
	recentUntil     time.Time
	recentAvailable bool
}

type Sessions struct {
	mu     sync.Mutex
	now    func() time.Time
	random io.Reader
	byHash map[[32]byte]*sessionRecord
	byID   map[string]*sessionRecord
	max    int
}

func NewSessions(now func() time.Time, random io.Reader) *Sessions {
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = rand.Reader
	}
	return &Sessions{now: now, random: random, byHash: map[[32]byte]*sessionRecord{}, byID: map[string]*sessionRecord{}, max: MaxSessions}
}

func (s *Sessions) Create(credentialRecordID string) (IssuedSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.pruneLocked(now)
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
	info := SessionInfo{ID: id, CredentialRecordID: credentialRecordID, CreatedAt: now, LastSeenAt: now, IdleExpiresAt: now.Add(SessionIdleLifetime), AbsoluteExpiresAt: now.Add(SessionAbsoluteLifetime)}
	r := &sessionRecord{hash: h, info: info}
	s.byHash[h], s.byID[id] = r, r
	return IssuedSession{Token: token, Info: info}, nil
}

func (s *Sessions) Authenticate(token string) (Principal, error) {
	if token == "" {
		return Principal{}, ErrInvalidSession
	}
	h := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.pruneLocked(now)
	r := s.byHash[h]
	if r == nil {
		return Principal{}, ErrInvalidSession
	}
	r.info.LastSeenAt = now
	r.info.IdleExpiresAt = minTime(now.Add(SessionIdleLifetime), r.info.AbsoluteExpiresAt)
	return Principal{SessionID: r.info.ID, CredentialRecordID: r.info.CredentialRecordID}, nil
}

func (s *Sessions) Info(p Principal) (SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.pruneLocked(now)
	r := s.byID[p.SessionID]
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
	r := s.byID[p.SessionID]
	if r == nil {
		return ErrInvalidSession
	}
	s.deleteLocked(r)
	return nil
}
func (s *Sessions) RevokeOthers(p Principal) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.byID[p.SessionID]
	if keep == nil {
		return 0, ErrInvalidSession
	}
	n := 0
	for _, r := range s.byID {
		if r != keep {
			s.deleteLocked(r)
			n++
		}
	}
	return n, nil
}
func (s *Sessions) RevokeCredential(recordID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.byID {
		if r.info.CredentialRecordID == recordID {
			s.deleteLocked(r)
			n++
		}
	}
	return n
}
func (s *Sessions) GrantRecentAuth(p Principal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byID[p.SessionID]
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
	r := s.byID[p.SessionID]
	return r != nil && r.recentAvailable && s.now().UTC().Before(r.recentUntil)
}
func (s *Sessions) ConsumeRecentAuth(p Principal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byID[p.SessionID]
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
func (s *Sessions) deleteLocked(r *sessionRecord) {
	delete(s.byHash, r.hash)
	delete(s.byID, r.info.ID)
}
func (s *Sessions) pruneLocked(now time.Time) {
	for _, r := range s.byID {
		if !now.Before(r.info.IdleExpiresAt) || !now.Before(r.info.AbsoluteExpiresAt) {
			s.deleteLocked(r)
		}
	}
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

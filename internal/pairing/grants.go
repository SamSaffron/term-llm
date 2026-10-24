package pairing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ScopeHubChat is the only grant scope in pilot v1: peer DataChannel access
// to the fixed Hub chat target. It never confers Hub security settings,
// node-registration credentials, master tokens, or other sensitive routes;
// ingress enforces the route allowlist. Scope is host-local and not part of
// any wire transcript.
const ScopeHubChat = "hub-chat"

// Grant authorizes a paired device key for peer sessions with this host. It
// always expires (default 24h). In pilot v1 reconnect discovery is limited to
// the original broker mailbox (MailboxID) while it is live (at most 30
// minutes); after MailboxExpiresAt the grant stays valid for Authorize, but
// Host.NewChallenge fails with ErrReconnectUnavailable and the device must
// pair again.
type Grant struct {
	ID         string    `json:"id"` // base64url of 16 random bytes
	Scope      string    `json:"scope"`
	DeviceKey  B64       `json:"device_key"`
	DeviceName string    `json:"device_name"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"` // required; there are no non-expiring grants
	// MailboxID is the broker session ID used for pairing; MailboxExpiresAt is
	// when that broker session ends.
	MailboxID        string     `json:"mailbox_id"`
	MailboxExpiresAt time.Time  `json:"mailbox_expires_at"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
}

// DeviceFingerprint returns the human-comparable device key fingerprint.
func (g Grant) DeviceFingerprint() string { return KeyFingerprint(g.DeviceKey) }

// Check reports ErrRevoked, ErrExpired or ErrInvalid (unknown scope) when the
// grant is no longer usable.
func (g Grant) Check(now time.Time) error {
	if g.Scope != ScopeHubChat {
		return fmt.Errorf("%w: grant scope %q", ErrInvalid, g.Scope)
	}
	if g.RevokedAt != nil {
		return ErrRevoked
	}
	if g.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: grant has no expiry", ErrInvalid)
	}
	if !now.Before(g.ExpiresAt) {
		return fmt.Errorf("grant: %w", ErrExpired)
	}
	return nil
}

type grantFile struct {
	V      int     `json:"v"`
	Grants []Grant `json:"grants"`
}

// GrantStore persists device grants in a single 0600 JSON file, guarded by an
// in-process mutex and a cross-process file lock; writes are atomic renames.
type GrantStore struct {
	path string
	mu   sync.Mutex
}

// OpenGrantStore opens (without creating) the grant file at path and verifies
// that an existing file is private and decodable.
func OpenGrantStore(path string) (*GrantStore, error) {
	s := &GrantStore{path: path}
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if _, err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path returns the backing file path.
func (s *GrantStore) Path() string { return s.path }

func (s *GrantStore) load() (grantFile, error) {
	data, err := readPrivateFile(s.path)
	if isNotExist(err) {
		return grantFile{V: Version}, nil
	}
	if err != nil {
		return grantFile{}, fmt.Errorf("load grants: %w", err)
	}
	var f grantFile
	if err := json.Unmarshal(data, &f); err != nil {
		return grantFile{}, fmt.Errorf("decode grants %s: %w", s.path, err)
	}
	if f.V != Version {
		return grantFile{}, fmt.Errorf("grants %s: unsupported version %d", s.path, f.V)
	}
	return f, nil
}

func (s *GrantStore) update(fn func(*grantFile) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return withFileLock(s.path, func() error {
		f, err := s.load()
		if err != nil {
			return err
		}
		if err := fn(&f); err != nil {
			return err
		}
		data, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			return err
		}
		return writePrivateFile(s.path, data)
	})
}

func (s *GrantStore) read() (grantFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var f grantFile
	err := withFileLock(s.path, func() error {
		var err error
		f, err = s.load()
		return err
	})
	return f, err
}

// List returns all grants (including revoked/expired) sorted by creation time.
func (s *GrantStore) List() ([]Grant, error) {
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(f.Grants, func(i, j int) bool { return f.Grants[i].CreatedAt.Before(f.Grants[j].CreatedAt) })
	return f.Grants, nil
}

// Get returns the grant with id or ErrGrantNotFound.
func (s *GrantStore) Get(id string) (Grant, error) {
	f, err := s.read()
	if err != nil {
		return Grant{}, err
	}
	for _, g := range f.Grants {
		if subtle.ConstantTimeCompare([]byte(g.ID), []byte(id)) == 1 {
			return g, nil
		}
	}
	return Grant{}, ErrGrantNotFound
}

// Add persists a new grant; the ID must be unique, the scope ScopeHubChat and
// the expiry set.
func (s *GrantStore) Add(g Grant) error {
	if g.Scope != ScopeHubChat || g.ExpiresAt.IsZero() || g.ID == "" || len(g.DeviceKey) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: grant must have an id, device key, scope %q and an expiry", ErrInvalid, ScopeHubChat)
	}
	return s.update(func(f *grantFile) error {
		for _, existing := range f.Grants {
			if existing.ID == g.ID {
				return fmt.Errorf("%w: duplicate grant id", ErrInvalid)
			}
		}
		f.Grants = append(f.Grants, g)
		return nil
	})
}

// Revoke marks the grant revoked; revoking twice is not an error.
func (s *GrantStore) Revoke(id string, now time.Time) error {
	return s.update(func(f *grantFile) error {
		for i := range f.Grants {
			if f.Grants[i].ID == id {
				if f.Grants[i].RevokedAt == nil {
					t := now.UTC()
					f.Grants[i].RevokedAt = &t
				}
				return nil
			}
		}
		return ErrGrantNotFound
	})
}

// Remove deletes the grant entirely.
func (s *GrantStore) Remove(id string) error {
	return s.update(func(f *grantFile) error {
		for i := range f.Grants {
			if f.Grants[i].ID == id {
				f.Grants = append(f.Grants[:i], f.Grants[i+1:]...)
				return nil
			}
		}
		return ErrGrantNotFound
	})
}

// Authorize reports whether grant id is currently usable by deviceKey
// (exists, key matches, scope is ScopeHubChat, not revoked or expired). It
// reads the file on every call, so a revocation by another process takes
// effect immediately; call it per peer request for a live check.
func (s *GrantStore) Authorize(id string, deviceKey ed25519.PublicKey, now time.Time) (Grant, error) {
	return s.authorize(id, deviceKey, now, false)
}

// authorize checks that id names a usable grant for deviceKey. When touch is
// set it also records LastUsedAt.
func (s *GrantStore) authorize(id string, deviceKey []byte, now time.Time, touch bool) (Grant, error) {
	var out Grant
	check := func(f *grantFile) error {
		for i := range f.Grants {
			g := &f.Grants[i]
			if subtle.ConstantTimeCompare([]byte(g.ID), []byte(id)) != 1 {
				continue
			}
			if !bytes.Equal(g.DeviceKey, deviceKey) {
				return ErrGrantNotFound
			}
			if err := g.Check(now); err != nil {
				return err
			}
			if touch {
				t := now.UTC()
				g.LastUsedAt = &t
			}
			out = *g
			return nil
		}
		return ErrGrantNotFound
	}
	if touch {
		if err := s.update(check); err != nil {
			return Grant{}, err
		}
		return out, nil
	}
	f, err := s.read()
	if err != nil {
		return Grant{}, err
	}
	if err := check(&f); err != nil {
		return Grant{}, err
	}
	return out, nil
}

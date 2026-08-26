package passkeyauth

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/samsaffron/term-llm/internal/config"
)

var (
	ErrFirstCredentialExists = errors.New("first credential already exists")
	ErrDuplicateCredential   = errors.New("credential is already enrolled")
	ErrCredentialNotFound    = errors.New("credential not found")
	ErrFinalCredential       = errors.New("cannot remove the final credential")
)

type storeFile struct {
	Version int    `json:"version"`
	RPID    string `json:"rp_id"`
	User    User   `json:"user"`
}

type StoreOptions struct {
	Path      string
	RPID      string
	UserName  string
	Now       func() time.Time
	Random    io.Reader
	Warnf     func(string, ...any)
	WriteFile func(string, []byte, os.FileMode) error
}

type Store struct {
	mu                   sync.Mutex
	path, rpID, userName string
	now                  func() time.Time
	random               io.Reader
	warnf                func(string, ...any)
	writeFile            func(string, []byte, os.FileMode) error
	data                 storeFile
}

func OpenStore(opts StoreOptions) (*Store, error) {
	if opts.Path == "" || opts.RPID == "" {
		return nil, fmt.Errorf("auth store path and RP ID are required")
	}
	if strings.TrimSpace(opts.UserName) == "" {
		opts.UserName = DefaultUserName
	}
	userName, err := normalizeUserName(opts.UserName)
	if err != nil {
		return nil, err
	}
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
	opts.UserName = userName
	s := &Store{path: opts.Path, rpID: opts.RPID, userName: opts.UserName, now: opts.Now, random: opts.Random, warnf: opts.Warnf, writeFile: opts.WriteFile}
	if err := s.open(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) open() error {
	info, err := os.Lstat(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect passkey auth store: %w", err)
		}
		id, err := randomToken(s.random, 32)
		if err != nil {
			return fmt.Errorf("generate passkey user handle: %w", err)
		}
		s.data = storeFile{Version: StoreVersion, RPID: s.rpID, User: User{ID: id, Name: s.userName, Credentials: []Credential{}}}
		return s.writeLocked()
	}
	if runtime.GOOS != "windows" {
		if parent, err := os.Stat(filepath.Dir(s.path)); err != nil || parent.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("passkey auth directory must have private permissions (0700)")
		}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("passkey auth store must be a regular non-symlink file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("passkey auth store %s has insecure permissions %04o; require 0600", s.path, info.Mode().Perm())
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read passkey auth store: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s.data); err != nil {
		return fmt.Errorf("parse passkey auth store: %w", err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("parse passkey auth store: trailing JSON data")
	}
	return s.validateLocked()
}

func (s *Store) validateLocked() error {
	if s.data.Version != StoreVersion {
		return fmt.Errorf("unsupported passkey auth store version %d", s.data.Version)
	}
	if s.data.RPID != s.rpID {
		return fmt.Errorf("passkey auth store RP ID %q does not match configured RP ID %q", s.data.RPID, s.rpID)
	}
	h, err := base64.RawURLEncoding.DecodeString(s.data.User.ID)
	if err != nil || len(h) != 32 {
		return fmt.Errorf("invalid passkey user handle")
	}
	if _, err := normalizeUserName(s.data.User.Name); err != nil {
		return fmt.Errorf("invalid persisted passkey user: %w", err)
	}
	records, ids := map[string]bool{}, map[string]bool{}
	for i, c := range s.data.User.Credentials {
		if c.RecordID == "" || records[c.RecordID] {
			return fmt.Errorf("invalid or duplicate credential record ID at index %d", i)
		}
		if err := validateProtocolCredential(c.WebAuthn); err != nil {
			return fmt.Errorf("corrupt WebAuthn credential at index %d: %w", i, err)
		}
		id := base64.RawURLEncoding.EncodeToString(c.WebAuthn.ID)
		if ids[id] {
			return fmt.Errorf("duplicate WebAuthn credential at index %d", i)
		}
		if _, err := NormalizeCredentialName(c.DisplayName); err != nil {
			return fmt.Errorf("credential %d: %w", i, err)
		}
		if c.CreatedAt.IsZero() || c.LastUsedAt.IsZero() || c.LastUsedAt.Before(c.CreatedAt) {
			return fmt.Errorf("credential %d has invalid timestamps", i)
		}
		records[c.RecordID], ids[id] = true, true
	}
	return nil
}

func validateProtocolCredential(credential webauthn.Credential) error {
	if len(credential.ID) == 0 || len(credential.ID) > 1023 {
		return fmt.Errorf("credential ID length is invalid")
	}
	if len(credential.PublicKey) == 0 {
		return fmt.Errorf("credential public key is missing")
	}
	if _, err := webauthncose.ParsePublicKey(credential.PublicKey); err != nil {
		return fmt.Errorf("credential public key is invalid: %w", err)
	}
	if !credential.Flags.UserPresent || !credential.Flags.UserVerified {
		return fmt.Errorf("credential is missing required user-presence or verification metadata")
	}
	if credential.Flags.BackupState && !credential.Flags.BackupEligible {
		return fmt.Errorf("credential backup state is inconsistent")
	}
	if len(credential.Authenticator.AAGUID) != 0 && len(credential.Authenticator.AAGUID) != 16 {
		return fmt.Errorf("credential AAGUID length is invalid")
	}
	if len(credential.Attestation.ClientDataHash) != 0 && len(credential.Attestation.ClientDataHash) != 32 {
		return fmt.Errorf("credential client-data hash length is invalid")
	}
	if credential.AttestationFormat != "" && !protocol.IsAttestationFormatString(credential.AttestationFormat) {
		return fmt.Errorf("credential attestation format is invalid")
	}
	switch credential.AttestationType {
	case "", "basic_full", "basic_surrogate", "attca", "anonca", "ecdaa", "none":
	default:
		return fmt.Errorf("credential attestation type is invalid")
	}
	switch credential.Authenticator.Attachment {
	case "", protocol.Platform, protocol.CrossPlatform:
	default:
		return fmt.Errorf("credential attachment is invalid")
	}
	seen := map[protocol.AuthenticatorTransport]bool{}
	for _, transport := range credential.Transport {
		switch transport {
		case protocol.USB, protocol.NFC, protocol.BLE, protocol.SmartCard, protocol.Hybrid, protocol.Internal:
		default:
			return fmt.Errorf("credential transport %q is invalid", transport)
		}
		if seen[transport] {
			return fmt.Errorf("credential transport %q is duplicated", transport)
		}
		seen[transport] = true
	}
	return nil
}

func (s *Store) writeLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create passkey auth directory: %w", err)
	}
	if runtime.GOOS != "windows" {
		if d, err := os.Stat(filepath.Dir(s.path)); err != nil || d.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("passkey auth directory must have private permissions (0700)")
		}
	}
	if info, err := os.Lstat(s.path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return fmt.Errorf("refusing to write passkey auth store through a symlink")
	}
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode passkey auth store: %w", err)
	}
	if err := s.writeFile(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write passkey auth store: %w", err)
	}
	return nil
}

func (s *Store) Path() string { return s.path }
func (s *Store) User() User {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneUser(s.data.User)
}
func (s *Store) CredentialCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data.User.Credentials)
}
func (s *Store) HasCredential(recordID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, credential := range s.data.User.Credentials {
		if credential.RecordID == recordID {
			return true
		}
	}
	return false
}
func (s *Store) Credentials() []Credential { return s.User().Credentials }

func (s *Store) newCredential(raw webauthn.Credential, name string) (Credential, error) {
	name, err := NormalizeCredentialName(name)
	if err != nil {
		return Credential{}, err
	}
	recordID, err := randomToken(s.random, 16)
	if err != nil {
		return Credential{}, fmt.Errorf("generate credential record ID: %w", err)
	}
	now := s.now().UTC()
	return Credential{RecordID: recordID, DisplayName: name, WebAuthn: cloneProtocolCredential(raw), CreatedAt: now, LastUsedAt: now}, nil
}

func (s *Store) CommitFirstCredential(raw webauthn.Credential, name string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.User.Credentials) != 0 {
		return Credential{}, ErrFirstCredentialExists
	}
	return s.addLocked(raw, name)
}
func (s *Store) AddCredential(raw webauthn.Credential, name string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addLocked(raw, name)
}
func (s *Store) addLocked(raw webauthn.Credential, name string) (Credential, error) {
	if err := validateProtocolCredential(raw); err != nil {
		return Credential{}, fmt.Errorf("invalid WebAuthn credential: %w", err)
	}
	for _, c := range s.data.User.Credentials {
		if bytes.Equal(c.WebAuthn.ID, raw.ID) {
			return Credential{}, ErrDuplicateCredential
		}
	}
	var c Credential
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		c, err = s.newCredential(raw, name)
		if err != nil {
			return Credential{}, err
		}
		unique := true
		for _, existing := range s.data.User.Credentials {
			if existing.RecordID == c.RecordID {
				unique = false
				break
			}
		}
		if unique {
			break
		}
		c = Credential{}
	}
	if c.RecordID == "" {
		return Credential{}, fmt.Errorf("generate unique credential record ID")
	}
	s.data.User.Credentials = append(s.data.User.Credentials, c)
	if err := s.writeLocked(); err != nil {
		s.data.User.Credentials = s.data.User.Credentials[:len(s.data.User.Credentials)-1]
		return Credential{}, err
	}
	return cloneCredential(c), nil
}

func (s *Store) RenameCredential(recordID, name string) (Credential, error) {
	name, err := NormalizeCredentialName(name)
	if err != nil {
		return Credential{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.User.Credentials {
		if s.data.User.Credentials[i].RecordID == recordID {
			old := s.data.User.Credentials[i].DisplayName
			s.data.User.Credentials[i].DisplayName = name
			if err := s.writeLocked(); err != nil {
				s.data.User.Credentials[i].DisplayName = old
				return Credential{}, err
			}
			return cloneCredential(s.data.User.Credentials[i]), nil
		}
	}
	return Credential{}, ErrCredentialNotFound
}

func (s *Store) DeleteCredential(recordID string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.User.Credentials) <= 1 {
		return Credential{}, ErrFinalCredential
	}
	for i, c := range s.data.User.Credentials {
		if c.RecordID == recordID {
			s.data.User.Credentials = append(s.data.User.Credentials[:i], s.data.User.Credentials[i+1:]...)
			if err := s.writeLocked(); err != nil {
				s.data.User.Credentials = append(s.data.User.Credentials, Credential{})
				copy(s.data.User.Credentials[i+1:], s.data.User.Credentials[i:])
				s.data.User.Credentials[i] = c
				return Credential{}, err
			}
			return cloneCredential(c), nil
		}
	}
	return Credential{}, ErrCredentialNotFound
}

func (s *Store) UpdateAfterAssertion(validated webauthn.Credential, reportedSignCount ...uint32) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.User.Credentials {
		c := &s.data.User.Credentials[i]
		if !bytes.Equal(c.WebAuthn.ID, validated.ID) {
			continue
		}
		old := cloneCredential(*c)
		if c.WebAuthn.Flags.BackupEligible != validated.Flags.BackupEligible {
			return Credential{}, fmt.Errorf("credential backup eligibility changed")
		}
		if err := validateProtocolCredential(validated); err != nil {
			return Credential{}, fmt.Errorf("validated credential metadata is invalid: %w", err)
		}
		oldCount, newCount := c.WebAuthn.Authenticator.SignCount, validated.Authenticator.SignCount
		if len(reportedSignCount) != 0 {
			newCount = reportedSignCount[0]
		}
		if oldCount > 0 && newCount == 0 {
			validated.Authenticator.SignCount = oldCount
			validated.Authenticator.CloneWarning = c.WebAuthn.Authenticator.CloneWarning
		} else if oldCount > 0 && newCount > 0 && newCount <= oldCount {
			s.warnf("WebAuthn signature counter did not increase for an enrolled credential")
			validated.Authenticator.SignCount = oldCount
			validated.Authenticator.CloneWarning = true
		}
		c.WebAuthn = cloneProtocolCredential(validated)
		c.LastUsedAt = s.now().UTC()
		if err := s.writeLocked(); err != nil {
			*c = old
			return Credential{}, err
		}
		return cloneCredential(*c), nil
	}
	return Credential{}, ErrCredentialNotFound
}

func cloneUser(a User) User {
	out := a
	out.Credentials = make([]Credential, len(a.Credentials))
	for i := range a.Credentials {
		out.Credentials[i] = cloneCredential(a.Credentials[i])
	}
	return out
}
func randomToken(r io.Reader, n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

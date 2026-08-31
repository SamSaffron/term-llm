package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/filelock"
	"golang.org/x/oauth2"
)

const storeSchemaVersion = 1

// OAuth2Config is the serializable subset of oauth2.Config required to reuse a
// registration and refresh a token after process restart.
type OAuth2Config struct {
	ClientID     string          `json:"client_id"`
	ClientSecret string          `json:"client_secret,omitempty"`
	Endpoint     oauth2.Endpoint `json:"endpoint"`
	RedirectURL  string          `json:"redirect_url"`
	Scopes       []string        `json:"scopes,omitempty"`
}

func configFromOAuth2(cfg *oauth2.Config) OAuth2Config {
	return OAuth2Config{
		ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, Endpoint: cfg.Endpoint,
		RedirectURL: cfg.RedirectURL, Scopes: append([]string(nil), cfg.Scopes...),
	}
}

func (c OAuth2Config) oauth2Config() *oauth2.Config {
	return &oauth2.Config{
		ClientID: c.ClientID, ClientSecret: c.ClientSecret, Endpoint: c.Endpoint,
		RedirectURL: c.RedirectURL, Scopes: append([]string(nil), c.Scopes...),
	}
}

// Session is one persisted MCP OAuth grant. Credential-bearing values in this
// type must stay inside this package and the private store file.
type Session struct {
	Version            uint64        `json:"version"`
	Endpoint           string        `json:"endpoint"`
	Issuer             string        `json:"issuer"`
	Config             OAuth2Config  `json:"config"`
	Token              *oauth2.Token `json:"token"`
	RevocationEndpoint string        `json:"revocation_endpoint,omitempty"`
	UpdatedAt          time.Time     `json:"updated_at"`
}

// Store is the persistence contract used by Coordinator. Update executes while
// holding both an in-process mutex and a cross-process file lock, allowing a
// refresh-token exchange and its rotated-token write to be serialized.
type Store interface {
	Path() string
	Load(endpoint string) (*Session, error)
	Update(endpoint string, fn func(*Session) (*Session, error)) (*Session, error)
	Delete(endpoint string) (*Session, error)
}

type storeFile struct {
	Version int                 `json:"version"`
	Entries map[string]*Session `json:"entries"`
}

// FileStore stores all MCP grants in one private, versioned JSON file.
type FileStore struct {
	path string
	mu   sync.Mutex
}

func DefaultStorePath() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "term-llm", "mcp_oauth.json"), nil
}

func NewFileStore(path string) *FileStore { return &FileStore{path: path} }
func (s *FileStore) Path() string         { return s.path }

// CanonicalEndpoint normalizes identity without changing path or query, which
// are part of the OAuth resource identity.
func CanonicalEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse MCP endpoint: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("MCP endpoint must use http or https")
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("MCP endpoint is missing a host")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port != "" && !((u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80")) {
		host = host + ":" + port
	}
	if strings.Contains(u.Hostname(), ":") {
		host = "[" + strings.ToLower(u.Hostname()) + "]"
		if port != "" && !((u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80")) {
			host += ":" + port
		}
	}
	u.Host = host
	u.Fragment = ""
	return u.String(), nil
}

func entryKey(endpoint, issuer string) string {
	sum := sha256.Sum256([]byte(endpoint + "\x00" + strings.TrimSuffix(issuer, "/")))
	return hex.EncodeToString(sum[:])
}

func cloneSession(in *Session) *Session {
	if in == nil {
		return nil
	}
	out := *in
	out.Config.Scopes = append([]string(nil), in.Config.Scopes...)
	if in.Token != nil {
		tok := *in.Token
		out.Token = &tok
	}
	return &out
}

func (s *FileStore) Load(endpoint string) (*Session, error) {
	canonical, err := CanonicalEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	var found *Session
	err = s.withLock(func() error {
		data, err := s.readLocked()
		if err != nil {
			return err
		}
		for _, entry := range data.Entries {
			if entry.Endpoint == canonical && (found == nil || entry.Version > found.Version) {
				found = cloneSession(entry)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}

func (s *FileStore) Update(endpoint string, fn func(*Session) (*Session, error)) (*Session, error) {
	canonical, err := CanonicalEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	var result *Session
	err = s.withLock(func() error {
		data, err := s.readLocked()
		if err != nil {
			return err
		}
		var current *Session
		var currentKey string
		for key, entry := range data.Entries {
			if entry.Endpoint == canonical && (current == nil || entry.Version > current.Version) {
				current, currentKey = entry, key
			}
		}
		next, err := fn(cloneSession(current))
		if err != nil {
			return err
		}
		if next == nil {
			result = cloneSession(current)
			return nil
		}
		if next.Endpoint != "" {
			normalized, err := CanonicalEndpoint(next.Endpoint)
			if err != nil {
				return err
			}
			if normalized != canonical {
				return fmt.Errorf("credential endpoint changed during update")
			}
		}
		next = cloneSession(next)
		next.Endpoint = canonical
		if current == nil {
			if next.Version != 0 {
				return ErrStaleVersion
			}
			next.Version = 1
		} else {
			if next.Version != current.Version {
				return ErrStaleVersion
			}
			next.Version = current.Version + 1
			delete(data.Entries, currentKey)
		}
		next.UpdatedAt = time.Now().UTC()
		data.Entries[entryKey(canonical, next.Issuer)] = next
		if err := s.writeLocked(data); err != nil {
			return err
		}
		result = cloneSession(next)
		return nil
	})
	return result, err
}

func (s *FileStore) Delete(endpoint string) (*Session, error) {
	canonical, err := CanonicalEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	var deleted *Session
	err = s.withLock(func() error {
		data, err := s.readLocked()
		if err != nil {
			return err
		}
		changed := false
		for key, entry := range data.Entries {
			if entry.Endpoint == canonical {
				if deleted == nil || entry.Version > deleted.Version {
					deleted = cloneSession(entry)
				}
				delete(data.Entries, key)
				changed = true
			}
		}
		if changed {
			return s.writeLocked(data)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if deleted == nil {
		return nil, ErrNotFound
	}
	return deleted, nil
}

func (s *FileStore) withLock(fn func() error) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return fmt.Errorf("MCP OAuth store path is empty")
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create MCP OAuth store directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure MCP OAuth store directory: %w", err)
	}
	if err := rejectUnsafeTarget(s.path); err != nil {
		return err
	}
	lockPath := s.path + ".lock"
	if err := rejectUnsafeTarget(lockPath); err != nil {
		return err
	}
	unlock, err := filelock.Lock(lockPath)
	if err != nil {
		return fmt.Errorf("lock MCP OAuth store: %w", err)
	}
	defer func() {
		if unlockErr := unlock(); err == nil && unlockErr != nil {
			err = fmt.Errorf("unlock MCP OAuth store: %w", unlockErr)
		}
	}()
	if err := os.Chmod(lockPath, 0o600); err != nil {
		return fmt.Errorf("secure MCP OAuth lock file: %w", err)
	}
	if err := rejectUnsafeTarget(s.path); err != nil {
		return err
	}
	if _, statErr := os.Stat(s.path); statErr == nil {
		if err := os.Chmod(s.path, 0o600); err != nil {
			return fmt.Errorf("secure MCP OAuth store: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect MCP OAuth store: %w", statErr)
	}
	return fn()
}

func rejectUnsafeTarget(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect MCP OAuth store target: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("MCP OAuth store target must be a regular file")
	}
	return nil
}

func (s *FileStore) readLocked() (*storeFile, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return &storeFile{Version: storeSchemaVersion, Entries: make(map[string]*Session)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read MCP OAuth store: %w", err)
	}
	var file storeFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse MCP OAuth store: %w", err)
	}
	if file.Version != storeSchemaVersion {
		return nil, fmt.Errorf("unsupported MCP OAuth store version %d", file.Version)
	}
	if file.Entries == nil {
		return nil, fmt.Errorf("invalid MCP OAuth store: missing entries")
	}
	for _, entry := range file.Entries {
		if entry == nil || entry.Token == nil || entry.Config.ClientID == "" || entry.Endpoint == "" {
			return nil, fmt.Errorf("invalid MCP OAuth store entry")
		}
		canonical, err := CanonicalEndpoint(entry.Endpoint)
		if err != nil || canonical != entry.Endpoint {
			return nil, fmt.Errorf("invalid MCP OAuth store endpoint")
		}
	}
	return &file, nil
}

func (s *FileStore) writeLocked(file *storeFile) (err error) {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal MCP OAuth store: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".mcp_oauth-*")
	if err != nil {
		return fmt.Errorf("create MCP OAuth temporary store: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return fmt.Errorf("write MCP OAuth temporary store: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close MCP OAuth temporary store: %w", closeErr)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace MCP OAuth store: %w", err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("secure MCP OAuth store: %w", err)
	}
	if dirFile, err := os.Open(dir); err == nil {
		if syncErr := dirFile.Sync(); syncErr != nil && !errors.Is(syncErr, context.Canceled) {
			_ = dirFile.Close()
			return fmt.Errorf("sync MCP OAuth store directory: %w", syncErr)
		}
		_ = dirFile.Close()
	}
	return nil
}

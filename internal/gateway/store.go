package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultMaxConcurrentInference = 2
	DefaultSearchRatePerMinute    = 30
	DefaultSearchBurst            = 5
	DefaultMaxConcurrentSearch    = 2
	DefaultFetchRatePerMinute     = 30
	DefaultFetchBurst             = 5
	DefaultMaxConcurrentFetch     = 2
	DefaultEnrollmentTTL          = 15 * time.Minute
	MaxEnrollmentTTL              = 24 * time.Hour
)

type Policy struct {
	AllowProviders         []string `json:"allow_providers,omitempty"`
	DenyProviders          []string `json:"deny_providers,omitempty"`
	AllowModels            []string `json:"allow_models,omitempty"`
	DenyModels             []string `json:"deny_models,omitempty"`
	AllowCLI               bool     `json:"allow_cli,omitempty"`
	AllowSearch            bool     `json:"allow_search"`
	AllowFetch             bool     `json:"allow_fetch"`
	MaxConcurrentInference int      `json:"max_concurrent_inference,omitempty"`
	SearchRatePerMinute    int      `json:"search_rate_per_minute,omitempty"`
	SearchBurst            int      `json:"search_burst,omitempty"`
	MaxConcurrentSearch    int      `json:"max_concurrent_search,omitempty"`
	FetchRatePerMinute     int      `json:"fetch_rate_per_minute,omitempty"`
	FetchBurst             int      `json:"fetch_burst,omitempty"`
	MaxConcurrentFetch     int      `json:"max_concurrent_fetch,omitempty"`
}

type Client struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TokenHash string    `json:"token_hash"`
	CreatedAt time.Time `json:"created_at"`
	RevokedAt time.Time `json:"revoked_at,omitempty"`
	Policy    Policy    `json:"policy"`
}

type Enrollment struct {
	Name      string    `json:"name"`
	TokenHash string    `json:"token_hash"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	UsedAt    time.Time `json:"used_at,omitempty"`
	Policy    Policy    `json:"policy"`
}

type ClientStore struct {
	path           string
	enrollmentPath string
	mu             sync.RWMutex
	clients        []Client
	enrollments    []Enrollment
}

func OpenClientStore(path string) (*ClientStore, error) {
	store := &ClientStore{path: path, enrollmentPath: strings.TrimSuffix(path, filepath.Ext(path)) + ".enrollments.json"}
	if err := readJSONFile(path, &store.clients); err != nil {
		return nil, fmt.Errorf("read gateway clients: %w", err)
	}
	if err := readJSONFile(store.enrollmentPath, &store.enrollments); err != nil {
		return nil, fmt.Errorf("read gateway enrollments: %w", err)
	}
	return store, nil
}

func readJSONFile(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

func (s *ClientStore) Add(name string, policy Policy) (Client, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Client{}, "", fmt.Errorf("client name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockGatewayClientStore(s.path + ".lock")
	if err != nil {
		return Client{}, "", err
	}
	defer func() { _ = unlock() }()
	var clients []Client
	if err := readJSONFile(s.path, &clients); err != nil {
		return Client{}, "", fmt.Errorf("reload gateway clients: %w", err)
	}
	s.clients = clients
	return s.addLocked(name, policy)
}

func (s *ClientStore) addLocked(name string, policy Policy) (Client, string, error) {
	if s.activeClientNameLocked(name) {
		return Client{}, "", fmt.Errorf("active gateway client %q already exists; revoke it before rotating credentials", name)
	}
	token, err := randomSecret("tlg1", 32)
	if err != nil {
		return Client{}, "", err
	}
	clientID, err := randomSecret("client", 12)
	if err != nil {
		return Client{}, "", err
	}
	client := Client{ID: clientID, Name: name, TokenHash: hashToken(token), CreatedAt: time.Now().UTC(), Policy: policy}
	s.clients = append(s.clients, client)
	if err := s.saveClientsLocked(); err != nil {
		s.clients = s.clients[:len(s.clients)-1]
		return Client{}, "", err
	}
	return client, token, nil
}

// CreateEnrollment creates a persisted, single-use bootstrap token bound to a
// client name, restricted policy, and short expiry.
func (s *ClientStore) CreateEnrollment(name string, policy Policy, ttl time.Duration) (Enrollment, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Enrollment{}, "", fmt.Errorf("enrollment client name is required")
	}
	if len(policy.AllowProviders) == 0 && len(policy.AllowModels) == 0 {
		return Enrollment{}, "", fmt.Errorf("enrollment requires --allow-provider or --allow-model")
	}
	if ttl <= 0 {
		ttl = DefaultEnrollmentTTL
	}
	if ttl > MaxEnrollmentTTL {
		return Enrollment{}, "", fmt.Errorf("enrollment TTL must not exceed %s", MaxEnrollmentTTL)
	}
	token, err := randomSecret("tlge1", 32)
	if err != nil {
		return Enrollment{}, "", err
	}
	now := time.Now().UTC()
	enrollment := Enrollment{Name: name, TokenHash: hashToken(token), CreatedAt: now, ExpiresAt: now.Add(ttl), Policy: policy}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockGatewayClientStore(s.path + ".lock")
	if err != nil {
		return Enrollment{}, "", err
	}
	defer func() { _ = unlock() }()
	var clients []Client
	if err := readJSONFile(s.path, &clients); err != nil {
		return Enrollment{}, "", fmt.Errorf("reload gateway clients: %w", err)
	}
	s.clients = clients
	if s.activeClientNameLocked(name) {
		return Enrollment{}, "", fmt.Errorf("active gateway client %q already exists; revoke it before rotating credentials", name)
	}
	var enrollments []Enrollment
	if err := readJSONFile(s.enrollmentPath, &enrollments); err != nil {
		return Enrollment{}, "", fmt.Errorf("reload gateway enrollments: %w", err)
	}
	s.enrollments = enrollments
	s.enrollments = append(s.enrollments, enrollment)
	if err := s.saveEnrollmentsLocked(); err != nil {
		s.enrollments = s.enrollments[:len(s.enrollments)-1]
		return Enrollment{}, "", err
	}
	return enrollment, token, nil
}

// ConsumeEnrollment atomically marks a valid token used before minting its
// per-client credential. A token can never create two clients, including across
// process restarts.
func (s *ClientStore) ConsumeEnrollment(token, requestedName string) (Client, string, error) {
	hash := hashToken(strings.TrimSpace(token))
	requestedName = strings.TrimSpace(requestedName)
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockGatewayClientStore(s.path + ".lock")
	if err != nil {
		return Client{}, "", err
	}
	defer func() { _ = unlock() }()
	var enrollments []Enrollment
	if err := readJSONFile(s.enrollmentPath, &enrollments); err != nil {
		return Client{}, "", fmt.Errorf("reload gateway enrollments: %w", err)
	}
	s.enrollments = enrollments
	var clients []Client
	if err := readJSONFile(s.path, &clients); err != nil {
		return Client{}, "", fmt.Errorf("reload gateway clients: %w", err)
	}
	s.clients = clients
	for i := range s.enrollments {
		enrollment := &s.enrollments[i]
		if subtle.ConstantTimeCompare([]byte(hash), []byte(enrollment.TokenHash)) != 1 {
			continue
		}
		if !enrollment.UsedAt.IsZero() {
			return Client{}, "", fmt.Errorf("enrollment token has already been used")
		}
		if !now.Before(enrollment.ExpiresAt) {
			return Client{}, "", fmt.Errorf("enrollment token has expired")
		}
		if requestedName != "" && requestedName != enrollment.Name {
			return Client{}, "", fmt.Errorf("enrollment token is bound to client %q", enrollment.Name)
		}
		if s.activeClientNameLocked(enrollment.Name) {
			return Client{}, "", fmt.Errorf("active gateway client %q already exists; revoke it before rotating credentials", enrollment.Name)
		}
		enrollment.UsedAt = now
		if err := s.saveEnrollmentsLocked(); err != nil {
			enrollment.UsedAt = time.Time{}
			return Client{}, "", err
		}
		client, clientToken, err := s.addLocked(enrollment.Name, enrollment.Policy)
		if err != nil {
			enrollment.UsedAt = time.Time{}
			if rollbackErr := s.saveEnrollmentsLocked(); rollbackErr != nil {
				return Client{}, "", fmt.Errorf("create enrolled client: %v; restore enrollment token: %w", err, rollbackErr)
			}
			return Client{}, "", err
		}
		return client, clientToken, nil
	}
	return Client{}, "", fmt.Errorf("invalid enrollment token")
}

// Authenticate reloads the durable client file before every decision. Management
// commands intentionally open a separate ClientStore, so request-time reloads make
// additions and revocations visible to a running gateway as soon as the atomic
// rename reaches the filesystem; there is no polling interval or process restart.
func (s *ClientStore) Authenticate(token string) (Client, bool) {
	hash := hashToken(strings.TrimSpace(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	var clients []Client
	if err := readJSONFile(s.path, &clients); err != nil {
		// Authentication fails closed if the durable store cannot be read. Atomic
		// writers ensure a valid old or new file is normally observed.
		return Client{}, false
	}
	s.clients = clients
	for _, client := range s.clients {
		if !client.RevokedAt.IsZero() {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hash), []byte(client.TokenHash)) == 1 {
			return client, true
		}
	}
	return Client{}, false
}

func (s *ClientStore) List() []Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]Client(nil), s.clients...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (s *ClientStore) Revoke(idOrName string) error {
	idOrName = strings.TrimSpace(idOrName)
	if idOrName == "" {
		return fmt.Errorf("gateway client ID or name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockGatewayClientStore(s.path + ".lock")
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	var clients []Client
	if err := readJSONFile(s.path, &clients); err != nil {
		return fmt.Errorf("reload gateway clients: %w", err)
	}
	s.clients = clients

	// IDs take precedence over names. Name-based revocation covers every active
	// legacy match, while new writes enforce one active client per name.
	for i := range s.clients {
		if s.clients[i].ID == idOrName {
			if !s.clients[i].RevokedAt.IsZero() {
				return nil
			}
			previous := s.clients[i].RevokedAt
			s.clients[i].RevokedAt = time.Now().UTC()
			if err := s.saveClientsLocked(); err != nil {
				s.clients[i].RevokedAt = previous
				return err
			}
			return nil
		}
	}

	matched := false
	changed := false
	previous := append([]Client(nil), s.clients...)
	now := time.Now().UTC()
	for i := range s.clients {
		if s.clients[i].Name != idOrName {
			continue
		}
		matched = true
		if s.clients[i].RevokedAt.IsZero() {
			s.clients[i].RevokedAt = now
			changed = true
		}
	}
	if !matched {
		return fmt.Errorf("gateway client %q not found", idOrName)
	}
	if !changed {
		return nil
	}
	if err := s.saveClientsLocked(); err != nil {
		s.clients = previous
		return err
	}
	return nil
}

func (s *ClientStore) activeClientNameLocked(name string) bool {
	for _, client := range s.clients {
		if client.Name == name && client.RevokedAt.IsZero() {
			return true
		}
	}
	return false
}

func (s *ClientStore) saveClientsLocked() error {
	return writeSecureJSON(s.path, s.clients, "gateway clients")
}

func (s *ClientStore) saveEnrollmentsLocked() error {
	return writeSecureJSON(s.enrollmentPath, s.enrollments, "gateway enrollments")
}

func writeSecureJSON(path string, value any, label string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create gateway state directory: %w", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", label, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", label, err)
	}
	return nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomSecret(prefix string, size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate secure gateway identifier: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(raw), nil
}

func (p Policy) Allows(provider, model string, cli bool) bool {
	if !p.AllowsProvider(provider, cli) {
		return false
	}
	if matchesPolicy(p.DenyModels, provider+":"+model) || matchesPolicy(p.DenyModels, model) {
		return false
	}
	if len(p.AllowModels) > 0 && !matchesPolicy(p.AllowModels, provider+":"+model) && !matchesPolicy(p.AllowModels, model) {
		return false
	}
	return true
}

func (p Policy) AllowsProvider(provider string, cli bool) bool {
	if cli && !p.AllowCLI {
		return false
	}
	if matchesPolicy(p.DenyProviders, provider) {
		return false
	}
	return len(p.AllowProviders) == 0 || matchesPolicy(p.AllowProviders, provider)
}

func (p Policy) InferenceConcurrency() int {
	if p.MaxConcurrentInference > 0 {
		return p.MaxConcurrentInference
	}
	return DefaultMaxConcurrentInference
}

func matchesPolicy(patterns []string, value string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "*" || pattern == value {
			return true
		}
		if strings.HasSuffix(pattern, "*") && strings.HasPrefix(value, strings.TrimSuffix(pattern, "*")) {
			return true
		}
	}
	return false
}

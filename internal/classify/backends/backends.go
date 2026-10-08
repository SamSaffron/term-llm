// Package backends builds classify.Backend implementations from
// classify.providers configuration.
package backends

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/classify"
	"github.com/samsaffron/term-llm/internal/classify/cloudflare"
	"github.com/samsaffron/term-llm/internal/classify/openai"
	"github.com/samsaffron/term-llm/internal/classify/typesafe"
	"github.com/samsaffron/term-llm/internal/config"
)

// Connection holds the resolved settings used to construct a backend.
type Connection struct {
	// Type is the provider type: config.ClassifyProviderTypeSafe or
	// config.ClassifyProviderOpenAI.
	Type    string
	APIKey  string
	BaseURL string
	// AccountID fills {account_id} in a cloudflare BaseURL.
	AccountID string
	Timeout   time.Duration
}

// Overrides are per-invocation settings that take precedence over the
// provider configuration. Zero values keep the configured setting.
type Overrides struct {
	BaseURL string
	Timeout time.Duration
}

// Factory constructs a backend from a resolved connection.
type Factory func(Connection) (classify.Backend, error)

// Resolve resolves a selected provider's credential, endpoint, and timeout.
// Only this provider's credential is resolved, and it is chosen for the
// effective endpoint: a --base-url override pointing away from the type's
// default endpoint never receives the shared environment key.
func Resolve(provider config.ClassifyProviderConfig, overrides Overrides) (Connection, error) {
	conn := Connection{Type: provider.Type, Timeout: overrides.Timeout}
	var err error
	conn.BaseURL = strings.TrimSpace(overrides.BaseURL)
	if conn.BaseURL == "" {
		if conn.BaseURL, err = provider.BaseURLRef().Resolve(); err != nil {
			return conn, fmt.Errorf("resolve classify provider base URL: %w", err)
		}
	}
	if conn.APIKey, err = provider.KeyFor(conn.BaseURL).Resolve(); err != nil {
		return conn, fmt.Errorf("resolve classify provider API key: %w", err)
	}
	if provider.Type == config.ClassifyProviderCloudflare {
		if conn.AccountID, err = provider.AccountIDRef().Resolve(); err != nil {
			return conn, fmt.Errorf("resolve classify provider account ID: %w", err)
		}
	}
	if conn.Timeout == 0 && provider.TimeoutSeconds != 0 {
		if conn.Timeout, err = ProviderTimeout(provider); err != nil {
			return conn, err
		}
	}
	return conn, nil
}

// ProviderTimeout converts the provider's timeout_seconds, rejecting values
// that are negative or overflow time.Duration.
func ProviderTimeout(provider config.ClassifyProviderConfig) (time.Duration, error) {
	if provider.TimeoutSeconds < 0 {
		return 0, errors.New("classify provider timeout_seconds must not be negative")
	}
	const maxTimeoutSeconds = int64(1<<63-1) / int64(time.Second)
	if int64(provider.TimeoutSeconds) > maxTimeoutSeconds {
		return 0, errors.New("classify provider timeout_seconds is too large")
	}
	return time.Duration(provider.TimeoutSeconds) * time.Second, nil
}

// New constructs the backend for conn.Type.
func New(conn Connection) (classify.Backend, error) {
	switch conn.Type {
	case config.ClassifyProviderTypeSafe:
		return typesafe.NewClient(typesafe.Options{APIKey: conn.APIKey, BaseURL: conn.BaseURL, Timeout: conn.Timeout})
	case config.ClassifyProviderOpenAI:
		return openai.NewClient(openai.Options{APIKey: conn.APIKey, BaseURL: conn.BaseURL, Timeout: conn.Timeout})
	case config.ClassifyProviderCloudflare:
		return cloudflare.NewClient(cloudflare.Options{APIKey: conn.APIKey, BaseURL: conn.BaseURL, AccountID: conn.AccountID, Timeout: conn.Timeout})
	default:
		return nil, fmt.Errorf("unsupported classify provider type %q", conn.Type)
	}
}

// Open resolves provider and constructs its backend with factory (New when
// nil).
func Open(provider config.ClassifyProviderConfig, overrides Overrides, factory Factory) (classify.Backend, error) {
	conn, err := Resolve(provider, overrides)
	if err != nil {
		return nil, err
	}
	if factory == nil {
		factory = New
	}
	return factory(conn)
}

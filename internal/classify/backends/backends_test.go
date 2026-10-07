package backends

import (
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/classify"
	"github.com/samsaffron/term-llm/internal/classify/openai"
	"github.com/samsaffron/term-llm/internal/classify/typesafe"
	"github.com/samsaffron/term-llm/internal/config"
)

func TestOpenDispatchesByProviderType(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("TYPESAFE_API_KEY", "typesafe-key")
	cfg := config.ClassifyConfig{}
	for name, isWant := range map[string]func(classify.Backend) bool{
		config.ClassifyProviderTypeSafe: func(b classify.Backend) bool { _, ok := b.(*typesafe.Client); return ok },
		config.ClassifyProviderOpenAI:   func(b classify.Backend) bool { _, ok := b.(*openai.Client); return ok },
	} {
		provider, err := cfg.ResolveProvider(name)
		if err != nil {
			t.Fatal(err)
		}
		backend, err := Open(provider, Overrides{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !isWant(backend) {
			t.Fatalf("%s backend = %T", name, backend)
		}
	}
}

func TestResolveAppliesProviderSettingsAndOverrides(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "openai-key")
	cfg := config.ClassifyConfig{}
	provider, err := cfg.ResolveProvider(config.ClassifyProviderOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := Resolve(provider, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	want := Connection{Type: config.ClassifyProviderOpenAI, APIKey: "openai-key", BaseURL: config.DefaultOpenAIDecisionsBaseURL, Timeout: 10 * time.Second}
	if conn != want {
		t.Fatalf("conn = %#v, want %#v", conn, want)
	}
	conn, err = Resolve(provider, Overrides{BaseURL: "https://proxy.example/v1", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if conn.BaseURL != "https://proxy.example/v1" || conn.Timeout != time.Second {
		t.Fatalf("overrides not applied: %#v", conn)
	}
}

// TestResolveWithholdsEnvKeyFromOverrideHost pins that --base-url cannot ship
// the shared environment credential to another host.
func TestResolveWithholdsEnvKeyFromOverrideHost(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("TYPESAFE_API_KEY", "typesafe-key")
	cfg := config.ClassifyConfig{}
	for _, tc := range []struct {
		name, apiKey, override, want string
	}{
		{config.ClassifyProviderOpenAI, "", "https://proxy.example/v1", ""},
		{config.ClassifyProviderTypeSafe, "", "https://proxy.example", ""},
		{config.ClassifyProviderOpenAI, "explicit", "https://proxy.example/v1", "explicit"},
		{config.ClassifyProviderOpenAI, "", config.DefaultOpenAIDecisionsBaseURL + "/", "openai-key"},
	} {
		provider, err := cfg.ResolveProvider(tc.name)
		if err != nil {
			t.Fatal(err)
		}
		provider.APIKey = tc.apiKey
		conn, err := Resolve(provider, Overrides{BaseURL: tc.override})
		if err != nil {
			t.Fatal(err)
		}
		if conn.APIKey != tc.want {
			t.Fatalf("%s override %s: key %q, want %q", tc.name, tc.override, conn.APIKey, tc.want)
		}
	}
}

func TestOpenUsesFactoryAndRejectsBadTimeouts(t *testing.T) {
	var got Connection
	factory := func(conn Connection) (classify.Backend, error) { got = conn; return nil, nil }
	provider := config.ClassifyProviderConfig{Type: config.ClassifyProviderTypeSafe, APIKey: "k", BaseURL: "https://x", TimeoutSeconds: 3}
	if _, err := Open(provider, Overrides{}, factory); err != nil || got.Timeout != 3*time.Second || got.APIKey != "k" {
		t.Fatalf("factory conn = %#v, err %v", got, err)
	}
	provider.TimeoutSeconds = -1
	if _, err := Open(provider, Overrides{}, factory); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative timeout err = %v", err)
	}
	if _, err := New(Connection{Type: "other"}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported type err = %v", err)
	}
}

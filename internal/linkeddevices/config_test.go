package linkeddevices

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/peerbroker"
)

func TestLinkedDevicesServerOptionsPreserveExplicitICEPolicy(t *testing.T) {
	t.Setenv("TERM_LLM_LINKED_DEVICES_BROKER_URL", "")
	cfg := &config.Config{LinkedDevices: config.LinkedDevicesConfig{
		BrokerURL:           "https://broker.example.com",
		AdmissionCredential: strings.Repeat("a", 32),
		ICEServers: []config.LinkedDevicesICEServerConfig{
			{URLs: []string{"stun:stun.example.com:3478"}},
			{URLs: []string{"turns:turn.example.com:5349?transport=tcp"}, CredentialSource: "static", Username: "device", Credential: "secret"},
		},
	}}

	got := ServerOptionsFromConfig(cfg)
	if got.UnavailableReason != "" || got.BrokerURL != cfg.LinkedDevices.BrokerURL || got.AdmissionCredential != cfg.LinkedDevices.AdmissionCredential {
		t.Fatal("server options did not preserve configured broker fields")
	}
	if got.ICEPolicy == nil || !got.ICEPolicy.Configured() {
		t.Fatal("ICE policy was not explicitly configured")
	}
	servers := got.ICEPolicy.Servers()
	if len(servers) != 2 || servers[1].CredentialSource != peerbroker.CredentialSourceStatic || servers[1].Credential != "secret" {
		t.Fatalf("ICE servers = %+v", servers)
	}
}

func TestLinkedDevicesServerOptionsPreserveHostOnlyPolicy(t *testing.T) {
	t.Setenv("TERM_LLM_LINKED_DEVICES_BROKER_URL", "http://127.0.0.1:1")
	cfg := &config.Config{LinkedDevices: config.LinkedDevicesConfig{ICEServers: []config.LinkedDevicesICEServerConfig{}}}
	got := ServerOptionsFromConfig(cfg)
	if got.ICEPolicy == nil || !got.ICEPolicy.Configured() || !got.ICEPolicy.Empty() {
		t.Fatalf("explicit empty ICE policy = %+v", got.ICEPolicy)
	}
}

func TestLinkedDevicesServerOptionsDoNotResolveCredentialsWhileDisabled(t *testing.T) {
	t.Setenv("TERM_LLM_LINKED_DEVICES_BROKER_URL", "")
	marker := filepath.Join(t.TempDir(), "resolved")
	reference := "$(touch " + marker + "; printf secret)"
	cfg := &config.Config{LinkedDevices: config.LinkedDevicesConfig{
		Diagnostics:         true,
		AdmissionCredential: reference,
		ICEServers: []config.LinkedDevicesICEServerConfig{{
			URLs: []string{"turn:relay.example.com:3478"}, CredentialSource: "static", Username: "device", Credential: reference,
		}},
	}}

	got := ServerOptionsFromConfig(cfg)
	if got.UnavailableReason != "" || !got.Diagnostics {
		t.Fatalf("disabled configuration options = %+v", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("disabled linked devices resolved a credential: %v", err)
	}
}

func TestLinkedDevicesServerOptionsDiagnoseInvalidEnvironment(t *testing.T) {
	t.Run("broker URL", func(t *testing.T) {
		t.Setenv("TERM_LLM_LINKED_DEVICES_BROKER_URL", "http://10.0.0.5:8091")
		got := ServerOptionsFromConfig(&config.Config{})
		if !strings.Contains(got.UnavailableReason, "broker URL") {
			t.Fatalf("unavailable reason = %q", got.UnavailableReason)
		}
	})
	t.Run("admission", func(t *testing.T) {
		t.Setenv("TERM_LLM_LINKED_DEVICES_BROKER_URL", "https://broker.example.com")
		t.Setenv("TERM_LLM_LINKED_DEVICES_ADMISSION", "short")
		got := ServerOptionsFromConfig(&config.Config{})
		if !strings.Contains(got.UnavailableReason, "credentials") || strings.Contains(got.UnavailableReason, "short") {
			t.Fatalf("unavailable reason = %q", got.UnavailableReason)
		}
	})
}

func TestLinkedDevicesServerOptionsRedactResolutionFailure(t *testing.T) {
	t.Setenv("TERM_LLM_LINKED_DEVICES_BROKER_URL", "http://127.0.0.1:1")
	secretPath := filepath.Join(t.TempDir(), "missing-private-secret")
	cfg := &config.Config{LinkedDevices: config.LinkedDevicesConfig{
		AdmissionCredential: "file://" + secretPath,
		ICEServers:          config.DefaultLinkedDevicesICEServers(),
	}}

	got := ServerOptionsFromConfig(cfg)
	if got.UnavailableReason == "" || strings.Contains(got.UnavailableReason, secretPath) {
		t.Fatalf("unavailable reason = %q", got.UnavailableReason)
	}
	if got.BrokerURL != "" || got.ICEPolicy != nil {
		t.Fatalf("failed resolution published partial configuration: %+v", got)
	}
}

func TestLinkedDevicesLoadedPolicyDoesNotUseBrokerTURNDefaults(t *testing.T) {
	for _, tc := range []struct{ name, yamlValue, wantICE string }{
		{"omitted", "", `[{"urls":["stun:stun.term-llm.com:3478"]}]`},
		{"empty", "  ice_servers: []\n", `[]`},
		{"null", "  ice_servers: null\n", `[{"urls":["stun:stun.term-llm.com:3478"]}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configDir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", configDir)
			t.Setenv("TERM_LLM_LINKED_DEVICES_BROKER_URL", "")
			t.Setenv("TERM_LLM_LINKED_DEVICES_ADMISSION", "")
			provider := &unexpectedDefaultTURNProvider{}
			for _, withProvider := range []bool{false, true} {
				brokerConfig := peerbroker.Config{STUNURLs: []string{"stun:stun.example.com:3478"}}
				if withProvider {
					brokerConfig.TURNProvider = provider
				}
				handler, err := peerbroker.NewHandler(brokerConfig)
				if err != nil {
					t.Fatal(err)
				}
				defer handler.Close()
				var calls atomic.Int64
				broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					handler.ServeHTTP(w, r)
				}))
				defer broker.Close()
				if err := os.MkdirAll(filepath.Join(configDir, "term-llm"), 0700); err != nil {
					t.Fatal(err)
				}
				contents := "default_provider: openai\nlinked_devices:\n  broker_url: " + broker.URL + "\n" + tc.yamlValue
				if err := os.WriteFile(filepath.Join(configDir, "term-llm", "config.yaml"), []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
				cfg, err := config.Load()
				if err != nil {
					t.Fatal(err)
				}
				options := ServerOptionsFromConfig(cfg)
				if options.UnavailableReason != "" || options.ICEPolicy == nil || !options.ICEPolicy.Configured() {
					t.Fatalf("default policy must be explicitly configured: %+v", options)
				}
				runtime, err := New(Options{DataDir: t.TempDir(), Target: "default-policy", StoreInstanceID: "store", Authenticated: true, BrokerURL: options.BrokerURL, ICEPolicy: options.ICEPolicy})
				if err != nil {
					t.Fatal(err)
				}
				defer runtime.Close()
				if err := runtime.Start(); err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 0 {
					t.Fatalf("disabled runtime contacted broker %d times", calls.Load())
				}
				if err := runtime.Enable(t.Context()); err != nil {
					t.Fatal(err)
				}
				runtime.mu.Lock()
				client, hostID, capability := runtime.broker, runtime.hostID, runtime.leaseCapability
				runtime.mu.Unlock()
				ice, err := client.ICE(t.Context(), hostID, peerbroker.PeerHost, "", capability)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(ice.ICEServers)
				if err != nil {
					t.Fatal(err)
				}
				if string(encoded) != tc.wantICE || !ice.ExpiresAt.IsZero() || provider.calls.Load() != 0 {
					t.Fatalf("ICE policy fell back to broker defaults: servers=%s expiry=%v mints=%d", encoded, ice.ExpiresAt, provider.calls.Load())
				}
			}
		})
	}
}

// Neither the free STUN default nor host-only override may mint TURN credentials.
type unexpectedDefaultTURNProvider struct{ calls atomic.Int64 }

func (*unexpectedDefaultTURNProvider) URLs() []string {
	return []string{"turns:turn.example.com:5349?transport=tcp"}
}
func (p *unexpectedDefaultTURNProvider) Mint(context.Context, string, []string) (peerbroker.ICECredentials, error) {
	p.calls.Add(1)
	return peerbroker.ICECredentials{}, errors.New("unexpected TURN mint")
}

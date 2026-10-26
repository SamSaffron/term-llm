package linkeddevices

import (
	"os"
	"path/filepath"
	"strings"
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
		AdmissionCredential: reference,
		ICEServers: []config.LinkedDevicesICEServerConfig{{
			URLs: []string{"turn:relay.example.com:3478"}, CredentialSource: "static", Username: "device", Credential: reference,
		}},
	}}

	got := ServerOptionsFromConfig(cfg)
	if got.UnavailableReason != "" {
		t.Fatalf("disabled configuration reason = %q", got.UnavailableReason)
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

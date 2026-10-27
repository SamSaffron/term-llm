package config

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLinkedDevicesDefaults(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)

	// Write minimal config without linked_devices
	writeTestConfig(t, configDir, "default_provider: openai")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// broker_url has no schema default (so it cannot shadow the environment
	// override); the hosted default applies at resolution.
	t.Setenv("TERM_LLM_LINKED_DEVICES_BROKER_URL", "")
	if cfg.LinkedDevices.BrokerURL != "" {
		t.Errorf("expected unset broker_url, got %q", cfg.LinkedDevices.BrokerURL)
	}
	if got := cfg.LinkedDevicesBrokerURL(); got != DefaultLinkedDevicesBrokerURL {
		t.Errorf("effective broker URL = %q, want hosted default %q", got, DefaultLinkedDevicesBrokerURL)
	}
	if !cfg.LinkedDevicesEnabled() {
		t.Error("an ordinary configuration must have linked devices available")
	}
	if cfg.LinkedDevices.AdmissionCredential != "" {
		t.Errorf("expected empty admission_credential, got %q", cfg.LinkedDevices.AdmissionCredential)
	}
	if cfg.LinkedDevices.Diagnostics {
		t.Error("linked-device diagnostics must default off")
	}

	if len(cfg.LinkedDevices.ICEServers) != 1 {
		t.Fatalf("default ICE policy must contain only free STUN, got %+v", cfg.LinkedDevices.ICEServers)
	}
	server := cfg.LinkedDevices.ICEServers[0]
	if len(server.URLs) != 1 || server.URLs[0] != "stun:stun.term-llm.com:3478" || server.CredentialSource != "" || server.Username != "" || server.Credential != "" {
		t.Fatalf("default ICE server must be credential-free STUN, got %+v", server)
	}
}

func TestLinkedDevicesDiagnosticsOptIn(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	writeTestConfig(t, configDir, "default_provider: openai\nlinked_devices:\n  diagnostics: true\n")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LinkedDevices.Diagnostics {
		t.Fatal("linked_devices.diagnostics opt-in was not loaded")
	}
}

func TestLinkedDevicesExplicitEmpty(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)

	// Write config with explicit empty ice_servers
	writeTestConfig(t, configDir, `
default_provider: openai
linked_devices:
  ice_servers: []
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Explicit empty list should be preserved
	if cfg.LinkedDevices.ICEServers == nil {
		t.Fatal("ice_servers should not be nil when explicitly set to []")
	}
	if len(cfg.LinkedDevices.ICEServers) != 0 {
		t.Errorf("expected empty ice_servers, got %d entries", len(cfg.LinkedDevices.ICEServers))
	}
}

func TestLinkedDevicesCustomConfig(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)

	writeTestConfig(t, configDir, `
default_provider: openai
linked_devices:
  broker_url: https://broker.example.com
  admission_credential: secret123secret123secret123secret12
  ice_servers:
    - urls:
        - stun:stun.example.com:3478
    - urls:
        - turn:turn.example.com:3478
      credential_source: static
      username: user1
      credential: pass1
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.LinkedDevices.BrokerURL != "https://broker.example.com" {
		t.Errorf("expected broker_url https://broker.example.com, got %q", cfg.LinkedDevices.BrokerURL)
	}
	if cfg.LinkedDevices.AdmissionCredential != "secret123secret123secret123secret12" {
		t.Errorf("unexpected admission_credential %q", cfg.LinkedDevices.AdmissionCredential)
	}

	if len(cfg.LinkedDevices.ICEServers) != 2 {
		t.Fatalf("expected 2 ICE servers, got %d", len(cfg.LinkedDevices.ICEServers))
	}

	// Check STUN server
	stun := cfg.LinkedDevices.ICEServers[0]
	if len(stun.URLs) != 1 || stun.URLs[0] != "stun:stun.example.com:3478" {
		t.Errorf("unexpected STUN server URLs: %v", stun.URLs)
	}
	if stun.CredentialSource != "" {
		t.Errorf("STUN server should have empty credential_source, got %q", stun.CredentialSource)
	}

	// Check TURN server
	turn := cfg.LinkedDevices.ICEServers[1]
	if len(turn.URLs) != 1 || turn.URLs[0] != "turn:turn.example.com:3478" {
		t.Errorf("unexpected TURN server URLs: %v", turn.URLs)
	}
	if turn.CredentialSource != "static" {
		t.Errorf("expected credential_source static, got %q", turn.CredentialSource)
	}
	if turn.Username != "user1" {
		t.Errorf("expected username user1, got %q", turn.Username)
	}
	if turn.Credential != "pass1" {
		t.Errorf("expected credential pass1, got %q", turn.Credential)
	}
}

func TestLinkedDevicesValidationErrors(t *testing.T) {
	tests := []struct {
		name   string
		config string
		errMsg string
	}{
		{
			name: "too many servers",
			config: `
linked_devices:
  ice_servers:
    - urls: ["stun:s1.example.com:3478"]
    - urls: ["stun:s2.example.com:3478"]
    - urls: ["stun:s3.example.com:3478"]
    - urls: ["stun:s4.example.com:3478"]
    - urls: ["stun:s5.example.com:3478"]
    - urls: ["stun:s6.example.com:3478"]
    - urls: ["stun:s7.example.com:3478"]
    - urls: ["stun:s8.example.com:3478"]
    - urls: ["stun:s9.example.com:3478"]
`,
			errMsg: "at most 8 server entries allowed",
		},
		{
			name: "too many URLs per entry",
			config: `
linked_devices:
  ice_servers:
    - urls:
        - stun:s1.example.com:3478
        - stun:s2.example.com:3478
        - stun:s3.example.com:3478
        - stun:s4.example.com:3478
        - stun:s5.example.com:3478
        - stun:s6.example.com:3478
        - stun:s7.example.com:3478
        - stun:s8.example.com:3478
        - stun:s9.example.com:3478
`,
			errMsg: "at most 8 URLs per entry allowed",
		},
		{
			name: "empty URLs",
			config: `
linked_devices:
  ice_servers:
    - urls: []
`,
			errMsg: "urls: cannot be empty",
		},
		{
			name: "mixed STUN and TURN",
			config: `
linked_devices:
  ice_servers:
    - urls:
        - stun:stun.example.com:3478
        - turn:turn.example.com:3478
`,
			errMsg: "cannot mix STUN and TURN URLs",
		},
		{
			name: "TURN without credential_source",
			config: `
linked_devices:
  ice_servers:
    - urls:
        - turn:turn.example.com:3478
`,
			errMsg: "TURN URLs require credential_source",
		},
		{
			name: "STUN with username",
			config: `
linked_devices:
  ice_servers:
    - urls:
        - stun:stun.example.com:3478
      username: user1
`,
			errMsg: "username and credential forbidden for STUN",
		},
		{
			name: "broker with username",
			config: `
linked_devices:
  ice_servers:
    - urls:
        - turn:turn.example.com:3478
      credential_source: broker
      username: user1
`,
			errMsg: "username and credential forbidden for credential_source=broker",
		},
		{
			name: "static without username",
			config: `
linked_devices:
  ice_servers:
    - urls:
        - turn:turn.example.com:3478
      credential_source: static
      credential: pass1
`,
			errMsg: "credential_source=static requires username",
		},
		{
			name: "static without credential",
			config: `
linked_devices:
  ice_servers:
    - urls:
        - turn:turn.example.com:3478
      credential_source: static
      username: user1
`,
			errMsg: "credential_source=static requires credential",
		},
		{
			name: "invalid credential_source",
			config: `
linked_devices:
  ice_servers:
    - urls:
        - turn:turn.example.com:3478
      credential_source: invalid
`,
			errMsg: "invalid credential_source",
		},
		{
			name: "invalid URL scheme",
			config: `
linked_devices:
  ice_servers:
    - urls:
        - http://example.com
`,
			errMsg: "invalid scheme",
		},
		{
			name: "URL with userinfo",
			config: `
linked_devices:
  ice_servers:
    - urls:
        - stun:user:pass@stun.example.com:3478
`,
			errMsg: "URL userinfo not allowed",
		},
		{
			name: "URL with fragment",
			config: `
linked_devices:
  ice_servers:
    - urls:
        - stun:stun.example.com:3478#fragment
`,
			errMsg: "URL fragment not allowed",
		},
		{
			name: "broker with STUN URL",
			config: `
linked_devices:
  ice_servers:
    - urls:
        - stun:stun.example.com:3478
      credential_source: broker
`,
			errMsg: "credential_source=broker requires TURN URLs",
		},
		{
			name: "static with STUN URL",
			config: `
linked_devices:
  ice_servers:
    - urls:
        - stun:stun.example.com:3478
      credential_source: static
      username: user1
      credential: pass1
`,
			errMsg: "credential_source=static requires TURN URLs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configDir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", configDir)

			writeTestConfig(t, configDir, "default_provider: openai\n"+tt.config)

			_, err := Load()
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
			}
		})
	}
}

func TestLinkedDevicesURLLimits(t *testing.T) {
	t.Run("URL too long", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", configDir)

		url := "stun:" + strings.Repeat("a", 500) + ".example.com:3478"
		config := `
default_provider: openai
linked_devices:
  ice_servers:
    - urls:
        - "` + url + `"
`
		writeTestConfig(t, configDir, config)

		_, err := Load()
		if err == nil {
			t.Fatal("expected validation error, got nil")
		}
		if !strings.Contains(err.Error(), "exceeds 512 bytes") {
			t.Errorf("expected error containing %q, got %q", "exceeds 512 bytes", err.Error())
		}
	})

	t.Run("URL with control character rejected by YAML", func(t *testing.T) {
		// Control characters in URLs are rejected by the YAML parser itself,
		// which is even stricter than our validator. We just verify the config
		// fails to load.
		configDir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", configDir)

		url := "stun:stun.example.com:3478" + string(rune(0x01))
		config := `
default_provider: openai
linked_devices:
  ice_servers:
    - urls:
        - "` + url + `"
`
		writeTestConfig(t, configDir, config)

		_, err := Load()
		if err == nil {
			t.Fatal("expected error for control character in YAML, got nil")
		}
		// YAML parser rejects control characters before our validator runs
		if !strings.Contains(err.Error(), "control characters") {
			t.Errorf("expected error about control characters, got %q", err.Error())
		}
	})
}

func TestLinkedDevicesResolution(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("TEST_BROKER_URL", "https://broker.env.example.com")
	t.Setenv("TEST_ADMISSION", strings.Repeat("a", 32))
	t.Setenv("TEST_USERNAME", "env-user")
	t.Setenv("TEST_PASSWORD", "env-pass")

	writeTestConfig(t, configDir, `
default_provider: openai
linked_devices:
  broker_url: $TEST_BROKER_URL
  admission_credential: $TEST_ADMISSION
  ice_servers:
    - urls:
        - turn:turn.example.com:3478
      credential_source: static
      username: $TEST_USERNAME
      credential: $TEST_PASSWORD
`)

	cfg, err := loadUnresolved()
	if err != nil {
		t.Fatalf("loadUnresolved failed: %v", err)
	}

	// Before resolution, values should be unresolved
	if !strings.HasPrefix(cfg.LinkedDevices.BrokerURL, "$") {
		t.Errorf("expected unresolved broker_url, got %q", cfg.LinkedDevices.BrokerURL)
	}

	// Resolve
	if err := cfg.ResolveLinkedDevices(); err != nil {
		t.Fatalf("ResolveLinkedDevices failed: %v", err)
	}

	// After resolution
	if cfg.LinkedDevices.BrokerURL != "https://broker.env.example.com" {
		t.Errorf("expected resolved broker_url, got %q", cfg.LinkedDevices.BrokerURL)
	}
	if cfg.LinkedDevices.AdmissionCredential != strings.Repeat("a", 32) {
		t.Errorf("expected resolved admission_credential, got %q", cfg.LinkedDevices.AdmissionCredential)
	}

	turn := cfg.LinkedDevices.ICEServers[0]
	if turn.Username != "env-user" {
		t.Errorf("expected resolved username, got %q", turn.Username)
	}
	if turn.Credential != "env-pass" {
		t.Errorf("expected resolved credential, got %q", turn.Credential)
	}
}

func TestLinkedDevicesResolvedValueLimits(t *testing.T) {
	tests := []struct {
		name   string
		envVal string
		field  string
		errMsg string
	}{
		{
			name:   "username too long",
			envVal: strings.Repeat("a", 600),
			field:  "username",
			errMsg: "must be 1-256 bytes",
		},
		{
			name:   "username with control char",
			envVal: "user\x01name", // ASCII SOH control character
			field:  "username",
			errMsg: "contains control characters",
		},
		{
			name:   "credential too long",
			envVal: strings.Repeat("a", 600),
			field:  "credential",
			errMsg: "must be 1-256 bytes",
		},
		{
			name:   "credential with control char",
			envVal: "pass\nword",
			field:  "credential",
			errMsg: "contains control characters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configDir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", configDir)
			t.Setenv("TEST_VALUE", tt.envVal)

			config := `
default_provider: openai
linked_devices:
  ice_servers:
    - urls:
        - turn:turn.example.com:3478
      credential_source: static
      username: user1
      credential: pass1
`
			if tt.field == "username" {
				config = strings.Replace(config, "username: user1", "username: $TEST_VALUE", 1)
			} else {
				config = strings.Replace(config, "credential: pass1", "credential: $TEST_VALUE", 1)
			}

			writeTestConfig(t, configDir, config)

			cfg, err := loadUnresolved()
			if err != nil {
				t.Fatalf("loadUnresolved failed: %v", err)
			}

			err = cfg.ResolveLinkedDevices()
			if err == nil {
				t.Fatal("expected resolution error, got nil")
			}
			if !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
			}
		})
	}
}

func TestLinkedDevicesBrokerURLPrecedence(t *testing.T) {
	const yamlURL, envURL = "https://broker.example.com", "https://broker.env.example.com"
	tests := []struct {
		name, config, env, want string
	}{
		{name: "explicit YAML", config: "linked_devices:\n  broker_url: " + yamlURL, want: yamlURL},
		{name: "environment fallback", env: envURL, want: envURL},
		{name: "YAML wins over environment", config: "linked_devices:\n  broker_url: " + yamlURL, env: envURL, want: yamlURL},
		{name: "empty YAML uses environment", config: "linked_devices:\n  broker_url: \"\"", env: envURL, want: envURL},
		{name: "other linked_devices keys keep environment", config: "linked_devices:\n  diagnostics: true", env: envURL, want: envURL},
		{name: "built-in default", want: DefaultLinkedDevicesBrokerURL},
		{name: "empty YAML uses built-in default", config: "linked_devices:\n  broker_url: \"\"", want: DefaultLinkedDevicesBrokerURL},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configDir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", configDir)
			t.Setenv("TERM_LLM_LINKED_DEVICES_BROKER_URL", tt.env)
			t.Setenv("TERM_LLM_LINKED_DEVICES_ADMISSION", "")
			writeTestConfig(t, configDir, "default_provider: openai\n"+tt.config)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			if !cfg.LinkedDevicesEnabled() {
				t.Fatal("linked devices unavailable")
			}
			if got := cfg.LinkedDevicesBrokerURL(); got != tt.want {
				t.Fatalf("effective broker URL = %q, want %q", got, tt.want)
			}
			if err := cfg.ResolveLinkedDevices(); err != nil {
				t.Fatal(err)
			}
			if cfg.LinkedDevices.BrokerURL != tt.want {
				t.Fatalf("resolved broker URL = %q, want %q", cfg.LinkedDevices.BrokerURL, tt.want)
			}
		})
	}
}

func TestDefaultLinkedDevicesBrokerURLIsValid(t *testing.T) {
	if err := validateLinkedDevicesBrokerURL(DefaultLinkedDevicesBrokerURL); err != nil || !strings.HasPrefix(DefaultLinkedDevicesBrokerURL, "https://") {
		t.Fatalf("hosted default broker URL %q is invalid: %v", DefaultLinkedDevicesBrokerURL, err)
	}
}

func TestLinkedDevicesKeySpecsSensitive(t *testing.T) {
	specs := ConfigKeySpecs()

	var admissionFound, diagnosticsFound, iceServersFound bool
	for _, spec := range specs {
		switch spec.Path {
		case "linked_devices.admission_credential":
			admissionFound = true
			if !spec.Sensitive {
				t.Error("admission_credential should be marked sensitive")
			}
		case "linked_devices.diagnostics":
			diagnosticsFound = true
			if !spec.HasDefault || spec.Default != false || spec.Sensitive || spec.RedactExplicit {
				t.Errorf("diagnostics KeySpec = %+v", spec)
			}
		case "linked_devices.ice_servers":
			iceServersFound = true
			if !spec.RedactExplicit {
				t.Error("explicit ice_servers should be redacted")
			}
			if !spec.HasDefault {
				t.Error("ice_servers should expose its canonical default")
			}
		}
	}

	if !admissionFound {
		t.Error("admission_credential KeySpec not found")
	}
	if !diagnosticsFound {
		t.Error("diagnostics KeySpec not found")
	}
	if !iceServersFound {
		t.Error("ice_servers KeySpec not found")
	}
}

func TestLinkedDevicesBrokerURLValidationAndEnvironmentAdmission(t *testing.T) {
	for _, raw := range []string{"http://10.0.0.5:8091", "https://user@example.com", "https://example.com?query=1"} {
		cfg := &Config{LinkedDevices: LinkedDevicesConfig{BrokerURL: raw}}
		if err := cfg.ValidateLinkedDevices(); err == nil || !strings.Contains(err.Error(), "linked_devices.broker_url") {
			t.Fatalf("broker URL %q error = %v", raw, err)
		}
	}
	for _, raw := range []string{"https://broker.example.com/path", "http://127.0.0.1:8091", "http://localhost:8091"} {
		cfg := &Config{LinkedDevices: LinkedDevicesConfig{BrokerURL: raw}}
		if err := cfg.ValidateLinkedDevices(); err != nil {
			t.Fatalf("broker URL %q: %v", raw, err)
		}
	}
	t.Setenv("TERM_LLM_LINKED_DEVICES_BROKER_URL", "https://broker.example.com")
	t.Setenv("TERM_LLM_LINKED_DEVICES_ADMISSION", "short")
	cfg := &Config{}
	if err := cfg.ResolveLinkedDevices(); err == nil || !strings.Contains(err.Error(), "admission_credential") {
		t.Fatalf("short environment admission error = %v", err)
	}
}

func TestLinkedDevicesConfigFormattingRedactsSecrets(t *testing.T) {
	admission := strings.Repeat("a", 32)
	username, credential := "static-user", "static-password"
	cfg := LinkedDevicesConfig{
		BrokerURL: "https://broker.example.com", AdmissionCredential: admission,
		ICEServers: []LinkedDevicesICEServerConfig{{
			URLs: []string{"turns:relay.example.com:5349"}, CredentialSource: "static", Username: username, Credential: credential,
		}},
	}
	formatted := fmt.Sprintf("%v %#v", cfg, cfg)
	for _, secret := range []string{admission, username, credential} {
		if strings.Contains(formatted, secret) {
			t.Fatalf("formatted linked-device config leaked %q: %s", secret, formatted)
		}
	}
}

func TestLinkedDevicesDefaultsRenderValidYAML(t *testing.T) {
	defaults := DefaultLinkedDevicesICEServers()

	data, err := yaml.Marshal(defaults)
	if err != nil {
		t.Fatalf("failed to marshal defaults to YAML: %v", err)
	}

	var parsed []LinkedDevicesICEServerConfig
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal YAML: %v", err)
	}

	if len(parsed) != len(defaults) {
		t.Errorf("round-trip changed length: %d -> %d", len(defaults), len(parsed))
	}
}

func TestLinkedDevicesNoResolutionWhenUnused(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)

	// Set up an env var that would fail resolution if accessed
	t.Setenv("NONEXISTENT_COMMAND", "nonexistent")

	writeTestConfig(t, configDir, `
default_provider: openai
linked_devices:
  broker_url: ""
  admission_credential: $(nonexistent_command)
`)

	// Load should succeed without trying to resolve admission_credential
	// because broker_url is empty (feature disabled)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed when feature unused: %v", err)
	}

	// Verify the unresolved value is still there
	if !strings.HasPrefix(cfg.LinkedDevices.AdmissionCredential, "$") {
		t.Error("admission_credential was resolved even though broker_url is empty")
	}
}

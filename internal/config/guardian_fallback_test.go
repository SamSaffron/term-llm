package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestGuardianFallbackEnabledAndValidate(t *testing.T) {
	tests := []struct {
		name     string
		fallback GuardianFallbackConfig
		backend  string
		enabled  bool
		wantErr  string
	}{
		{name: "unset", backend: "", enabled: false},
		{name: "unset on classify", backend: "classify", enabled: false},
		{name: "blank values are unset", fallback: GuardianFallbackConfig{Provider: "  ", Model: "\t"}, backend: "classify", enabled: false},
		{name: "provider only", fallback: GuardianFallbackConfig{Provider: "chatgpt"}, backend: "classify", enabled: true},
		{name: "model only", fallback: GuardianFallbackConfig{Model: "gpt-5.6-luna-low"}, backend: " classify ", enabled: true},
		{name: "provider with logging", fallback: GuardianFallbackConfig{Provider: "chatgpt", LogPath: "~/.cache/guardian.jsonl"}, backend: "classify", enabled: true},
		{name: "logging off is still configured", fallback: GuardianFallbackConfig{Provider: "chatgpt", LogPath: "off"}, backend: "classify", enabled: true},
		{name: "logging OFF is case-insensitive", fallback: GuardianFallbackConfig{Provider: "chatgpt", LogPath: " OFF "}, backend: "classify", enabled: true},
		{name: "absolute log path", fallback: GuardianFallbackConfig{Provider: "chatgpt", LogPath: "/var/tmp/escalations.jsonl"}, backend: "classify", enabled: true},
		{name: "relative log path is rejected", fallback: GuardianFallbackConfig{Provider: "chatgpt", LogPath: "logs/escalations.jsonl"}, backend: "classify", enabled: true, wantErr: `guardian.fallback.log_path must be absolute, start with ~, or be off (got "logs/escalations.jsonl")`},
		{name: "bare file name is rejected", fallback: GuardianFallbackConfig{Model: "gpt-5.6-luna-low", LogPath: "escalations.jsonl"}, backend: "classify", enabled: true, wantErr: `guardian.fallback.log_path must be absolute, start with ~, or be off (got "escalations.jsonl")`},
		{name: "log path only", fallback: GuardianFallbackConfig{LogPath: "/tmp/escalations.jsonl"}, backend: "classify", wantErr: "guardian.fallback.log_path requires guardian.fallback.provider or guardian.fallback.model"},
		{name: "log path only on llm", fallback: GuardianFallbackConfig{LogPath: "/tmp/escalations.jsonl"}, backend: "llm", wantErr: "guardian.fallback.log_path requires guardian.fallback.provider or guardian.fallback.model"},
		{name: "provider on default backend", fallback: GuardianFallbackConfig{Provider: "chatgpt"}, backend: "", enabled: true, wantErr: "guardian.fallback requires guardian.backend: classify"},
		{name: "provider on llm backend", fallback: GuardianFallbackConfig{Model: "gpt-5.6-luna-low"}, backend: "llm", enabled: true, wantErr: "guardian.fallback requires guardian.backend: classify"},
		{name: "logging on llm backend", fallback: GuardianFallbackConfig{Provider: "chatgpt", LogPath: "off"}, backend: "llm", enabled: true, wantErr: "guardian.fallback requires guardian.backend: classify"},
		// Unknown backend values are rejected by the runtime's backend switch.
		{name: "unknown backend", fallback: GuardianFallbackConfig{Provider: "chatgpt"}, backend: "unknown", enabled: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fallback.Enabled(); got != tc.enabled {
				t.Fatalf("Enabled() = %t, want %t", got, tc.enabled)
			}
			err := tc.fallback.Validate(tc.backend)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate(%q) = %v", tc.backend, err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Validate(%q) = %v, want %q", tc.backend, err, tc.wantErr)
			}
		})
	}
}

func TestGuardianFallbackSchemaKeys(t *testing.T) {
	for _, key := range []string{"guardian.fallback.provider", "guardian.fallback.model", "guardian.fallback.log_path"} {
		if !IsKnownKey(key) {
			t.Fatalf("KnownKeys missing %s", key)
		}
		if _, ok := GetDefaults()[key]; ok {
			t.Fatalf("%s must not carry a persisted default", key)
		}
	}
	found := false
	for _, spec := range ConfigKeySpecs() {
		if spec.Path == "guardian.fallback.log_path" {
			found = true
		}
	}
	if !found {
		t.Fatal("guardian.fallback.log_path is missing from the config schema")
	}
}

func TestGuardianFallbackLoadsFromYAML(t *testing.T) {
	configHome := t.TempDir()
	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := "guardian:\n  backend: classify\n  fallback:\n    provider: chatgpt\n    model: gpt-5.6-luna-low\n    log_path: /tmp/guardian-escalations.jsonl\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	viper.Reset()
	t.Cleanup(viper.Reset)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := GuardianFallbackConfig{Provider: "chatgpt", Model: "gpt-5.6-luna-low", LogPath: "/tmp/guardian-escalations.jsonl"}
	if cfg.Guardian.Fallback != want || !cfg.Guardian.Fallback.Enabled() {
		t.Fatalf("guardian.fallback = %+v, want %+v", cfg.Guardian.Fallback, want)
	}
}

func TestGuardianFallbackLoadRejectsMisconfiguration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents string
		wantErr  string
	}{
		{
			name:     "log path without reviewer",
			contents: "guardian:\n  backend: classify\n  fallback:\n    log_path: /tmp/escalations.jsonl\n",
			wantErr:  "guardian.fallback.log_path requires guardian.fallback.provider or guardian.fallback.model",
		},
		{
			name:     "fallback on llm backend",
			contents: "guardian:\n  fallback:\n    provider: chatgpt\n    model: gpt-5.6-luna-low\n",
			wantErr:  "guardian.fallback requires guardian.backend: classify",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configHome := t.TempDir()
			configDir := filepath.Join(configHome, "term-llm")
			if err := os.MkdirAll(configDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(tc.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_CONFIG_HOME", configHome)
			viper.Reset()
			t.Cleanup(viper.Reset)

			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load() = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestGuardianEscalationLogPath(t *testing.T) {
	t.Run("xdg data home", func(t *testing.T) {
		dataHome := t.TempDir()
		t.Setenv("XDG_DATA_HOME", dataHome)
		got, err := GuardianEscalationLogPath()
		want := filepath.Join(dataHome, "term-llm", "guardian", "escalations.jsonl")
		if err != nil || got != want {
			t.Fatalf("path = %q (err %v), want %q", got, err, want)
		}
	})
	t.Run("home fallback", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", home)
		got, err := GuardianEscalationLogPath()
		want := filepath.Join(home, ".local", "share", "term-llm", "guardian", "escalations.jsonl")
		if err != nil || got != want {
			t.Fatalf("path = %q (err %v), want %q", got, err, want)
		}
	})
	t.Run("relative xdg data home is ignored", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("XDG_DATA_HOME", filepath.Join("relative", "data"))
		t.Setenv("HOME", home)
		got, err := GuardianEscalationLogPath()
		want := filepath.Join(home, ".local", "share", "term-llm", "guardian", "escalations.jsonl")
		if err != nil || got != want {
			t.Fatalf("path = %q (err %v), want %q", got, err, want)
		}
	})
	t.Run("unresolvable home never uses the working directory", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "")
		if _, err := os.UserHomeDir(); err == nil {
			t.Skip("platform resolves a home directory without HOME")
		}
		got, err := GuardianEscalationLogPath()
		if err == nil || got != "" {
			t.Fatalf("path = %q (err %v), want an error", got, err)
		}
	})
}

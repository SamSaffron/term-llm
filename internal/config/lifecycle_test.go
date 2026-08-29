package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestLifecycleConfigDefaultsAndLoad(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	viper.Reset()
	t.Cleanup(viper.Reset)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Lifecycle.Enabled || len(cfg.Lifecycle.Adapters) != 1 || cfg.Lifecycle.Adapters[0] != "auto" || cfg.Lifecycle.OSC != "off" || len(cfg.Lifecycle.Commands) != 0 {
		t.Fatalf("lifecycle defaults = %#v", cfg.Lifecycle)
	}

	configDir, _ := GetConfigDir()
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := `lifecycle:
  enabled: true
  adapters: [herdr, cmux]
  osc: auto
  commands:
    - name: tmux
      command: [/usr/local/bin/term-llm-tmux-bridge, --pane, "#{pane_id}"]
      timeout: 750ms
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	viper.Reset()
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Lifecycle.Commands) != 1 || cfg.Lifecycle.Commands[0].Name != "tmux" || cfg.Lifecycle.Commands[0].Command[1] != "--pane" || cfg.Lifecycle.Commands[0].Timeout != "750ms" {
		t.Fatalf("loaded lifecycle config = %#v", cfg.Lifecycle)
	}

	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("lifecycle:\n  enabled: true\n  adapters: []\n  osc: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	viper.Reset()
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Lifecycle.Adapters) != 0 {
		t.Fatalf("explicit empty lifecycle.adapters = %#v, want empty", cfg.Lifecycle.Adapters)
	}
}

func TestValidateLifecycle(t *testing.T) {
	tests := []struct {
		name string
		cfg  LifecycleConfig
		want string
	}{
		{name: "valid defaults", cfg: LifecycleConfig{Enabled: true, Adapters: []string{"auto"}, OSC: "off"}},
		{name: "valid allowlist and sink", cfg: LifecycleConfig{Enabled: true, Adapters: []string{"herdr", "cmux"}, OSC: "on", Commands: []LifecycleCommandConfig{{Name: "zellij", Command: []string{"/bin/bridge", "--literal;arg"}, Timeout: "1s"}}}},
		{name: "unknown adapter", cfg: LifecycleConfig{Adapters: []string{"tmux"}, OSC: "off"}, want: "expected auto, herdr, or cmux"},
		{name: "auto mixed", cfg: LifecycleConfig{Adapters: []string{"auto", "cmux"}, OSC: "off"}, want: "auto cannot be combined"},
		{name: "bad osc", cfg: LifecycleConfig{Adapters: []string{"auto"}, OSC: "sometimes"}, want: "expected off, auto, or on"},
		{name: "empty command", cfg: LifecycleConfig{Adapters: []string{"auto"}, OSC: "off", Commands: []LifecycleCommandConfig{{Name: "tmux"}}}, want: "executable cannot be empty"},
		{name: "blank executable", cfg: LifecycleConfig{Adapters: []string{"auto"}, OSC: "off", Commands: []LifecycleCommandConfig{{Name: "tmux", Command: []string{" "}}}}, want: "executable cannot be empty"},
		{name: "reserved name", cfg: LifecycleConfig{Adapters: []string{"auto"}, OSC: "off", Commands: []LifecycleCommandConfig{{Name: "herdr", Command: []string{"bridge"}}}}, want: "reserved name"},
		{name: "duplicate sink", cfg: LifecycleConfig{Adapters: []string{"auto"}, OSC: "off", Commands: []LifecycleCommandConfig{{Name: "Tmux", Command: []string{"a"}}, {Name: "tmux", Command: []string{"b"}}}}, want: "duplicate name"},
		{name: "bad timeout", cfg: LifecycleConfig{Adapters: []string{"auto"}, OSC: "off", Commands: []LifecycleCommandConfig{{Name: "tmux", Command: []string{"bridge"}, Timeout: "forever"}}}, want: "expected a duration"},
		{name: "long timeout", cfg: LifecycleConfig{Adapters: []string{"auto"}, OSC: "off", Commands: []LifecycleCommandConfig{{Name: "tmux", Command: []string{"bridge"}, Timeout: "31s"}}}, want: "at most 30s"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (&Config{Lifecycle: test.cfg}).ValidateLifecycle()
			if test.want == "" && err != nil {
				t.Fatalf("ValidateLifecycle() error = %v", err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("ValidateLifecycle() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestLifecycleKeysAppearInSchemaDefaults(t *testing.T) {
	defaults := GetDefaults()
	for key, want := range map[string]any{
		"lifecycle.enabled": true,
		"lifecycle.osc":     "off",
	} {
		if got := defaults[key]; got != want {
			t.Fatalf("default %s = %#v, want %#v", key, got, want)
		}
	}
	if !IsKnownKey("lifecycle.commands") {
		t.Fatal("lifecycle.commands is not a known config key")
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestShareConfigDefaultsAndLoad(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	viper.Reset()
	t.Cleanup(viper.Reset)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Share.Provider != DefaultShareProvider || cfg.Share.Timeout != DefaultShareTimeout || len(cfg.Share.Command) != 0 {
		t.Fatalf("default share config=%+v", cfg.Share)
	}

	configDir, err := GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte("share:\n  provider: command\n  command: [/opt/share-helper, --tenant, demo]\n  timeout: 45s\n")
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	viper.Reset()
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Share.Provider != "command" || cfg.Share.Timeout != "45s" || strings.Join(cfg.Share.Command, "|") != "/opt/share-helper|--tenant|demo" {
		t.Fatalf("loaded share config=%+v", cfg.Share)
	}
}

func TestValidateShare(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  ShareConfig
		want string
	}{
		{name: "github", cfg: ShareConfig{Provider: "github", Timeout: "120s"}},
		{name: "command", cfg: ShareConfig{Provider: "command", Command: []string{"helper", "--prefix"}, Timeout: "10s"}},
		{name: "unknown provider", cfg: ShareConfig{Provider: "other"}, want: "expected github or command"},
		{name: "command missing argv", cfg: ShareConfig{Provider: "command"}, want: "executable cannot be empty"},
		{name: "github ignored command", cfg: ShareConfig{Provider: "github", Command: []string{"helper"}}, want: "only valid"},
		{name: "nul argument", cfg: ShareConfig{Provider: "command", Command: []string{"helper", "bad\x00arg"}}, want: "contains NUL"},
		{name: "invalid timeout", cfg: ShareConfig{Provider: "github", Timeout: "forever"}, want: "greater than zero"},
		{name: "long timeout", cfg: ShareConfig{Provider: "github", Timeout: "601s"}, want: "at most 600s"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := (&Config{Share: test.cfg}).ValidateShare()
			if test.want == "" && err != nil {
				t.Fatalf("ValidateShare error=%v", err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("ValidateShare error=%v, want containing %q", err, test.want)
			}
		})
	}
}

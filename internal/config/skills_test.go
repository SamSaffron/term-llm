package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectSkillScriptToolsDefaultsAndLoad(t *testing.T) {
	const key = "skills.allow_project_script_tools"
	if !IsKnownKey(key) || GetDefaults()[key] != false {
		t.Fatalf("%s must be a known key with default false", key)
	}
	for _, tc := range []struct {
		name, content string
		want          bool
	}{
		{"omitted", "skills: {}\n", false},
		{"enabled", "skills:\n  allow_project_script_tools: true\n", true},
		{"disabled", "skills:\n  allow_project_script_tools: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			path, err := GetConfigPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Skills.AllowProjectScriptTools != tc.want {
				t.Fatalf("AllowProjectScriptTools = %t, want %t", cfg.Skills.AllowProjectScriptTools, tc.want)
			}
			// An unrelated config edit must preserve this security setting.
			if _, err := SetAgentPreference("developer", "provider", "debug"); err != nil {
				t.Fatal(err)
			}
			roundTrip, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if roundTrip.Skills.AllowProjectScriptTools != tc.want {
				t.Fatalf("round-trip AllowProjectScriptTools = %t, want %t", roundTrip.Skills.AllowProjectScriptTools, tc.want)
			}
		})
	}
}

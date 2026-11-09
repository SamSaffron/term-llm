package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPPolicyNullShapesAreErrors(t *testing.T) {
	for _, body := range []string{
		"mcp: null", "mcp:", "mcp: {servers: null}", "mcp: {servers: {db: null}}",
		"mcp: {servers: {db: {agents: null}}}", "mcp:\n  servers:\n    db:\n      agents:\n        # - last-agent\n",
	} {
		t.Run(body, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)
			writeTestConfig(t, home, body)
			_, err := LoadMCPPolicy()
			if err == nil || !strings.Contains(err.Error(), filepath.Join(home, "term-llm", "config.yaml")) {
				t.Fatalf("null policy accepted/missing path: %v", err)
			}
			if strings.Contains(body, "agents") && !strings.Contains(err.Error(), "use [] to deny everyone or remove the key") {
				t.Fatalf("no remediation: %v", err)
			}
			if _, err := loadUnresolved(); err == nil {
				t.Fatal("startup accepted null policy")
			}
		})
	}
}

func TestMCPPolicyRejectsCaseVariantDuplicateSections(t *testing.T) {
	for _, body := range []string{
		"mcp: {servers: {db: {agents: []}}}\nMCP: {}\n",
		"Mcp: {}\nmCP: {servers: {db: {agents: [trusted]}}}\n",
	} {
		t.Run(body, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)
			writeTestConfig(t, home, body)
			for i := 0; i < 5; i++ {
				if _, err := LoadMCPPolicy(); err == nil || !strings.Contains(err.Error(), "duplicate case-variant mcp") {
					t.Fatalf("ambiguous policy accepted: %v", err)
				}
			}
		})
	}
}

func TestMCPPolicyDecodeSettingsErrorsIncludePath(t *testing.T) {
	const path = "test-config-path.yaml"
	for _, value := range []any{nil, []any{}, map[string]any{"servers": nil}, map[string]any{"servers": map[string]any{"db": map[string]any{"agents": "bad"}}}} {
		var cfg Config
		if err := decodeSettings(map[string]any{"mcp": value}, &cfg, path); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("decodeSettings path missing: %v", err)
		}
	}
}

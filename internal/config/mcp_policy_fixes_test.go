package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMCPPolicySavePreservesWizardPolicy(t *testing.T) {
	for _, emptyMap := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil", true: "empty"}[emptyMap], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)
			writeTestConfig(t, home, "mcp: {servers: {Prod.DB: {agents: []}, allowed: {agents: [migration-data]}, open: {}}}")
			before, err := LoadMCPPolicy()
			if err != nil {
				t.Fatal(err)
			}
			cfg := &Config{}
			if emptyMap {
				cfg.MCP.Servers = map[string]MCPServerPolicy{}
			}
			if err := Save(cfg); err != nil {
				t.Fatal(err)
			}
			after, err := LoadMCPPolicy()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("wizard erased policy: %#v -> %#v", before, after)
			}
		})
	}
}

func TestMCPPolicySaveRejectsCorruptExistingConfig(t *testing.T) {
	for _, body := range []string{"mcp: [invalid", "mcp: {servers: {db: {agent: [trusted]}}}", "mcp: {servers: {db: {agents: scalar}}}"} {
		t.Run(body, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)
			writeTestConfig(t, home, body)
			if err := Save(&Config{MCP: MCPConfig{Servers: map[string]MCPServerPolicy{}}}); err == nil {
				t.Fatal("wizard overwrote corrupt policy")
			}
			data, err := os.ReadFile(filepath.Join(home, "term-llm", "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != body {
				t.Fatalf("failed save changed config: %s", data)
			}
		})
	}
}

func TestMCPPolicySaveOmitsEmptySection(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Save(&Config{}); err != nil {
		t.Fatal(err)
	}
	path, err := GetConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "mcp:") {
		t.Fatalf("stray empty policy: %s", body)
	}
}

func TestMCPPolicyKnownKeys(t *testing.T) {
	for _, key := range []string{"mcp", "mcp.servers", "mcp.servers.foo", "mcp.servers.foo.agents"} {
		if !IsKnownKey(key) {
			t.Errorf("valid key rejected: %s", key)
		}
	}
	for _, key := range []string{"mcp.foo", "mcp.servers.", "mcp.servers.foo.agent", "mcp.servers.Prod.DB", "mcp.servers.Prod.DB.agents", "mcp.servers.foo.agents.extra"} {
		if IsKnownKey(key) {
			t.Errorf("invalid key accepted: %s", key)
		}
	}
}

func TestMCPPolicyDecodeErrorsIncludeConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeTestConfig(t, home, "mcp: {servers: {db: {agent: [trusted]}}}")
	path := filepath.Join(home, "term-llm", "config.yaml")
	_, loadErr := loadUnresolved()
	_, policyErr := LoadMCPPolicy()
	for _, err := range []error{loadErr, policyErr} {
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("decode error missing path %s: %v", path, err)
		}
	}
}

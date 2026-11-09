package config

import (
	"reflect"
	"testing"
)

func TestMCPPolicyDecode(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		bad        bool
		agents     []string
	}{
		{name: "absent", body: "{}"},
		{name: "missing agents", body: "mcp: {servers: {Prod.DB: {}}}"},
		{name: "null", body: "mcp: {servers: {Prod.DB: {agents: null}}}", bad: true},
		{name: "deny all", body: "mcp: {servers: {Prod.DB: {agents: []}}}", agents: []string{}},
		{name: "allow", body: "mcp: {servers: {Prod.DB: {agents: [migration-data]}}}", agents: []string{"migration-data"}},
		{name: "blank", body: "mcp: {servers: {Prod.DB: {agents: [' ']}}}", bad: true},
		{name: "empty", body: "mcp: {servers: {Prod.DB: {agents: ['']}}}", bad: true},
		{name: "number", body: "mcp: {servers: {Prod.DB: {agents: [42]}}}", bad: true},
		{name: "bool", body: "mcp: {servers: {Prod.DB: {agents: [true]}}}", bad: true},
		{name: "scalar", body: "mcp: {servers: {Prod.DB: {agents: migration-data}}}", bad: true},
		{name: "typo", body: "mcp: {servers: {Prod.DB: {agent: [migration-data]}}}", bad: true},
		{name: "null server", body: "mcp: {servers: {Prod.DB: null}}", bad: true},
		{name: "bad servers", body: "mcp: {servers: []}", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)
			writeTestConfig(t, home, tc.body)
			cfg, err := loadUnresolved()
			policy, policyErr := LoadMCPPolicy()
			if tc.bad {
				if err == nil || policyErr == nil {
					t.Fatalf("bad policy accepted: load=%v policy=%v", err, policyErr)
				}
				return
			}
			if err != nil || policyErr != nil {
				t.Fatalf("load=%v policy=%v", err, policyErr)
			}
			if !reflect.DeepEqual(cfg.MCP.Servers["Prod.DB"].Agents, tc.agents) {
				t.Fatalf("agents = %#v, want %#v", cfg.MCP.Servers["Prod.DB"].Agents, tc.agents)
			}
			if !reflect.DeepEqual(cfg.MCP, policy) {
				t.Fatalf("startup/reload mismatch: %#v / %#v", cfg.MCP, policy)
			}
			for _, id := range []string{"", "migration-data", "other"} {
				want := tc.agents == nil || (len(tc.agents) > 0 && id == "migration-data")
				if policy.Allows("Prod.DB", id) != want {
					t.Errorf("Allows(%q) != %v", id, want)
				}
			}
		})
	}
}

func TestMCPPolicyOpaqueServerNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeTestConfig(t, home, "mcp:\n  servers:\n    Prod.DB:\n      agents: [migration-data]\n    prod.db:\n      agents: []\n")
	cfg, err := loadUnresolved()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCP.Servers) != 2 || !cfg.MCP.Allows("Prod.DB", "migration-data") || cfg.MCP.Allows("prod.db", "migration-data") || !cfg.MCP.Allows("PROD.DB", "") {
		t.Fatalf("lost opaque names: %#v", cfg.MCP)
	}
}

func TestMCPPolicySaveRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeTestConfig(t, home, "mcp: {servers: {Prod.DB: {agents: []}, allowed: {agents: [migration-data]}, open: {}}}")
	cfg, err := loadUnresolved()
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := loadUnresolved()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.MCP, got.MCP) {
		t.Fatalf("round trip changed policy: %#v -> %#v", cfg.MCP, got.MCP)
	}
	for _, key := range []string{"mcp", "mcp.servers", "mcp.servers.prod", "mcp.servers.prod.agents"} {
		if !IsKnownKey(key) {
			t.Errorf("unknown policy key %s", key)
		}
	}
}

package mcp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAgentScope(t *testing.T) {
	for _, tc := range []struct {
		name, agent string
		agents      []string
		allowed     bool
	}{
		{"unrestricted plain", "", nil, true},
		{"empty unrestricted", "other", []string{}, true},
		{"allowed", "migration-data", []string{"migration-data"}, true},
		{"other", "developer", []string{"migration-data"}, false},
		{"plain", "", []string{"migration-data"}, false},
		{"exact", "Migration-data", []string{"migration-data"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Servers: map[string]ServerConfig{"db": {Command: "must-not-start", Agents: tc.agents}}}
			manager := NewManagerWithConfigForAgent(cfg, tc.agent)
			defer manager.StopAll()
			err := manager.CheckSelection([]string{"db"})
			if (err == nil) != tc.allowed {
				t.Fatalf("CheckSelection = %v", err)
			}
			if !tc.allowed {
				var denied *AgentAccessError
				if err := manager.Enable(t.Context(), "db"); !errors.As(err, &denied) {
					t.Fatalf("Enable = %v", err)
				}
				if len(manager.AllTools()) != 0 || len(manager.EnabledServers()) != 0 {
					t.Fatal("denied server published state")
				}
				if len(manager.FilterSelection([]string{"db"})) != 0 {
					t.Fatal("denied selection retained")
				}
			}
		})
	}
}

func TestAgentScopeConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{`["migration-data"]`, true}, {`[]`, true}, {`null`, true},
		{`[""]`, false}, {`[" "]`, false}, {`[null]`, false}, {`[1]`, false}, {`"migration-data"`, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mcp.json")
			if err := os.WriteFile(path, []byte(`{"servers":{"db":{"command":"db","agents":`+tc.value+`}}}`), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfigFromPath(path)
			if (err == nil) != tc.valid {
				t.Fatalf("LoadConfig = %v", err)
			}
			if !tc.valid && !strings.Contains(err.Error(), "agents") {
				t.Fatalf("unclear error: %v", err)
			}
		})
	}
	if err := (&ServerConfig{Command: "db", Agents: []string{""}}).Validate(); err == nil {
		t.Fatal("Validate accepted empty agent")
	}
}

func TestAgentScopeConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	original := `{"future":{"keep":true},"servers":{"db":{"command":"db","agents":["migration-data"],"extension":{"keep":true}},"remove":{"command":"x"}}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateConfigAtPath(path, func(c *Config) error {
		c.RemoveServer("remove")
		c.AddServer("new", ServerConfig{Command: "new"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Servers["db"].Agents, []string{"migration-data"}) {
		t.Fatalf("agents lost: %+v", cfg)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["future"] == nil || !strings.Contains(string(data), `"extension"`) {
		t.Fatalf("extensions lost: %s", data)
	}
	copy := NewManagerWithConfig(cfg).Config()
	server := copy.Servers["db"]
	server.Agents[0] = "other"
	if cfg.Servers["db"].Agents[0] != "migration-data" {
		t.Fatal("Config aliases agents")
	}
}

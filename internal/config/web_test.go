package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWebAgentDefaults(t *testing.T) {
	defaults := GetDefaults()
	if got := defaults["web.project_default_agent"]; got != DefaultWebProjectDefaultAgent {
		t.Fatalf("web.project_default_agent default = %#v, want %q", got, DefaultWebProjectDefaultAgent)
	}
	if got := defaults["web.agent_picker.common"]; !reflect.DeepEqual(got, DefaultWebAgentPickerCommon()) {
		t.Fatalf("web.agent_picker.common default = %#v, want %#v", got, DefaultWebAgentPickerCommon())
	}
	for _, key := range []string{"web", "web.project_default_agent", "web.agent_picker", "web.agent_picker.common"} {
		if !IsKnownKey(key) {
			t.Errorf("%s is not a known config key", key)
		}
	}
}

func TestWebConfigAbsentCommonUsesStockList(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	writeTestConfig(t, configHome, "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Web.ProjectDefaultAgent != DefaultWebProjectDefaultAgent {
		t.Fatalf("project default = %q", cfg.Web.ProjectDefaultAgent)
	}
	if got := cfg.Web.AgentPicker.CommonAgents(); !reflect.DeepEqual(got, DefaultWebAgentPickerCommon()) {
		t.Fatalf("common = %#v, want stock %#v", got, DefaultWebAgentPickerCommon())
	}
	if cfg.Web.AgentPicker.Common == nil {
		t.Fatal("an absent key must decode the merged stock default, not nil")
	}
}

func TestWebConfigExplicitCommonReplacesStockList(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	writeTestConfig(t, configHome, "web:\n  agent_picker:\n    common: [custom-a, custom-b]\n")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Web.AgentPicker.CommonAgents(); !reflect.DeepEqual(got, []string{"custom-a", "custom-b"}) {
		t.Fatalf("common = %#v", got)
	}
}

func TestWebConfigEmptyCommonDisablesSplit(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	writeTestConfig(t, configHome, "web:\n  agent_picker:\n    common: []\n")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Web.AgentPicker.Common == nil {
		t.Fatal("an explicit [] must stay explicit, not fall back to the stock list")
	}
	if got := cfg.Web.AgentPicker.CommonAgents(); len(got) != 0 {
		t.Fatalf("common = %#v, want empty", got)
	}
	if data, err := yaml.Marshal(cfg.Web.AgentPicker); err != nil || !strings.Contains(string(data), "common: []") {
		t.Fatalf("explicit empty roundtrip = %q, %v", data, err)
	}
}

func TestWebConfigProjectDefaultAgentLoads(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	writeTestConfig(t, configHome, "web:\n  project_default_agent: planner\n")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Web.ProjectDefaultAgent != "planner" {
		t.Fatalf("project default = %q", cfg.Web.ProjectDefaultAgent)
	}
}

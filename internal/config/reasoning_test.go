package config

import "testing"

func TestResolveReasoningServeDefaultsAndOverrides(t *testing.T) {
	t.Setenv("TERM_LLM_SHOW_RAW_REASONING", "")
	for _, tt := range []struct {
		name string
		cfg  *Config
		want string
	}{
		{"nil", nil, ReasoningDisplayOff},
		{"zero", &Config{}, ReasoningDisplayOff},
		{"defaults", &Config{Reasoning: DefaultReasoningConfig()}, ReasoningDisplayOff},
		{"style only", &Config{Reasoning: ReasoningConfig{HiddenLabel: "Pondering..."}}, ReasoningDisplayOff},
		{"top level", &Config{Reasoning: ReasoningConfig{Display: ReasoningDisplayExpanded}}, ReasoningDisplayExpanded},
		{"explicit auto", &Config{Reasoning: ReasoningConfig{Display: ReasoningDisplayAuto}}, ReasoningDisplayAuto},
		{"serve override", &Config{Reasoning: ReasoningConfig{Display: ReasoningDisplayExpanded, Serve: ReasoningSurfaceConfig{Display: ReasoningDisplayCollapsed}}}, ReasoningDisplayCollapsed},
		{"explicit inherit", &Config{Reasoning: ReasoningConfig{Serve: ReasoningSurfaceConfig{Display: ReasoningInherit}}}, ReasoningDisplayAuto},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.ResolveReasoning("serve").Display; got != tt.want {
				t.Fatalf("serve display = %q, want %q", got, tt.want)
			}
		})
	}
	for _, cfg := range []*Config{nil, {}, {Reasoning: DefaultReasoningConfig()}} {
		for _, surface := range []string{"chat", "ask", "jobs"} {
			if got := cfg.ResolveReasoning(surface).Display; got != ReasoningDisplayAuto {
				t.Fatalf("%s display = %q, want auto", surface, got)
			}
		}
	}
}

func TestLoadReasoningServeDisplayInheritance(t *testing.T) {
	t.Setenv("TERM_LLM_SHOW_RAW_REASONING", "")
	for _, tt := range []struct {
		name  string
		yaml  string
		serve string
		chat  string
	}{
		{"defaults", "{}", ReasoningDisplayOff, ReasoningDisplayAuto},
		{"unrelated setting", "reasoning:\n  hidden_label: Pondering...\n", ReasoningDisplayOff, ReasoningDisplayAuto},
		{"top level collapsed", "reasoning:\n  display: collapsed\n", ReasoningDisplayCollapsed, ReasoningDisplayCollapsed},
		{"top level auto", "reasoning:\n  display: auto\n", ReasoningDisplayAuto, ReasoningDisplayAuto},
		{"serve collapsed", "reasoning:\n  serve:\n    display: collapsed\n", ReasoningDisplayCollapsed, ReasoningDisplayAuto},
		{"serve wins", "reasoning:\n  display: expanded\n  serve:\n    display: off\n", ReasoningDisplayOff, ReasoningDisplayExpanded},
		{"serve auto", "reasoning:\n  serve:\n    display: auto\n", ReasoningDisplayAuto, ReasoningDisplayAuto},
		{"serve inherit", "reasoning:\n  serve:\n    display: inherit\n", ReasoningDisplayAuto, ReasoningDisplayAuto},
		{"inherit top level", "reasoning:\n  display: expanded\n  serve:\n    display: inherit\n", ReasoningDisplayExpanded, ReasoningDisplayExpanded},
		{"serve status only", "reasoning:\n  serve:\n    status: none\n", ReasoningDisplayOff, ReasoningDisplayAuto},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)
			writeTestConfig(t, home, tt.yaml)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.ResolveReasoning("serve").Display; got != tt.serve {
				t.Errorf("serve display = %q, want %q", got, tt.serve)
			}
			if got := cfg.ResolveReasoning("chat").Display; got != tt.chat {
				t.Errorf("chat display = %q, want %q", got, tt.chat)
			}
		})
	}
}

func TestResolveReasoningServeRawEnvOverride(t *testing.T) {
	t.Setenv("TERM_LLM_SHOW_RAW_REASONING", "1")
	resolved := (*Config)(nil).ResolveReasoning("serve")
	if resolved.Display != ReasoningDisplayRaw || resolved.Source != ReasoningSourceAll || !resolved.Raw {
		t.Fatalf("raw override did not apply to serve: %+v", resolved)
	}
}

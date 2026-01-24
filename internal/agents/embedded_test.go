package agents

import (
	"testing"
)

func TestGetBuiltinAgents(t *testing.T) {
	agents := getBuiltinAgents()

	if len(agents) != len(builtinAgentNames) {
		t.Errorf("len(getBuiltinAgents()) = %d, want %d", len(agents), len(builtinAgentNames))
	}

	// Create map for easier lookup
	byName := make(map[string]*Agent)
	for _, a := range agents {
		byName[a.Name] = a
	}

	// Check each builtin exists
	for _, name := range builtinAgentNames {
		agent, ok := byName[name]
		if !ok {
			t.Errorf("missing builtin agent: %s", name)
			continue
		}
		if agent.Source != SourceBuiltin {
			t.Errorf("%s.Source = %v, want %v", name, agent.Source, SourceBuiltin)
		}
	}
}

func TestGetBuiltinAgent(t *testing.T) {
	for _, name := range builtinAgentNames {
		t.Run(name, func(t *testing.T) {
			agent, err := getBuiltinAgent(name)
			if err != nil {
				t.Fatalf("getBuiltinAgent(%s): %v", name, err)
			}

			if agent.Name != name {
				t.Errorf("Name = %q, want %q", agent.Name, name)
			}
			if agent.Source != SourceBuiltin {
				t.Errorf("Source = %v, want %v", agent.Source, SourceBuiltin)
			}
			if agent.Description == "" {
				t.Error("Description should not be empty")
			}
			if agent.SystemPrompt == "" {
				t.Error("SystemPrompt should not be empty")
			}
		})
	}

	// Non-existent builtin
	_, err := getBuiltinAgent("nonexistent")
	if err == nil {
		t.Error("getBuiltinAgent(nonexistent) should return error")
	}
}

func TestIsBuiltinAgent(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"active-review", true},
		{"agent-builder", true},
		{"artist", true},
		{"changelog", true},
		{"codebase", true},
		{"commit-message", true},
		{"developer", true},
		{"editor", true},
		{"file-organizer", true},
		{"researcher", true},
		{"reviewer", true},
		{"shell", true},
		{"nonexistent", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsBuiltinAgent(tt.name)
			if result != tt.expected {
				t.Errorf("IsBuiltinAgent(%q) = %v, want %v", tt.name, result, tt.expected)
			}
		})
	}
}

func TestBuiltinAgentConfigs(t *testing.T) {
	// Test specific builtin agent configurations
	tests := []struct {
		name               string
		expectedHasTools   bool
		expectedMaxTurns   int
		expectedHasShell   bool
		expectedAutoRun    bool
		expectedHasScripts bool
		expectedSearch     bool
	}{
		{"active-review", true, 50, false, false, false, false},
		{"agent-builder", true, 200, false, false, false, true},
		{"artist", true, 50, true, true, false, false},
		{"changelog", true, 0, true, true, false, false},
		{"codebase", true, 50, true, true, true, false},
		{"commit-message", true, 200, true, true, true, false},
		{"developer", true, 30, true, false, false, false},
		{"editor", true, 200, false, false, false, false},
		{"file-organizer", true, 100, true, true, false, false},
		{"researcher", true, 200, false, false, false, false},
		{"reviewer", true, 200, true, true, true, false},
		{"shell", true, 200, false, false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent, err := getBuiltinAgent(tt.name)
			if err != nil {
				t.Fatalf("getBuiltinAgent(%s): %v", tt.name, err)
			}

			hasTools := agent.HasEnabledList() || agent.HasDisabledList()
			if hasTools != tt.expectedHasTools {
				t.Errorf("hasTools = %v, want %v", hasTools, tt.expectedHasTools)
			}

			if agent.MaxTurns != tt.expectedMaxTurns {
				t.Errorf("MaxTurns = %d, want %d", agent.MaxTurns, tt.expectedMaxTurns)
			}

			hasShell := len(agent.Shell.Allow) > 0
			if hasShell != tt.expectedHasShell {
				t.Errorf("hasShell = %v, want %v", hasShell, tt.expectedHasShell)
			}

			if agent.Shell.AutoRun != tt.expectedAutoRun {
				t.Errorf("Shell.AutoRun = %v, want %v", agent.Shell.AutoRun, tt.expectedAutoRun)
			}

			hasScripts := len(agent.Shell.Scripts) > 0
			if hasScripts != tt.expectedHasScripts {
				t.Errorf("hasScripts = %v, want %v", hasScripts, tt.expectedHasScripts)
			}

			if agent.Search != tt.expectedSearch {
				t.Errorf("Search = %v, want %v", agent.Search, tt.expectedSearch)
			}
		})
	}
}

func TestGetBuiltinAgentNames(t *testing.T) {
	names := GetBuiltinAgentNames()

	if len(names) != 12 {
		t.Errorf("len(GetBuiltinAgentNames()) = %d, want 12", len(names))
	}

	expected := map[string]bool{
		"active-review":  true,
		"agent-builder":  true,
		"artist":         true,
		"changelog":      true,
		"codebase":       true,
		"commit-message": true,
		"developer":      true,
		"editor":         true,
		"file-organizer": true,
		"researcher":     true,
		"reviewer":       true,
		"shell":          true,
	}

	for _, name := range names {
		if !expected[name] {
			t.Errorf("unexpected builtin agent: %s", name)
		}
	}
}

package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilterInstructionsForModel(t *testing.T) {
	doc := strings.Join([]string{
		"# Project",
		"Always run tests.",
		"",
		"[[[claude-bin:opus]]]",
		"Do not say load-bearing.",
		"",
		"[[[chatgpt]]]",
		"Be terse.",
		"",
		"[[[all]]]",
		"Shared closing rule.",
	}, "\n")

	tests := []struct {
		name     string
		provider string
		model    string
		want     []string
		notWant  []string
	}{
		{
			name:     "matches provider and model prefix",
			provider: "claude-bin",
			model:    "opus-max",
			want:     []string{"Always run tests.", "Do not say load-bearing.", "Shared closing rule."},
			notWant:  []string{"Be terse.", "[[["},
		},
		{
			name:     "bare spec matches provider",
			provider: "chatgpt",
			model:    "gpt-5.6-sol-high",
			want:     []string{"Be terse.", "Shared closing rule."},
			notWant:  []string{"load-bearing"},
		},
		{
			name:     "non-matching model drops all gated blocks",
			provider: "openai",
			model:    "gpt-5.2",
			want:     []string{"Always run tests.", "Shared closing rule."},
			notWant:  []string{"load-bearing", "Be terse."},
		},
		{
			name:     "unknown model keeps everything",
			provider: "",
			model:    "",
			want:     []string{"Always run tests.", "Do not say load-bearing.", "Be terse.", "Shared closing rule."},
			notWant:  []string{"[[["},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FilterInstructionsForModel(doc, tc.provider, tc.model)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in:\n%s", want, got)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("unexpected %q in:\n%s", notWant, got)
				}
			}
		})
	}
}

func TestFilterInstructionsGateRunsToEndOfFile(t *testing.T) {
	// A gate is closed only by another marker. Horizontal rules and headings do
	// not close it, so everything after the marker stays scoped.
	doc := "[[[claude-bin:opus]]]\nopus only\n\n---\n\n## Heading\nstill opus only\n"

	got := FilterInstructionsForModel(doc, "chatgpt", "gpt-5.6")
	if strings.Contains(got, "opus only") || strings.Contains(got, "Heading") {
		t.Errorf("gated block leaked: %q", got)
	}

	got = FilterInstructionsForModel(doc, "claude-bin", "opus-max")
	if !strings.Contains(got, "still opus only") {
		t.Errorf("gated block truncated for matching model: %q", got)
	}
}

func TestFilterInstructionsMixedNegationIsAndNot(t *testing.T) {
	// A veto wins over a matching positive spec.
	doc := "[[[claude-bin, !opus]]]\nclaude but not opus\n"

	if got := FilterInstructionsForModel(doc, "claude-bin", "opus-max"); strings.Contains(got, "claude but not opus") {
		t.Errorf("negation did not veto matching positive: %q", got)
	}
	if got := FilterInstructionsForModel(doc, "claude-bin", "fable-medium"); !strings.Contains(got, "claude but not opus") {
		t.Errorf("block dropped for non-vetoed model: %q", got)
	}
}

func TestFilterInstructionsPipeSeparator(t *testing.T) {
	doc := "[[[opus | sol]]]\nlisted rule\n"
	if got := FilterInstructionsForModel(doc, "chatgpt", "sol-high"); !strings.Contains(got, "listed rule") {
		t.Errorf("pipe separator not honoured: %q", got)
	}
	if got := FilterInstructionsForModel(doc, "chatgpt", "gpt-6-astra"); strings.Contains(got, "listed rule") {
		t.Errorf("unmatched pipe list kept: %q", got)
	}
}

func TestFilterInstructionsNegationAndLists(t *testing.T) {
	doc := "[[[!opus]]]\nnon-opus rule\n[[[opus, sol]]]\nlisted rule\n"

	got := FilterInstructionsForModel(doc, "claude-bin", "opus-max")
	if strings.Contains(got, "non-opus rule") {
		t.Errorf("negated block kept: %q", got)
	}
	if !strings.Contains(got, "listed rule") {
		t.Errorf("listed spec dropped: %q", got)
	}

	got = FilterInstructionsForModel(doc, "chatgpt", "gpt-6-astra")
	if !strings.Contains(got, "non-opus rule") {
		t.Errorf("negated block dropped for other model: %q", got)
	}
	if strings.Contains(got, "listed rule") {
		t.Errorf("listed spec kept for other model: %q", got)
	}
}

// TestFilterInstructionsSpecMatching pins matching against provider keys and
// model IDs this repo actually ships. Loose substring matching would make
// suffix tokens like "bin", "max", "high", or a version digit act as
// accidental wildcards across unrelated providers.
func TestFilterInstructionsSpecMatching(t *testing.T) {
	tests := []struct {
		spec     string
		provider string
		model    string
		want     bool
	}{
		// Prefix matching ending on a separator is the headline case.
		{"opus", "claude-bin", "opus-max", true},
		{"opus", "claude-bin", "opus", true},
		{"claude-bin:opus", "claude-bin", "opus-max", true},
		{"claude-bin", "claude-bin", "opus-max", true},
		{"chatgpt", "chatgpt", "gpt-5.6-sol-high", true},
		{"gpt-5.6-sol-high", "chatgpt", "gpt-5.6-sol-high", true},

		// Not a prefix, or not ending on a separator.
		{"opus", "chatgpt", "opusculum", false},
		{"opus", "chatgpt", "opus2", false},
		{"opus", "anthropic", "claude-opus-5", false},

		// Suffix tokens must not become wildcards across providers.
		{"bin", "claude-bin", "opus-max", false},
		{"go", "opencode-go", "glm-5.3-max", false},
		{"max", "claude-bin", "opus-max", false},
		{"high", "cursor-bin", "grok-4.6-high", false},
		{"5", "chatgpt", "gpt-5.6-sol-high", false},
		{"6", "chatgpt", "gpt-6-astra-medium", false},

		// Explicit wildcards opt into broader matching.
		{"*opus*", "anthropic", "claude-opus-5", true},
		{"*-bin", "claude-bin", "opus-max", true},
		{"*high", "cursor-bin", "grok-4.6-high", true},
		{"claude-bin:*", "claude-bin", "anything", true},
		{"claude-bin:", "claude-bin", "anything", true},
		{":opus", "claude-bin", "opus-max", true},
		{"*/DeepSeek*", "cdck", "deepseek-ai/DeepSeek-V4-Flash", true},

		// Component mismatches.
		{"claude-bin:opus", "claude-bin", "fable-medium", false},
		{"claude-bin:opus", "chatgpt", "opus-max", false},
		{"opus", "claude-bin", "", false},

		// Model IDs may contain a colon; the spec must still be writable.
		{"llama3.2:latest", "ollama", "llama3.2:latest", true},
		{"ollama:llama3.2:latest", "ollama", "llama3.2:latest", true},
		{"llama3.2:latest", "ollama", "llama3.2:7b", false},

		// Matching is case-insensitive (gateMatches lowercases first).
		{"opus", "claude-bin", "opus-max", true},
	}

	for _, tc := range tests {
		t.Run(tc.spec+"/"+tc.provider+":"+tc.model, func(t *testing.T) {
			if got := specMatches(tc.spec, tc.provider, tc.model); got != tc.want {
				t.Errorf("specMatches(%q, %q, %q) = %v, want %v", tc.spec, tc.provider, tc.model, got, tc.want)
			}
		})
	}
}

func TestFilterInstructionsCaseInsensitive(t *testing.T) {
	doc := "[[[CLAUDE-BIN:Opus]]]\nopus rule\n"
	if got := FilterInstructionsForModel(doc, "claude-bin", "opus-max"); !strings.Contains(got, "opus rule") {
		t.Errorf("uppercase spec did not match: %q", got)
	}
}

func TestFilterInstructionsIgnoresMarkersInCodeBlocks(t *testing.T) {
	// Documenting the syntax inside your own file must not gate the rest of it.
	doc := "intro\n\n```md\n[[[claude-bin:opus]]]\nexample line\n```\n\nafter fence\n"
	got := FilterInstructionsForModel(doc, "chatgpt", "gpt-6-astra")
	for _, want := range []string{"intro", "[[[claude-bin:opus]]]", "example line", "after fence"} {
		if !strings.Contains(got, want) {
			t.Errorf("fenced content altered, missing %q in:\n%s", want, got)
		}
	}

	indented := "intro\n\n    [[[claude-bin:opus]]]\n    example line\n\nafter block\n"
	got = FilterInstructionsForModel(indented, "chatgpt", "gpt-6-astra")
	if !strings.Contains(got, "after block") || !strings.Contains(got, "[[[claude-bin:opus]]]") {
		t.Errorf("indented marker treated as a gate: %q", got)
	}
}

func TestFilterInstructionsInlineMentionUnchanged(t *testing.T) {
	// "[[[" appears but no line is a marker: content must be byte-identical.
	doc := "Write `[[[claude-bin:opus]]]` on its own line.\n\n\n\nTrailing prose\n"
	if got := FilterInstructionsForModel(doc, "chatgpt", "gpt-6"); got != doc {
		t.Errorf("inline mention rewrote content:\n%q\nwant:\n%q", got, doc)
	}
}

func TestFilterInstructionsCRLF(t *testing.T) {
	doc := "shared\r\n\r\n[[[claude-bin:opus]]]\r\nopus rule\r\n\r\n[[[all]]]\r\nclosing\r\n"
	got := FilterInstructionsForModel(doc, "chatgpt", "gpt-6-astra")
	if strings.Contains(got, "opus rule") {
		t.Errorf("CRLF marker not honoured: %q", got)
	}
	if !strings.Contains(got, "shared\r\n") || !strings.Contains(got, "closing") {
		t.Errorf("CRLF content mangled: %q", got)
	}
	if strings.Contains(got, "\r\n\r\n\r\n") {
		t.Errorf("blank run not collapsed for CRLF: %q", got)
	}
}

func TestFilterInstructionsEmptyGateResets(t *testing.T) {
	doc := "[[[claude-bin:opus]]]\nopus rule\n[[[]]]\nshared again\n"
	got := FilterInstructionsForModel(doc, "chatgpt", "gpt-6")
	if strings.Contains(got, "opus rule") {
		t.Errorf("gate not applied: %q", got)
	}
	if !strings.Contains(got, "shared again") {
		t.Errorf("empty gate did not reset: %q", got)
	}
}

func TestInstructionSetGatesUserPartOnly(t *testing.T) {
	// Identical markers in the user file and a project file: only the user file
	// is gate-filtered. The project marker stays inert literal text so shared
	// repository files never vary by model.
	gated := "shared rule\n[[[claude-bin:opus]]]\nopus rule\n"
	set := instructionSet{User: gated, Project: []string{gated}}

	got := set.Resolve("chatgpt", "gpt-6-astra")
	if strings.Count(got, "opus rule") != 1 {
		t.Errorf("expected project copy to survive and user copy to be dropped: %q", got)
	}
	if !strings.Contains(got, "[[[claude-bin:opus]]]") {
		t.Errorf("project marker should pass through verbatim: %q", got)
	}
	if strings.Count(got, "shared rule") != 2 {
		t.Errorf("ungated content dropped: %q", got)
	}

	got = set.Resolve("claude-bin", "opus-max")
	if strings.Count(got, "opus rule") != 2 {
		t.Errorf("expected both copies for matching model: %q", got)
	}
}

func TestExpandTemplateResolvesUserGatesByModel(t *testing.T) {
	ctx := TemplateContext{
		agentsSet: instructionSet{
			User:    "shared rule\n[[[claude-bin:opus]]]\nopus rule\n",
			Project: []string{"project rule"},
		},
		agentsSetLoaded: true,
	}

	got := ExpandTemplate("{{agents}}", ctx.WithLLM("chatgpt", "gpt-6-astra"))
	if !strings.Contains(got, "shared rule") || !strings.Contains(got, "project rule") {
		t.Errorf("ungated content dropped: %q", got)
	}
	if strings.Contains(got, "opus rule") {
		t.Errorf("gated rule leaked to other model: %q", got)
	}

	got = ExpandTemplate("{{agents}}", ctx.WithLLM("claude-bin", "opus-max"))
	if !strings.Contains(got, "opus rule") {
		t.Errorf("gated rule dropped for matching model: %q", got)
	}
}

func TestExpandTemplateAgentsDirectWriteWins(t *testing.T) {
	// A caller overriding Agents after WithLLM must not be silently ignored.
	ctx := TemplateContext{
		agentsSet:       instructionSet{User: "discovered\n[[[claude-bin:opus]]]\nopus rule\n"},
		agentsSetLoaded: true,
	}.WithLLM("claude-bin", "opus-max")
	if !strings.Contains(ctx.Agents, "opus rule") {
		t.Fatalf("WithLLM did not resolve Agents: %q", ctx.Agents)
	}

	ctx.Agents = "caller override"
	if got := ExpandTemplate("{{agents}}", ctx); got != "caller override" {
		t.Errorf("direct write ignored: %q", got)
	}
}

func TestNewTemplateContextResolvesGatesThroughWithLLM(t *testing.T) {
	// The production path: discovery first, model known later.
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	userDoc := "user shared\n[[[claude-bin:opus]]]\nuser opus only\n"
	if err := os.WriteFile(filepath.Join(configDir, "AGENTS.md"), []byte(userDoc), 0o644); err != nil {
		t.Fatalf("write user AGENTS.md: %v", err)
	}

	project := t.TempDir()
	ctx := NewTemplateContextForTemplateInDir("{{agents}}", project)
	if !strings.Contains(ctx.Agents, "user opus only") {
		t.Fatalf("pre-model context should keep gated content: %q", ctx.Agents)
	}

	got := ExpandTemplate("{{agents}}", ctx.WithLLM("chatgpt", "gpt-6-astra"))
	if strings.Contains(got, "user opus only") {
		t.Errorf("gate not applied on production path: %q", got)
	}
	if !strings.Contains(got, "user shared") {
		t.Errorf("ungated content dropped: %q", got)
	}
}

func TestExpandTemplateAgentsWithoutDiscovery(t *testing.T) {
	// Agents set directly (no discovery) is emitted verbatim: gating belongs to
	// the user-level file, which only discovery can identify.
	ctx := TemplateContext{Agents: "caller supplied\n[[[claude-bin:opus]]]\nopus rule\n"}
	if got := ExpandTemplate("{{agents}}", ctx.WithLLM("chatgpt", "gpt-6")); got != ctx.Agents {
		t.Errorf("directly set Agents changed: %q", got)
	}
}

func TestDiscoverProjectInstructionsForModelScopesGatesToUserFile(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	userDoc := "user shared\n[[[claude-bin:opus]]]\nuser opus only\n"
	if err := os.WriteFile(filepath.Join(configDir, "AGENTS.md"), []byte(userDoc), 0o644); err != nil {
		t.Fatalf("write user AGENTS.md: %v", err)
	}

	project := t.TempDir()
	projectDoc := "project shared\n[[[claude-bin:opus]]]\nproject gated text\n"
	if err := os.WriteFile(filepath.Join(project, "AGENTS.md"), []byte(projectDoc), 0o644); err != nil {
		t.Fatalf("write project AGENTS.md: %v", err)
	}

	got := DiscoverProjectInstructionsInDir(project, "chatgpt", "gpt-6-astra")
	if strings.Contains(got, "user opus only") {
		t.Errorf("user gate not applied: %q", got)
	}
	if !strings.Contains(got, "project gated text") {
		t.Errorf("project file must not be filtered: %q", got)
	}
	if !strings.Contains(got, "[[[claude-bin:opus]]]") {
		t.Errorf("project marker must pass through verbatim: %q", got)
	}
	if !strings.Contains(got, "user shared") || !strings.Contains(got, "project shared") {
		t.Errorf("ungated content dropped: %q", got)
	}

	got = DiscoverProjectInstructionsInDir(project, "claude-bin", "opus-max")
	if !strings.Contains(got, "user opus only") {
		t.Errorf("user gate dropped for matching model: %q", got)
	}
}

func TestDiscoverProjectInstructionsLeavesEveryProjectSourceInert(t *testing.T) {
	// AGENTS.override.md and the fallback chain take different branches in
	// loadProjectInstructionSet; none of them may be gate-filtered.
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	gated := "shared\n[[[claude-bin:opus]]]\ngated text\n"

	for _, tc := range []struct {
		name string
		file string
	}{
		{"override file", "AGENTS.override.md"},
		{"fallback file", "CLAUDE.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := t.TempDir()
			if err := os.WriteFile(filepath.Join(project, tc.file), []byte(gated), 0o644); err != nil {
				t.Fatalf("write %s: %v", tc.file, err)
			}

			got := DiscoverProjectInstructionsInDir(project, "chatgpt", "gpt-6-astra")
			if !strings.Contains(got, "gated text") {
				t.Errorf("%s was filtered: %q", tc.file, got)
			}
			if !strings.Contains(got, "[[[claude-bin:opus]]]") {
				t.Errorf("%s marker not verbatim: %q", tc.file, got)
			}
		})
	}
}

func TestFilterInstructionsNoGatesUnchanged(t *testing.T) {
	doc := "plain instructions\n\nwith paragraphs\n"
	if got := FilterInstructionsForModel(doc, "chatgpt", "gpt-6"); got != doc {
		t.Errorf("content changed: %q", got)
	}
}

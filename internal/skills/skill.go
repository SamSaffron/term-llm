// Package skills provides Agent Skills integration for term-llm.
// Skills are portable, cross-tool instruction bundles using the SKILL.md format.
package skills

import (
	"fmt"
	"regexp"
	"strings"
)

// SkillSource indicates where a skill was loaded from.
type SkillSource int

const (
	SourceLocal   SkillSource = iota // Project-local (.skills/, .claude/skills/, etc.)
	SourceUser                       // User-global (~/.config/term-llm/skills/, etc.)
	SourceBuiltin                    // Embedded built-in
	SourceClaude                     // Claude Code ecosystem (~/.claude/skills/)
	SourceCodex                      // Codex ecosystem (~/.codex/skills/)
	SourceGemini                     // Gemini CLI ecosystem (~/.gemini/skills/)
	SourceCursor                     // Cursor ecosystem (~/.cursor/skills/)
)

// SourceName returns a human-readable name for the skill source.
func (s SkillSource) SourceName() string {
	switch s {
	case SourceLocal:
		return "local"
	case SourceUser:
		return "user"
	case SourceBuiltin:
		return "builtin"
	case SourceClaude:
		return "claude"
	case SourceCodex:
		return "codex"
	case SourceGemini:
		return "gemini"
	case SourceCursor:
		return "cursor"
	default:
		return "unknown"
	}
}

// SkillToolDef defines a script-backed tool declared in a skill's SKILL.md frontmatter.
// When the skill is activated, these tools are dynamically registered with the engine.
// Scripts are resolved relative to the skill directory (SourcePath).
type SkillToolDef struct {
	// Name is the tool name shown to the LLM. Must match ^[a-z][a-z0-9_]*$
	Name string `yaml:"name"`

	// Description is the tool description passed to the LLM.
	Description string `yaml:"description"`

	// Script is the path to the script, relative to the skill directory.
	// Subdirectories are allowed (e.g. "scripts/travel-time.sh").
	Script string `yaml:"script"`

	// Input is a JSON Schema (type: object) for the tool's input parameters.
	// If omitted, the tool accepts no parameters.
	Input map[string]interface{} `yaml:"input,omitempty"`

	// TimeoutSeconds is the execution timeout. Default 30, max 300.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`

	// Env is a map of additional environment variables to set when running the script.
	Env map[string]string `yaml:"env,omitempty"`

	// Call controls how arguments are passed to the script.
	//   ""       / "args"       — named flags: --key value (default)
	//   "positional"            — positional args in schema property order
	//   "json"                  — JSON object on stdin
	Call string `yaml:"call,omitempty"`
}

// Skill represents a skill loaded from a SKILL.md file.
type Skill struct {
	// Required fields
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	// Optional standard fields
	License       string            `yaml:"license,omitempty"`
	Compatibility string            `yaml:"compatibility,omitempty"`
	AllowedTools  []string          `yaml:"-"` // Parsed from allowed-tools
	Metadata      map[string]string `yaml:"metadata,omitempty"`

	// Tools declares script-backed tools that are registered when this skill is activated.
	Tools []SkillToolDef `yaml:"-"`

	// Extras stores vendor-specific/unknown frontmatter fields
	Extras map[string]any `yaml:"-"`

	// Body is the Markdown content after frontmatter
	Body string `yaml:"-"`

	// Resource discovery
	References []string `yaml:"-"` // Files in references/
	Scripts    []string `yaml:"-"` // Files in scripts/
	Assets     []string `yaml:"-"` // Files in assets/

	// Source tracking
	Source     SkillSource `yaml:"-"`
	SourcePath string      `yaml:"-"` // Directory path

	// State
	loaded bool // True if full body has been loaded
}

// namePattern validates skill names per the spec:
// - 1-64 characters
// - lowercase letters, numbers, hyphens only
// - no leading/trailing hyphen
// - no consecutive hyphens
var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidateName checks if a skill name is valid per the spec.
// Returns an error describing the issue, or nil if valid.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("skill name is required")
	}
	if len(name) > 64 {
		return fmt.Errorf("skill name must be 1-64 characters, got %d", len(name))
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("must be lowercase letters, numbers, hyphens only")
	}
	if strings.Contains(name, "--") {
		return fmt.Errorf("cannot contain consecutive hyphens")
	}
	return nil
}

// Validate checks that the skill meets the spec requirements.
func (s *Skill) Validate() error {
	// Name validation
	if s.Name == "" {
		return fmt.Errorf("skill name is required")
	}
	if len(s.Name) > 64 {
		return fmt.Errorf("skill name must be 1-64 characters, got %d", len(s.Name))
	}
	if !namePattern.MatchString(s.Name) {
		return fmt.Errorf("skill name must be lowercase letters, numbers, hyphens only (no leading/trailing/consecutive hyphens): %q", s.Name)
	}
	if strings.Contains(s.Name, "--") {
		return fmt.Errorf("skill name cannot contain consecutive hyphens: %q", s.Name)
	}

	// Description validation
	if s.Description == "" {
		return fmt.Errorf("skill description is required")
	}
	if len(s.Description) > 1024 {
		return fmt.Errorf("skill description must be 1-1024 characters, got %d", len(s.Description))
	}

	// Compatibility validation
	if len(s.Compatibility) > 500 {
		return fmt.Errorf("skill compatibility must be <= 500 characters, got %d", len(s.Compatibility))
	}

	return nil
}

// IsLoaded returns true if the skill body has been loaded.
func (s *Skill) IsLoaded() bool {
	return s.loaded
}

// String returns a brief description of the skill.
func (s *Skill) String() string {
	var parts []string
	parts = append(parts, s.Name)
	if s.Description != "" {
		parts = append(parts, "-", s.Description)
	}
	return strings.Join(parts, " ")
}

// HasResources returns true if the skill has bundled resources.
func (s *Skill) HasResources() bool {
	return len(s.References) > 0 || len(s.Scripts) > 0 || len(s.Assets) > 0
}

// HasTools returns true if the skill declares any script-backed tools.
func (s *Skill) HasTools() bool {
	return len(s.Tools) > 0
}

// ResourceTree returns a formatted string of bundled resources.
func (s *Skill) ResourceTree() string {
	if !s.HasResources() {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("Bundled resources:\n")

	if len(s.References) > 0 {
		sb.WriteString("  references/\n")
		for _, r := range s.References {
			sb.WriteString(fmt.Sprintf("    %s\n", r))
		}
	}

	if len(s.Scripts) > 0 {
		sb.WriteString("  scripts/\n")
		for _, sc := range s.Scripts {
			sb.WriteString(fmt.Sprintf("    %s\n", sc))
		}
	}

	if len(s.Assets) > 0 {
		sb.WriteString("  assets/\n")
		for _, a := range s.Assets {
			sb.WriteString(fmt.Sprintf("    %s\n", a))
		}
	}

	return sb.String()
}

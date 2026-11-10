// Package skills provides Agent Skills integration for term-llm.
// Skills are portable, cross-tool instruction bundles using the SKILL.md format.
package skills

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// maxCodeModeFiles is the maximum number of source files in a code-mode manifest.
const maxCodeModeFiles = 32

// allowedModuleExts lists the file extensions permitted for code-mode source files.
var allowedModuleExts = map[string]bool{
	".mjs": true,
	".js":  true,
}

// CodeModeRequires lists exact registered tool identities needed by skill modules.
// These are dependencies only; they never register, enable, or grant tools.
type CodeModeRequires struct {
	Tools []string `yaml:"tools,omitempty"`
}

// CodeModeManifest describes the ES module sources shipped with a skill.
// Parsing and validation happen at load time; actual admission, trust decisions,
// and source snapshotting are handled by the host at activation.
type CodeModeManifest struct {
	// Module is the entry-point file, relative to the skill root.
	// It must appear in Files.
	Module string `yaml:"module"`

	// Files lists all declared source files, relative to the skill root.
	// At most 32 entries; each must use a supported extension (.mjs, .js).
	Files []string `yaml:"files"`

	// Types is an optional TypeScript declaration file (.d.ts), relative to root.
	Types string `yaml:"types,omitempty"`

	// Requires declares dependencies; it never registers, enables, or grants tools.
	Requires CodeModeRequires `yaml:"requires,omitempty"`
}

// Validate checks the manifest fields without reading any files from disk.
func (m *CodeModeManifest) Validate() error {
	if m == nil {
		return nil
	}
	if m.Module == "" {
		return fmt.Errorf("code-mode: module is required")
	}
	if err := validateCodeModeSourcePath(m.Module); err != nil {
		return fmt.Errorf("code-mode: module: %w", err)
	}
	if len(m.Files) == 0 {
		return fmt.Errorf("code-mode: files must not be empty")
	}
	if len(m.Files) > maxCodeModeFiles {
		return fmt.Errorf("code-mode: files: at most %d source files allowed, got %d", maxCodeModeFiles, len(m.Files))
	}
	entryInFiles := false
	seen := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		if err := validateCodeModeSourcePath(f); err != nil {
			return fmt.Errorf("code-mode: files %q: %w", f, err)
		}
		lower := strings.ToLower(filepath.ToSlash(f))
		if seen[lower] {
			return fmt.Errorf("code-mode: files: duplicate or case-alias entry %q", f)
		}
		seen[lower] = true
		if f == m.Module {
			entryInFiles = true
		}
	}
	if !entryInFiles {
		return fmt.Errorf("code-mode: module %q must be listed in files", m.Module)
	}
	if m.Types != "" {
		if err := validateCodeModeTypesPath(m.Types); err != nil {
			return fmt.Errorf("code-mode: types: %w", err)
		}
	}
	for _, tool := range m.Requires.Tools {
		if !toolNamePattern.MatchString(tool) {
			return fmt.Errorf("code-mode: requires.tools: invalid tool name %q", tool)
		}
	}
	return nil
}

// toolNamePattern validates tool names per the registry convention.
var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]*$`)

// validateCodeModeSourcePath checks a file path declared in a code-mode manifest.
// It rejects absolute paths, parent traversal, backslashes, drive letters,
// unsupported schemes, empty components, and enforces allowed extensions.
func validateCodeModeSourcePath(p string) error {
	if p == "" {
		return fmt.Errorf("path must not be empty")
	}
	if strings.ContainsAny(p, "\\") {
		return fmt.Errorf("backslashes are not allowed: %q", p)
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("absolute path not allowed: %q", p)
	}
	if strings.Contains(p, ":") {
		return fmt.Errorf("scheme specifier not allowed: %q", p)
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if strings.HasPrefix(clean, "..") {
		return fmt.Errorf("path traversal not allowed: %q", p)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid path component in %q", p)
		}
	}
	ext := strings.ToLower(filepath.Ext(p))
	if !allowedModuleExts[ext] {
		return fmt.Errorf("unsupported file extension %q (allowed: .mjs, .js): %q", ext, p)
	}
	return nil
}

// validateCodeModeTypesPath checks a types declaration file path.
func validateCodeModeTypesPath(p string) error {
	if p == "" {
		return fmt.Errorf("path must not be empty")
	}
	if strings.ContainsAny(p, "\\") {
		return fmt.Errorf("backslashes are not allowed: %q", p)
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("absolute path not allowed: %q", p)
	}
	if strings.Contains(p, ":") {
		return fmt.Errorf("scheme specifier not allowed: %q", p)
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if strings.HasPrefix(clean, "..") {
		return fmt.Errorf("path traversal not allowed: %q", p)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid path component in %q", p)
		}
	}
	if !strings.HasSuffix(strings.ToLower(p), ".d.ts") {
		return fmt.Errorf("types file must have .d.ts extension: %q", p)
	}
	return nil
}

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
	// UserInstalled is set by activation from discovery scope, never frontmatter.
	UserInstalled bool `yaml:"-" json:"-"`

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

// Validate checks the required fields of a script-backed skill tool.
func (d SkillToolDef) Validate() error {
	if d.Name == "" {
		return fmt.Errorf("skill tool: name is required")
	}
	if d.Description == "" {
		return fmt.Errorf("skill tool %q: description is required", d.Name)
	}
	if d.Script == "" {
		return fmt.Errorf("skill tool %q: script is required", d.Name)
	}
	return nil
}

// Skill represents a skill loaded from a SKILL.md file.
type Skill struct {
	// Required fields
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	// Optional standard fields
	License             string            `yaml:"license,omitempty"`
	Compatibility       string            `yaml:"compatibility,omitempty"`
	AllowedTools        []string          `yaml:"-"` // Parsed from allowed-tools
	AllowedToolsPresent bool              `yaml:"-"` // Distinguishes omitted from an explicit empty allowlist
	Metadata            map[string]string `yaml:"metadata,omitempty"`

	// Tools declares script-backed tools that are registered when this skill is activated.
	Tools []SkillToolDef `yaml:"-"`

	// CodeMode describes the ES module sources shipped with this skill, if any.
	// Set from the code-mode: frontmatter block; nil when absent.
	// Trust and admission are determined by the host at activation time.
	CodeMode *CodeModeManifest `yaml:"-"`

	// Extras stores vendor-specific/unknown frontmatter fields
	Extras map[string]any `yaml:"-"`

	// Body is the Markdown content after frontmatter
	Body string `yaml:"-"`

	// Resource discovery
	References []string `yaml:"-"` // Files in references/
	Scripts    []string `yaml:"-"` // Files in scripts/
	Assets     []string `yaml:"-"` // Files in assets/

	// Source tracking
	// UserInstalled identifies user-scope discovery independently of ecosystem.
	// Unclassified skills default to the project execution gate.
	UserInstalled bool        `yaml:"-" json:"-"`
	Source        SkillSource `yaml:"-"`
	SourcePath    string      `yaml:"-"` // Directory path

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

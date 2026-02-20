package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Registry manages skill discovery and resolution.
type Registry struct {
	// Configuration
	config RegistryConfig

	// Search paths in priority order (first match wins)
	searchPaths []searchPath

	// Cache of discovered skills (name -> skill)
	cache map[string]*Skill

	// Shadow counts for visibility
	shadowCounts map[string]int

	// Pre-built sets for O(1) lookup
	neverAutoSet     map[string]bool
	alwaysEnabledSet map[string]bool
}

type searchPath struct {
	path   string
	source SkillSource
}

// RegistryConfig configures the skill registry.
type RegistryConfig struct {
	// AutoInvoke allows model-driven skill activation
	AutoInvoke bool

	// MetadataBudgetTokens limits skill metadata in system prompt
	MetadataBudgetTokens int

	// MaxActive limits skills included in metadata injection
	MaxActive int

	// Ecosystem integration
	IncludeProjectSkills  bool // Discover from project-local paths
	IncludeEcosystemPaths bool // Include ~/.codex/skills, ~/.claude/skills, ~/.gemini/skills, .skills/

	// Skill lists
	AlwaysEnabled []string // Always include in metadata
	NeverAuto     []string // Must be explicit
}

// DefaultRegistryConfig returns the default configuration.
func DefaultRegistryConfig() RegistryConfig {
	return RegistryConfig{
		AutoInvoke:            true,
		MetadataBudgetTokens:  8000,
		MaxActive:             8,
		IncludeProjectSkills:  true,
		IncludeEcosystemPaths: true,
	}
}

// NewRegistry creates a skill registry with the given configuration.
func NewRegistry(cfg RegistryConfig) (*Registry, error) {
	neverAutoSet := make(map[string]bool, len(cfg.NeverAuto))
	for _, n := range cfg.NeverAuto {
		neverAutoSet[n] = true
	}
	alwaysEnabledSet := make(map[string]bool, len(cfg.AlwaysEnabled))
	for _, a := range cfg.AlwaysEnabled {
		alwaysEnabledSet[a] = true
	}

	r := &Registry{
		config:           cfg,
		cache:            make(map[string]*Skill),
		shadowCounts:     make(map[string]int),
		neverAutoSet:     neverAutoSet,
		alwaysEnabledSet: alwaysEnabledSet,
	}

	// Build search paths based on config
	if err := r.buildSearchPaths(); err != nil {
		return nil, err
	}

	return r, nil
}

// buildSearchPaths constructs the ordered list of search paths.
func (r *Registry) buildSearchPaths() error {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	home, _ := os.UserHomeDir()
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" && home != "" {
		configDir = filepath.Join(home, ".config")
	}

	// 1. Project-local paths (if enabled)
	if r.config.IncludeProjectSkills {
		r.addProjectPaths(cwd)
	}

	// 2. User-scope paths (always included)
	r.addUserPaths(home, configDir)

	return nil
}

// addProjectPaths adds project-local skill directories.
// Walks upward from CWD to repo root (or filesystem root).
func (r *Registry) addProjectPaths(cwd string) {
	// Find repo root
	repoRoot := findRepoRoot(cwd)
	if repoRoot == "" {
		repoRoot = cwd
	}

	// Walk from CWD up to repo root
	current := cwd
	for {
		// Add paths at this level in precedence order
		r.addProjectPathsAtLevel(current)

		// Stop at repo root
		if current == repoRoot {
			break
		}

		parent := filepath.Dir(current)
		if parent == current {
			break // Reached filesystem root
		}
		current = parent
	}
}

// addProjectPathsAtLevel adds skill directories at a specific directory level.
func (r *Registry) addProjectPathsAtLevel(dir string) {
	// Universal convention - always included when IncludeProjectSkills is true
	// (the caller already checks IncludeProjectSkills before calling addProjectPaths)
	r.searchPaths = append(r.searchPaths, searchPath{
		path:   filepath.Join(dir, ".skills"),
		source: SourceLocal,
	})

	// Ecosystem paths are gated by IncludeEcosystemPaths
	if !r.config.IncludeEcosystemPaths {
		return
	}

	// Claude Code
	r.searchPaths = append(r.searchPaths, searchPath{
		path:   filepath.Join(dir, ".claude", "skills"),
		source: SourceClaude,
	})

	// Codex
	r.searchPaths = append(r.searchPaths, searchPath{
		path:   filepath.Join(dir, ".codex", "skills"),
		source: SourceCodex,
	})

	// Gemini CLI
	r.searchPaths = append(r.searchPaths, searchPath{
		path:   filepath.Join(dir, ".gemini", "skills"),
		source: SourceGemini,
	})

	// Cursor
	r.searchPaths = append(r.searchPaths, searchPath{
		path:   filepath.Join(dir, ".cursor", "skills"),
		source: SourceCursor,
	})
}

// addUserPaths adds user-scope skill directories.
func (r *Registry) addUserPaths(home, configDir string) {
	// term-llm user skills (highest user-scope precedence)
	if configDir != "" {
		r.searchPaths = append(r.searchPaths, searchPath{
			path:   filepath.Join(configDir, "term-llm", "skills"),
			source: SourceUser,
		})
	}

	if home == "" {
		return
	}

	// Universal user skills - always included
	r.searchPaths = append(r.searchPaths, searchPath{
		path:   filepath.Join(home, ".skills"),
		source: SourceUser,
	})

	// Ecosystem paths are gated by IncludeEcosystemPaths
	if !r.config.IncludeEcosystemPaths {
		return
	}

	// Claude Code user skills
	r.searchPaths = append(r.searchPaths, searchPath{
		path:   filepath.Join(home, ".claude", "skills"),
		source: SourceClaude,
	})

	// Codex user skills
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	r.searchPaths = append(r.searchPaths, searchPath{
		path:   filepath.Join(codexHome, "skills"),
		source: SourceCodex,
	})

	// Gemini CLI user skills
	r.searchPaths = append(r.searchPaths, searchPath{
		path:   filepath.Join(home, ".gemini", "skills"),
		source: SourceGemini,
	})

	// Cursor user skills
	r.searchPaths = append(r.searchPaths, searchPath{
		path:   filepath.Join(home, ".cursor", "skills"),
		source: SourceCursor,
	})
}

// Get retrieves a skill by name, loading full content.
func (r *Registry) Get(name string) (*Skill, error) {
	// Check cache first
	if skill, ok := r.cache[name]; ok {
		// If we have metadata only, load full content
		if !skill.IsLoaded() {
			fullSkill, err := LoadFromDir(skill.SourcePath, skill.Source, true)
			if err != nil {
				return nil, err
			}
			r.cache[name] = fullSkill
			return fullSkill, nil
		}
		return skill, nil
	}

	// Search filesystem paths
	for _, sp := range r.searchPaths {
		skillDir := filepath.Join(sp.path, name)
		if IsSkillDir(skillDir) {
			skill, err := LoadFromDir(skillDir, sp.source, true)
			if err != nil {
				return nil, fmt.Errorf("load skill %s: %w", name, err)
			}
			if err := skill.Validate(); err != nil {
				return nil, fmt.Errorf("invalid skill %s: %w", name, err)
			}
			r.cache[name] = skill
			return skill, nil
		}
	}

	return nil, fmt.Errorf("skill not found: %s", name)
}

// List returns all available skills (metadata only).
// Each skill appears only once, with first-found taking precedence.
func (r *Registry) List() ([]*Skill, error) {
	seen := make(map[string]bool)
	r.shadowCounts = make(map[string]int)
	var skills []*Skill

	// Scan filesystem paths
	for _, sp := range r.searchPaths {
		found, err := r.scanDir(sp.path, sp.source)
		if err != nil {
			continue // Skip directories that don't exist or can't be read
		}

		for _, skill := range found {
			if seen[skill.Name] {
				r.shadowCounts[skill.Name]++
			} else {
				seen[skill.Name] = true
				skills = append(skills, skill)
			}
		}
	}

	// Sort by name
	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Name < skills[j].Name
	})

	return skills, nil
}

// ListAll returns all skills from all paths without shadowing.
// Use this when you want to see every installed copy of a skill.
func (r *Registry) ListAll() ([]*Skill, error) {
	var allSkills []*Skill

	// Scan filesystem paths
	for _, sp := range r.searchPaths {
		found, err := r.scanDir(sp.path, sp.source)
		if err != nil {
			continue // Skip directories that don't exist or can't be read
		}

		allSkills = append(allSkills, found...)
	}

	// Sort by name, then by path
	sort.Slice(allSkills, func(i, j int) bool {
		if allSkills[i].Name != allSkills[j].Name {
			return allSkills[i].Name < allSkills[j].Name
		}
		return allSkills[i].SourcePath < allSkills[j].SourcePath
	})

	return allSkills, nil
}

// ListBySource returns skills from a specific source.
func (r *Registry) ListBySource(source SkillSource) ([]*Skill, error) {
	all, err := r.List()
	if err != nil {
		return nil, err
	}

	var filtered []*Skill
	for _, skill := range all {
		if skill.Source == source {
			filtered = append(filtered, skill)
		}
	}
	return filtered, nil
}

// ShadowCount returns how many skills were shadowed by this name.
func (r *Registry) ShadowCount(name string) int {
	return r.shadowCounts[name]
}

// Reload clears the cache and rediscovers skills.
func (r *Registry) Reload() error {
	r.cache = make(map[string]*Skill)
	r.shadowCounts = make(map[string]int)
	r.searchPaths = nil
	return r.buildSearchPaths()
}

// scanDir scans a directory for skill subdirectories.
func (r *Registry) scanDir(dir string, source SkillSource) ([]*Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var skills []*Skill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillDir := filepath.Join(dir, entry.Name())
		if !IsSkillDir(skillDir) {
			continue
		}

		// Load metadata only for listing
		skill, err := LoadFromDir(skillDir, source, false)
		if err != nil {
			// Skip invalid skills with a diagnostic
			fmt.Fprintf(os.Stderr, "warning: skipping invalid skill %s: %v\n", skillDir, err)
			continue
		}

		if err := skill.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping invalid skill %s: %v\n", skillDir, err)
			continue
		}

		skills = append(skills, skill)
	}

	return skills, nil
}

// IsNeverAuto checks if a skill requires explicit activation.
func (r *Registry) IsNeverAuto(name string) bool {
	return r.neverAutoSet[name]
}

// IsAlwaysEnabled checks if a skill should always be included.
func (r *Registry) IsAlwaysEnabled(name string) bool {
	return r.alwaysEnabledSet[name]
}

// GetUserSkillsDir returns the path for user-global skills.
func GetUserSkillsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		configDir = filepath.Join(home, ".config")
	}

	return filepath.Join(configDir, "term-llm", "skills"), nil
}

// GetLocalSkillsDir returns the path for project-local skills.
func GetLocalSkillsDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, ".skills"), nil
}

// findRepoRoot walks upward to find a .git directory.
func findRepoRoot(start string) string {
	current := start
	for {
		gitPath := filepath.Join(current, ".git")
		if info, err := os.Stat(gitPath); err == nil && info.IsDir() {
			return current
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "" // Reached filesystem root
		}
		current = parent
	}
}

// expandPath expands ~ in paths.
func expandPath(path, home string) string {
	if strings.HasPrefix(path, "~/") && home != "" {
		return filepath.Join(home, path[2:])
	}
	return path
}

// CreateSkillDir creates a skill directory with template files.
func CreateSkillDir(baseDir, name string) error {
	skillDir := filepath.Join(baseDir, name)

	// Create directory
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	// Create SKILL.md
	skillMD := fmt.Sprintf(`---
name: %s
description: "Description of what this skill does and when to use it"
# license: MIT
# compatibility: "Requires Go 1.20+"
# allowed-tools: read grep glob
---

# %s

Instructions for the AI assistant when this skill is activated.

## When to Use

Describe the scenarios where this skill should be activated.

## Guidelines

- Add specific instructions
- Include domain-specific knowledge
- Define expected behavior
`, name, strings.Title(strings.ReplaceAll(name, "-", " ")))

	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0644); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}

	return nil
}

// CopySkill copies a skill to a new location.
func CopySkill(src *Skill, destDir, newName string) error {
	destSkillDir := filepath.Join(destDir, newName)

	// Create directory
	if err := os.MkdirAll(destSkillDir, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	// If source is from filesystem, copy files
	if src.Source != SourceBuiltin && src.SourcePath != "" {
		// Copy SKILL.md
		srcPath := filepath.Join(src.SourcePath, "SKILL.md")
		if data, err := os.ReadFile(srcPath); err == nil {
			// Update name in frontmatter
			content := strings.Replace(string(data), fmt.Sprintf("name: %s", src.Name), fmt.Sprintf("name: %s", newName), 1)
			if err := os.WriteFile(filepath.Join(destSkillDir, "SKILL.md"), []byte(content), 0644); err != nil {
				return fmt.Errorf("write SKILL.md: %w", err)
			}
		} else {
			// Try lowercase
			srcPath = filepath.Join(src.SourcePath, "skill.md")
			if data, err := os.ReadFile(srcPath); err == nil {
				content := strings.Replace(string(data), fmt.Sprintf("name: %s", src.Name), fmt.Sprintf("name: %s", newName), 1)
				if err := os.WriteFile(filepath.Join(destSkillDir, "SKILL.md"), []byte(content), 0644); err != nil {
					return fmt.Errorf("write SKILL.md: %w", err)
				}
			}
		}

		// Copy resource directories
		for _, subdir := range []string{"references", "scripts", "assets"} {
			srcSubdir := filepath.Join(src.SourcePath, subdir)
			if entries, err := os.ReadDir(srcSubdir); err == nil {
				destSubdir := filepath.Join(destSkillDir, subdir)
				if err := os.MkdirAll(destSubdir, 0755); err != nil {
					return fmt.Errorf("create %s: %w", subdir, err)
				}
				for _, entry := range entries {
					if entry.IsDir() {
						continue
					}
					srcFile := filepath.Join(srcSubdir, entry.Name())
					if data, err := os.ReadFile(srcFile); err == nil {
						destFile := filepath.Join(destSubdir, entry.Name())
						if err := os.WriteFile(destFile, data, 0644); err != nil {
							return fmt.Errorf("copy %s: %w", entry.Name(), err)
						}
					}
				}
			}
		}
	}

	return nil
}

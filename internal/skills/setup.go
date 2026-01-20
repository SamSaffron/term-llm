package skills

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// Setup holds the initialized skills system for a session.
type Setup struct {
	Registry *Registry
	XML      string   // Pregenerated <available_skills> XML
	Skills   []*Skill // Skills included in metadata
}

// NewSetup initializes the skills system from config.
// Returns nil if skills mode is "none" or no skills are available.
func NewSetup(cfg *config.SkillsConfig) (*Setup, error) {
	if cfg.Mode == "none" {
		return nil, nil
	}

	registry, err := NewRegistry(RegistryConfig{
		Mode:                  cfg.Mode,
		AutoInvoke:            cfg.AutoInvoke,
		MetadataBudgetTokens:  cfg.MetadataBudgetTokens,
		MaxActive:             cfg.MaxActive,
		SearchPaths:           cfg.SearchPaths,
		ExcludePaths:          cfg.ExcludePaths,
		IncludeProjectSkills:  cfg.IncludeProjectSkills,
		IncludeCodexPaths:     cfg.IncludeCodexPaths,
		IncludeClaudePaths:    cfg.IncludeClaudePaths,
		IncludeGeminiPaths:    cfg.IncludeGeminiPaths,
		IncludeUniversalPaths: cfg.IncludeUniversalPaths,
		AlwaysEnabled:         cfg.AlwaysEnabled,
		NeverAuto:             cfg.NeverAuto,
		Disabled:              cfg.Disabled,
		UseBuiltin:            cfg.UseBuiltin,
	})
	if err != nil {
		return nil, err
	}

	// List all available skills
	allSkills, err := registry.List()
	if err != nil {
		return nil, err
	}

	if len(allSkills) == 0 {
		// No skills available, return nil setup
		return &Setup{Registry: registry}, nil
	}

	// Filter by never_auto for metadata injection (explicit only skills excluded)
	var autoSkills []*Skill
	for _, skill := range allSkills {
		if !registry.IsNeverAuto(skill.Name) {
			autoSkills = append(autoSkills, skill)
		}
	}

	// Apply token budget and max count
	skills := TruncateSkillsToTokenBudget(
		autoSkills,
		cfg.AlwaysEnabled,
		cfg.MetadataBudgetTokens,
		cfg.MaxActive,
	)

	// Generate XML
	xml := GenerateAvailableSkillsXML(skills)

	return &Setup{
		Registry: registry,
		XML:      xml,
		Skills:   skills,
	}, nil
}

// HasSkillsXML returns true if the setup has skill XML to inject.
func (s *Setup) HasSkillsXML() bool {
	return s != nil && s.XML != ""
}

// CheckAgentsMdForSkills checks if AGENTS.md contains skill system markup.
// If true, the caller should not inject <available_skills> to avoid duplication.
func CheckAgentsMdForSkills() bool {
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}

	// Find repo root
	repoRoot := findRepoRoot(cwd)
	if repoRoot == "" {
		repoRoot = cwd
	}

	// Check AGENTS.md and AGENTS.override.md
	for _, name := range []string{"AGENTS.md", "AGENTS.override.md"} {
		path := filepath.Join(repoRoot, name)
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		contentStr := string(content)
		if strings.Contains(contentStr, "<skills_system") ||
			strings.Contains(contentStr, "<available_skills>") {
			return true
		}
	}

	return false
}

// LoadAgentsMd loads AGENTS.md and related files for system prompt injection.
// Returns empty string if AGENTS.md loading is disabled or files don't exist.
func LoadAgentsMd(cfg *config.AgentsMdConfig) string {
	if cfg == nil || !cfg.Enabled {
		return ""
	}

	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	repoRoot := findRepoRoot(cwd)
	if repoRoot == "" {
		repoRoot = cwd
	}

	var parts []string

	// 1. Load root AGENTS.md
	if content, err := os.ReadFile(filepath.Join(repoRoot, "AGENTS.md")); err == nil {
		parts = append(parts, string(content))
	}

	// 2. Load root AGENTS.override.md
	if content, err := os.ReadFile(filepath.Join(repoRoot, "AGENTS.override.md")); err == nil {
		parts = append(parts, string(content))
	}

	// 3. Walk from repo root to cwd, loading nested AGENTS.md files
	rel, _ := filepath.Rel(repoRoot, cwd)
	if rel != "." && rel != "" {
		current := repoRoot
		for _, segment := range strings.Split(rel, string(filepath.Separator)) {
			current = filepath.Join(current, segment)
			if content, err := os.ReadFile(filepath.Join(current, "AGENTS.md")); err == nil {
				parts = append(parts, string(content))
			}
		}
	}

	if len(parts) == 0 {
		return ""
	}

	return strings.Join(parts, "\n\n---\n\n")
}

package skills

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// Setup holds the initialized skills system for a session.
type Setup struct {
	Registry    *Registry
	XML         string   // Pregenerated <available_skills> XML
	Skills      []*Skill // Skills included in metadata
	TotalSkills int      // Total auto-invocable skills discovered
	HasOverflow bool     // True when more skills exist than are shown
}

// NewSetup initializes the skills system from config.
// Returns nil if skills are disabled or no skills are available.
func NewSetup(cfg *config.SkillsConfig) (*Setup, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	registry, err := NewRegistry(RegistryConfig{
		AutoInvoke:            cfg.AutoInvoke,
		MetadataBudgetTokens:  cfg.MetadataBudgetTokens,
		MaxVisibleSkills:      cfg.MaxVisibleSkills,
		IncludeProjectSkills:  cfg.IncludeProjectSkills,
		IncludeEcosystemPaths: cfg.IncludeEcosystemPaths,
		AlwaysEnabled:         cfg.AlwaysEnabled,
		NeverAuto:             cfg.NeverAuto,
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
		return nil, nil
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
		cfg.MaxVisibleSkills,
	)

	// Generate XML
	xml := GenerateAvailableSkillsXML(skills)

	totalAutoSkills := len(autoSkills)
	if len(skills) < totalAutoSkills {
		xml += GenerateSearchHint(len(skills), totalAutoSkills)
	}
	return &Setup{
		Registry:    registry,
		XML:         xml,
		Skills:      skills,
		TotalSkills: totalAutoSkills,
		HasOverflow: len(skills) < totalAutoSkills,
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

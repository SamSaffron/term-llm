package skills

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/config"
)

// Setup holds the initialized skills system for a session.
type Setup struct {
	Registry    *Registry
	XML         string   // Pregenerated <available_skills> XML
	Skills      []*Skill // Skills included in metadata
	TotalSkills int      // Total auto-invocable skills discovered
	HasOverflow bool     // True when more skills exist than are shown

	cfg          config.SkillsConfig
	metadataOnce sync.Once
	metadataErr  error
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

	hasSkills, err := registry.HasAvailableSkills()
	if err != nil {
		return nil, err
	}
	if !hasSkills {
		return nil, nil
	}

	return &Setup{
		Registry: registry,
		cfg:      *cfg,
	}, nil
}

func (s *Setup) ensureMetadata() error {
	if s == nil {
		return nil
	}

	s.metadataOnce.Do(func() {
		allSkills, err := s.Registry.List()
		if err != nil {
			s.metadataErr = err
			return
		}

		// Filter by never_auto for metadata injection (explicit only skills excluded)
		var autoSkills []*Skill
		for _, skill := range allSkills {
			if !s.Registry.IsNeverAuto(skill.Name) {
				autoSkills = append(autoSkills, skill)
			}
		}

		// Apply token budget and max count
		skills := TruncateSkillsToTokenBudget(
			autoSkills,
			s.cfg.AlwaysEnabled,
			s.cfg.MetadataBudgetTokens,
			s.cfg.MaxVisibleSkills,
		)

		// Generate XML
		xml := GenerateAvailableSkillsXML(skills)

		totalAutoSkills := len(autoSkills)
		if len(skills) < totalAutoSkills {
			xml += GenerateSearchHint(len(skills), totalAutoSkills)
		}

		s.XML = xml
		s.Skills = skills
		s.TotalSkills = totalAutoSkills
		s.HasOverflow = len(skills) < totalAutoSkills
	})

	return s.metadataErr
}

// HasSkillsXML returns true if the setup has skill XML to inject.
func (s *Setup) HasSkillsXML() bool {
	if s == nil {
		return false
	}
	if s.XML != "" {
		return true
	}
	if s.Registry == nil {
		return false
	}
	return s.ensureMetadata() == nil && s.XML != ""
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

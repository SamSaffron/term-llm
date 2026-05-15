package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/config"
)

// Setup holds the initialized skills system for a session.
type Setup struct {
	Registry    *Registry
	XML         string   // Pregenerated <available_skills> XML (populated lazily)
	Skills      []*Skill // Skills included in metadata (populated lazily)
	TotalSkills int      // Total auto-invocable skills discovered (populated lazily)
	HasOverflow bool     // True when more skills exist than are shown (populated lazily)

	alwaysEnabled        []string
	metadataBudgetTokens int
	maxVisibleSkills     int
	preloadedSkills      []*Skill // Primed startup catalog reused for first prompt metadata build

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

	// Prime the startup skill catalog once so prompt metadata generation can
	// reuse it without rescanning and reparsing before the first prompt.
	preloadedSkills, err := registry.List()
	if err != nil {
		return nil, err
	}
	if len(preloadedSkills) == 0 {
		return nil, nil
	}

	return &Setup{
		Registry:             registry,
		alwaysEnabled:        append([]string(nil), cfg.AlwaysEnabled...),
		metadataBudgetTokens: cfg.MetadataBudgetTokens,
		maxVisibleSkills:     cfg.MaxVisibleSkills,
		preloadedSkills:      preloadedSkills,
	}, nil
}

// EnsurePromptMetadata loads and caches prompt-facing skill metadata on demand.
func (s *Setup) EnsurePromptMetadata() error {
	if s == nil {
		return nil
	}
	if s.XML != "" || s.Registry == nil {
		return nil
	}

	s.metadataOnce.Do(func() {
		allSkills := s.preloadedSkills
		s.preloadedSkills = nil
		if allSkills == nil {
			var err error
			allSkills, err = s.Registry.List()
			if err != nil {
				s.metadataErr = fmt.Errorf("list skills: %w", err)
				return
			}
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
			s.alwaysEnabled,
			s.metadataBudgetTokens,
			s.maxVisibleSkills,
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
	return s.XML != ""
}

// CheckAgentsMdForSkills checks if AGENTS.md contains skill system markup.
// If true, the caller should not inject <available_skills> to avoid duplication.
func CheckAgentsMdForSkills() bool {
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}

	// Bound the search to the repository root. Walking past the repo can pick up
	// unrelated parent AGENTS.md files and incorrectly suppress skill metadata.
	repoRoot := findRepoRoot(cwd)
	if repoRoot == "" {
		repoRoot = cwd
	}

	for _, name := range []string{"AGENTS.md", "AGENTS.override.md"} {
		path := filepath.Join(repoRoot, name)
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := string(content)
		if strings.Contains(text, "<skills_system") ||
			strings.Contains(text, "<available_skills>") ||
			strings.Contains(text, "activate_skill") ||
			strings.Contains(text, "<skill>") {
			return true
		}
	}

	return false
}

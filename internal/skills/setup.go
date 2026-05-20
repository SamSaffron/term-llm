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

	promptMetadataSuppressed       bool
	promptMetadataSuppressionKnown bool

	metadataOnce sync.Once
	metadataErr  error
}

// SetupOptions controls optional startup behavior for NewSetupWithOptions.
type SetupOptions struct {
	// PromptMetadataSuppressed means the caller already has <available_skills>
	// metadata from another source (for example AGENTS.md). In that case setup only
	// verifies that at least one skill exists so activate_skill/search_skills can be
	// registered, and skips the full prompt catalog preload.
	PromptMetadataSuppressed bool

	// PromptMetadataSuppressionKnown records whether PromptMetadataSuppressed came
	// from an actual check. It lets callers avoid repeating the same AGENTS.md read
	// before deciding whether to inject metadata.
	PromptMetadataSuppressionKnown bool
}

// NewSetup initializes the skills system from config.
// Returns nil if skills are disabled or no skills are available.
func NewSetup(cfg *config.SkillsConfig) (*Setup, error) {
	return NewSetupWithOptions(cfg, SetupOptions{})
}

// NewSetupWithOptions initializes the skills system from config.
// Returns nil if skills are disabled or no skills are available.
func NewSetupWithOptions(cfg *config.SkillsConfig, opts SetupOptions) (*Setup, error) {
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

	setup := &Setup{
		Registry:                       registry,
		alwaysEnabled:                  append([]string(nil), cfg.AlwaysEnabled...),
		metadataBudgetTokens:           cfg.MetadataBudgetTokens,
		maxVisibleSkills:               cfg.MaxVisibleSkills,
		promptMetadataSuppressed:       opts.PromptMetadataSuppressed,
		promptMetadataSuppressionKnown: opts.PromptMetadataSuppressionKnown,
	}

	if opts.PromptMetadataSuppressed {
		// AGENTS.md (or an equivalent caller-owned prompt) already carries skill
		// metadata. Avoid parsing every SKILL.md just to have InjectSkillsMetadata
		// immediately skip it; a cheap first-valid-skill probe is enough to decide
		// whether the runtime skill tools should be registered.
		hasAny, err := registry.HasAnySkill()
		if err != nil {
			return nil, err
		}
		if !hasAny {
			return nil, nil
		}
		return setup, nil
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
	setup.preloadedSkills = preloadedSkills

	return setup, nil
}

// EnsurePromptMetadata loads and caches prompt-facing skill metadata on demand.
func (s *Setup) EnsurePromptMetadata() error {
	if s == nil {
		return nil
	}
	if s.XML != "" || s.Registry == nil || s.promptMetadataSuppressed {
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

// PromptMetadataSuppressed reports whether the caller already supplies skill
// metadata and whether that decision came from a completed suppression check.
func (s *Setup) PromptMetadataSuppressed() (suppressed bool, known bool) {
	if s == nil {
		return false, false
	}
	return s.promptMetadataSuppressed, s.promptMetadataSuppressionKnown
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

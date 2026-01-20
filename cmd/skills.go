package cmd

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/skills"
	skillsTui "github.com/samsaffron/term-llm/internal/tui/skills"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
)

var (
	skillsBuiltin   bool
	skillsLocal     bool
	skillsUser      bool
	skillsSource    string
	skillsBrowseTUI bool
	skillsBrowseAI  bool
)

var skillsCmd = &cobra.Command{
	Use:   "skills",
	Short: "Manage skills (portable instruction bundles)",
	Long: `List and manage Agent Skills for term-llm.

Skills are portable, cross-tool instruction bundles using the SKILL.md format.
They provide specialized capabilities and domain knowledge for completing tasks.

Skills can be discovered from multiple ecosystems:
- term-llm: ~/.config/term-llm/skills/, .skills/
- Claude Code: ~/.claude/skills/, .claude/skills/
- Codex: ~/.codex/skills/, .codex/skills/
- Gemini CLI: ~/.gemini/skills/, .gemini/skills/
- Cursor IDE: ~/.cursor/skills/, .cursor/skills/

Examples:
  term-llm skills                       # List all available skills
  term-llm skills --source user         # Only user-global skills
  term-llm skills --source claude       # Only Claude Code ecosystem skills
  term-llm skills new my-skill          # Create a new skill from template
  term-llm skills show commit-message   # Display skill details
  term-llm skills edit my-skill         # Open skill in $EDITOR
  term-llm skills copy commit my-commit # Copy for customization
  term-llm skills validate ./path/to/skill  # Validate a skill`,
	RunE: runSkillsList,
}

var skillsNewCmd = &cobra.Command{
	Use:   "new <name>",
	Short: "Create a new skill from template",
	Long: `Create a new skill directory with template files.

By default, creates the skill in the user's config directory
(~/.config/term-llm/skills/). Use --local to create in the
current project's .skills/ directory.

Examples:
  term-llm skills new my-skill        # Create in user config
  term-llm skills new my-skill --local # Create in project`,
	Args: cobra.ExactArgs(1),
	RunE: runSkillsNew,
}

var skillsShowCmd = &cobra.Command{
	Use:               "show <name>",
	Short:             "Display skill details",
	Args:              cobra.ExactArgs(1),
	RunE:              runSkillsShow,
	ValidArgsFunction: skillNameCompletion,
}

var skillsEditCmd = &cobra.Command{
	Use:               "edit <name>",
	Short:             "Open skill in $EDITOR",
	Args:              cobra.ExactArgs(1),
	RunE:              runSkillsEdit,
	ValidArgsFunction: skillNameCompletion,
}

var skillsCopyCmd = &cobra.Command{
	Use:   "copy <source> <dest>",
	Short: "Copy a skill for customization",
	Long: `Copy an existing skill to create a customized version.

This is useful for creating modified versions of ecosystem skills.

Examples:
  term-llm skills copy commit-message my-commit
  term-llm skills copy code-review my-review`,
	Args: cobra.ExactArgs(2),
	RunE: runSkillsCopy,
}

var skillsPathCmd = &cobra.Command{
	Use:   "path [name]",
	Short: "Print skill directory path",
	Long: `Print skill directories or the path to a specific skill.

Examples:
  term-llm skills path              # List all search paths
  term-llm skills path commit-message  # Print path to specific skill`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSkillsPath,
}

var skillsValidateCmd = &cobra.Command{
	Use:   "validate [path]",
	Short: "Validate a skill",
	Long: `Validate a skill's SKILL.md file for correctness.

If no path is provided, validates all discovered skills.

Examples:
  term-llm skills validate ./my-skill
  term-llm skills validate --all`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSkillsValidate,
}

var skillsBrowseCmd = &cobra.Command{
	Use:   "browse [search]",
	Short: "Browse the Skills marketplace",
	Long: `Search and browse SkillsMP.com for community skills.

Browse over 70,000 agent skills compatible with Claude Code, Codex CLI,
and other tools using the SKILL.md format.

Examples:
  term-llm skills browse                      # open interactive browser
  term-llm skills browse "code review"        # search for code review skills
  term-llm skills browse --no-tui             # simple CLI output
  term-llm skills browse --ai "help me test"  # AI semantic search

Set SKILLSMP_API_KEY environment variable for API access.`,
	RunE: runSkillsBrowse,
}

var skillsUpdateCmd = &cobra.Command{
	Use:   "update [name]",
	Short: "Update skills from their remote sources",
	Long: `Update installed skills that have provenance metadata.

Skills installed via 'skills browse' include provenance metadata that
tracks their remote source. This command fetches the latest version
and updates skills whose content has changed.

Updates are detected by comparing SHA256 hashes of description + body
(excluding metadata). Local skill names are preserved even if renamed.

Examples:
  term-llm skills update                # update all skills with provenance
  term-llm skills update my-skill       # update a specific skill
  term-llm skills update --dry-run      # show what would be updated
  term-llm skills update --force        # update even if unchanged`,
	RunE:              runSkillsUpdate,
	ValidArgsFunction: skillNameCompletion,
}

var skillsUpdateDryRun bool
var skillsUpdateForce bool
var skillsUpdateYes bool

var skillsValidateAll bool

func init() {
	skillsCmd.Flags().BoolVar(&skillsBuiltin, "builtin", false, "Show only built-in skills")
	skillsCmd.Flags().BoolVar(&skillsLocal, "local", false, "Show only project-local skills")
	skillsCmd.Flags().BoolVar(&skillsUser, "user", false, "Show only user-global skills")
	skillsCmd.Flags().StringVar(&skillsSource, "source", "", "Filter by source: local, user, builtin, claude, codex, gemini")
	skillsNewCmd.Flags().BoolVar(&skillsLocal, "local", false, "Create in project's .skills/ instead of user config")
	skillsCopyCmd.Flags().BoolVar(&skillsLocal, "local", false, "Copy to project's .skills/ instead of user config")
	skillsValidateCmd.Flags().BoolVar(&skillsValidateAll, "all", false, "Validate all discovered skills")
	skillsBrowseCmd.Flags().BoolVar(&skillsBrowseTUI, "no-tui", false, "Use simple CLI output instead of interactive browser")
	skillsBrowseCmd.Flags().BoolVar(&skillsBrowseAI, "ai", false, "Use AI semantic search instead of keyword search")
	skillsUpdateCmd.Flags().BoolVar(&skillsUpdateDryRun, "dry-run", false, "Show what would be updated without making changes")
	skillsUpdateCmd.Flags().BoolVar(&skillsUpdateForce, "force", false, "Update even if content hasn't changed")
	skillsUpdateCmd.Flags().BoolVarP(&skillsUpdateYes, "yes", "y", false, "Skip confirmation prompts")

	rootCmd.AddCommand(skillsCmd)
	skillsCmd.AddCommand(skillsNewCmd)
	skillsCmd.AddCommand(skillsShowCmd)
	skillsCmd.AddCommand(skillsEditCmd)
	skillsCmd.AddCommand(skillsCopyCmd)
	skillsCmd.AddCommand(skillsPathCmd)
	skillsCmd.AddCommand(skillsValidateCmd)
	skillsCmd.AddCommand(skillsBrowseCmd)
	skillsCmd.AddCommand(skillsUpdateCmd)
}

func getSkillsRegistry() (*skills.Registry, error) {
	cfg, err := loadConfigWithSetup()
	if err != nil {
		return nil, err
	}

	return skills.NewRegistry(skills.RegistryConfig{
		Mode:                  cfg.Skills.Mode,
		AutoInvoke:            cfg.Skills.AutoInvoke,
		MetadataBudgetTokens:  cfg.Skills.MetadataBudgetTokens,
		MaxActive:             cfg.Skills.MaxActive,
		SearchPaths:           cfg.Skills.SearchPaths,
		ExcludePaths:          cfg.Skills.ExcludePaths,
		IncludeProjectSkills:  true, // Always include for CLI listing
		IncludeCodexPaths:     cfg.Skills.IncludeCodexPaths,
		IncludeClaudePaths:    cfg.Skills.IncludeClaudePaths,
		IncludeGeminiPaths:    cfg.Skills.IncludeGeminiPaths,
		IncludeUniversalPaths: cfg.Skills.IncludeUniversalPaths,
		AlwaysEnabled:         cfg.Skills.AlwaysEnabled,
		NeverAuto:             cfg.Skills.NeverAuto,
		Disabled:              cfg.Skills.Disabled,
		UseBuiltin:            cfg.Skills.UseBuiltin,
	})
}

func runSkillsList(cmd *cobra.Command, args []string) error {
	registry, err := getSkillsRegistry()
	if err != nil {
		return fmt.Errorf("create registry: %w", err)
	}

	var skillList []*skills.Skill

	// Filter by source if flags are set
	if skillsBuiltin {
		skillList, err = registry.ListBySource(skills.SourceBuiltin)
	} else if skillsLocal {
		skillList, err = registry.ListBySource(skills.SourceLocal)
	} else if skillsUser {
		skillList, err = registry.ListBySource(skills.SourceUser)
	} else if skillsSource != "" {
		source := parseSkillSource(skillsSource)
		if source == -1 {
			return fmt.Errorf("unknown source: %s (valid: local, user, builtin, claude, codex, gemini)", skillsSource)
		}
		skillList, err = registry.ListBySource(source)
	} else {
		skillList, err = registry.List()
	}

	if err != nil {
		return fmt.Errorf("list skills: %w", err)
	}

	if len(skillList) == 0 {
		if skillsBuiltin || skillsLocal || skillsUser || skillsSource != "" {
			fmt.Println("No skills found matching filter.")
		} else {
			fmt.Println("No skills configured.")
			fmt.Println()
			fmt.Println("Create one with: term-llm skills new <name>")
			fmt.Println("Or discover from other ecosystems (Claude Code, Codex, Gemini CLI).")
		}
		return nil
	}

	// Group by source for display
	fmt.Printf("Available skills (%d):\n\n", len(skillList))

	// Track which sources we've seen
	var lastSource skills.SkillSource = -1

	for _, skill := range skillList {
		// Print source header if changed
		if skill.Source != lastSource {
			if lastSource != -1 {
				fmt.Println()
			}
			printSkillSourceHeader(skill.Source)
			lastSource = skill.Source
		}

		// Print skill info with shadow count
		fmt.Printf("    %s", skill.Name)
		if skill.Description != "" {
			// Truncate description if too long
			desc := skill.Description
			if len(desc) > 60 {
				desc = desc[:57] + "..."
			}
			fmt.Printf(" - %s", desc)
		}
		if shadowCount := registry.ShadowCount(skill.Name); shadowCount > 0 {
			fmt.Printf(" (shadows %d)", shadowCount)
		}
		fmt.Println()
	}

	fmt.Println()
	fmt.Println("Skills are automatically activated when relevant to your task.")
	return nil
}

func printSkillSourceHeader(source skills.SkillSource) {
	switch source {
	case skills.SourceLocal:
		localDir, _ := skills.GetLocalSkillsDir()
		fmt.Printf("  [local] %s/\n", localDir)
	case skills.SourceUser:
		userDir, _ := skills.GetUserSkillsDir()
		fmt.Printf("  [user] %s/\n", userDir)
	case skills.SourceBuiltin:
		fmt.Println("  [builtin]")
	case skills.SourceClaude:
		fmt.Println("  [claude] ~/.claude/skills/")
	case skills.SourceCodex:
		fmt.Println("  [codex] ~/.codex/skills/")
	case skills.SourceGemini:
		fmt.Println("  [gemini] ~/.gemini/skills/")
	case skills.SourceCursor:
		fmt.Println("  [cursor] ~/.cursor/skills/")
	}
}

func parseSkillSource(s string) skills.SkillSource {
	switch strings.ToLower(s) {
	case "local":
		return skills.SourceLocal
	case "user":
		return skills.SourceUser
	case "builtin":
		return skills.SourceBuiltin
	case "claude":
		return skills.SourceClaude
	case "codex":
		return skills.SourceCodex
	case "gemini":
		return skills.SourceGemini
	case "cursor":
		return skills.SourceCursor
	default:
		return -1
	}
}

func runSkillsNew(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Validate name
	if strings.ContainsAny(name, "/\\:*?\"<>|") {
		return fmt.Errorf("invalid skill name: %s", name)
	}

	// Determine base directory
	var baseDir string
	var err error
	if skillsLocal {
		baseDir, err = skills.GetLocalSkillsDir()
	} else {
		baseDir, err = skills.GetUserSkillsDir()
	}
	if err != nil {
		return fmt.Errorf("get skills dir: %w", err)
	}

	// Check if skill already exists
	skillDir := filepath.Join(baseDir, name)
	if _, err := os.Stat(skillDir); err == nil {
		return fmt.Errorf("skill already exists: %s", skillDir)
	}

	// Create skill
	if err := skills.CreateSkillDir(baseDir, name); err != nil {
		return fmt.Errorf("create skill: %w", err)
	}

	fmt.Printf("Created skill: %s\n\n", skillDir)
	fmt.Println("Files created:")
	fmt.Println("  SKILL.md - Skill configuration and instructions")
	fmt.Println()
	fmt.Printf("Edit with: term-llm skills edit %s\n", name)

	return nil
}

func runSkillsShow(cmd *cobra.Command, args []string) error {
	name := args[0]

	registry, err := getSkillsRegistry()
	if err != nil {
		return fmt.Errorf("create registry: %w", err)
	}

	skill, err := registry.Get(name)
	if err != nil {
		return err
	}

	// Display skill info
	fmt.Printf("Skill: %s\n", skill.Name)
	fmt.Printf("Source: %s\n", skill.Source.SourceName())
	if skill.SourcePath != "" {
		fmt.Printf("Path: %s\n", skill.SourcePath)
	}
	fmt.Println()

	fmt.Printf("Description: %s\n", skill.Description)
	fmt.Println()

	// Optional fields
	if skill.License != "" {
		fmt.Printf("License: %s\n", skill.License)
	}
	if skill.Compatibility != "" {
		fmt.Printf("Compatibility: %s\n", skill.Compatibility)
	}
	if len(skill.AllowedTools) > 0 {
		fmt.Printf("Allowed tools: %s\n", strings.Join(skill.AllowedTools, ", "))
	}
	if len(skill.Metadata) > 0 {
		fmt.Println("Metadata:")
		for k, v := range skill.Metadata {
			fmt.Printf("  %s: %s\n", k, v)
		}
	}
	if len(skill.Extras) > 0 {
		fmt.Println("Extra fields:")
		for k, v := range skill.Extras {
			fmt.Printf("  %s: %v\n", k, v)
		}
	}

	// Resources
	if skill.HasResources() {
		fmt.Println()
		fmt.Print(skill.ResourceTree())
	}

	// Instructions
	if skill.Body != "" {
		fmt.Println()
		fmt.Println("Instructions:")
		fmt.Println("---")
		// Show first 500 chars with ... if truncated
		body := skill.Body
		if len(body) > 500 {
			body = body[:500] + "\n..."
		}
		fmt.Println(body)
		fmt.Println("---")
	}

	return nil
}

func runSkillsEdit(cmd *cobra.Command, args []string) error {
	name := args[0]

	registry, err := getSkillsRegistry()
	if err != nil {
		return fmt.Errorf("create registry: %w", err)
	}

	skill, err := registry.Get(name)
	if err != nil {
		return err
	}

	// Built-in skills can't be edited directly
	if skill.Source == skills.SourceBuiltin {
		return fmt.Errorf("cannot edit built-in skill '%s'. Copy it first: term-llm skills copy %s my-%s", name, name, name)
	}

	// Get editor
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}

	// Open SKILL.md
	skillPath := filepath.Join(skill.SourcePath, "SKILL.md")
	editCmd := exec.Command(editor, skillPath)
	editCmd.Stdin = os.Stdin
	editCmd.Stdout = os.Stdout
	editCmd.Stderr = os.Stderr

	return editCmd.Run()
}

func runSkillsCopy(cmd *cobra.Command, args []string) error {
	srcName := args[0]
	destName := args[1]

	// Validate dest name
	if strings.ContainsAny(destName, "/\\:*?\"<>|") {
		return fmt.Errorf("invalid skill name: %s", destName)
	}

	registry, err := getSkillsRegistry()
	if err != nil {
		return fmt.Errorf("create registry: %w", err)
	}

	srcSkill, err := registry.Get(srcName)
	if err != nil {
		return err
	}

	// Determine destination directory
	var destDir string
	if skillsLocal {
		destDir, err = skills.GetLocalSkillsDir()
	} else {
		destDir, err = skills.GetUserSkillsDir()
	}
	if err != nil {
		return fmt.Errorf("get skills dir: %w", err)
	}

	// Check if dest already exists
	destSkillDir := filepath.Join(destDir, destName)
	if _, err := os.Stat(destSkillDir); err == nil {
		return fmt.Errorf("skill already exists: %s", destSkillDir)
	}

	// Copy the skill
	if err := skills.CopySkill(srcSkill, destDir, destName); err != nil {
		return fmt.Errorf("copy skill: %w", err)
	}

	fmt.Printf("Copied '%s' to '%s'\n", srcName, destSkillDir)
	fmt.Println()
	fmt.Printf("Edit with: term-llm skills edit %s\n", destName)

	return nil
}

func runSkillsPath(cmd *cobra.Command, args []string) error {
	if len(args) == 1 {
		// Print path to specific skill
		registry, err := getSkillsRegistry()
		if err != nil {
			return fmt.Errorf("create registry: %w", err)
		}

		skill, err := registry.Get(args[0])
		if err != nil {
			return err
		}

		fmt.Println(skill.SourcePath)
		return nil
	}

	// Print all search paths
	localDir, _ := skills.GetLocalSkillsDir()
	userDir, _ := skills.GetUserSkillsDir()

	fmt.Println("Skill directories (searched in order):")
	fmt.Println()

	// Project-local paths
	fmt.Println("  Project-local (if include_project_skills=true):")
	if _, err := os.Stat(localDir); err == nil {
		fmt.Printf("    .skills/: %s\n", localDir)
	} else {
		fmt.Printf("    .skills/: %s (not created)\n", localDir)
	}
	fmt.Println("    .claude/skills/, .codex/skills/, .gemini/skills/, .cursor/skills/")
	fmt.Println()

	// User-global paths
	fmt.Println("  User-global:")
	if _, err := os.Stat(userDir); err == nil {
		fmt.Printf("    term-llm: %s\n", userDir)
	} else {
		fmt.Printf("    term-llm: %s (not created)\n", userDir)
	}
	fmt.Println("    ~/.skills/, ~/.claude/skills/, ~/.codex/skills/, ~/.gemini/skills/, ~/.cursor/skills/")
	fmt.Println()

	fmt.Println("  Built-in: (embedded in binary)")

	return nil
}

func runSkillsValidate(cmd *cobra.Command, args []string) error {
	if len(args) == 1 {
		// Validate specific skill path
		path := args[0]

		// Check if it's a directory with SKILL.md
		if !skills.IsSkillDir(path) {
			return fmt.Errorf("no SKILL.md found in %s", path)
		}

		skill, err := skills.LoadFromDir(path, skills.SourceLocal, true)
		if err != nil {
			fmt.Printf("INVALID: %s\n", err)
			return nil
		}

		if err := skill.Validate(); err != nil {
			fmt.Printf("INVALID: %s\n", err)
			return nil
		}

		fmt.Printf("VALID: %s\n", skill.Name)
		return nil
	}

	if !skillsValidateAll {
		return fmt.Errorf("provide a path or use --all to validate all skills")
	}

	// Validate all discovered skills
	registry, err := getSkillsRegistry()
	if err != nil {
		return fmt.Errorf("create registry: %w", err)
	}

	skillList, err := registry.List()
	if err != nil {
		return fmt.Errorf("list skills: %w", err)
	}

	if len(skillList) == 0 {
		fmt.Println("No skills found to validate.")
		return nil
	}

	validCount := 0
	invalidCount := 0

	for _, skill := range skillList {
		// Load full content for validation
		fullSkill, err := registry.Get(skill.Name)
		if err != nil {
			fmt.Printf("INVALID: %s - %v\n", skill.Name, err)
			invalidCount++
			continue
		}

		if err := fullSkill.Validate(); err != nil {
			fmt.Printf("INVALID: %s - %v\n", skill.Name, err)
			invalidCount++
			continue
		}

		fmt.Printf("VALID: %s (%s)\n", skill.Name, skill.Source.SourceName())
		validCount++
	}

	fmt.Println()
	fmt.Printf("Validated %d skills: %d valid, %d invalid\n", validCount+invalidCount, validCount, invalidCount)

	return nil
}

// skillNameCompletion provides shell completion for skill names.
func skillNameCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	registry, err := getSkillsRegistry()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	skillList, err := registry.List()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	var names []string
	for _, skill := range skillList {
		if strings.HasPrefix(skill.Name, toComplete) {
			names = append(names, skill.Name)
		}
	}

	return names, cobra.ShellCompDirectiveNoFileComp
}

// SkillFlagCompletion provides shell completion for skill-related flags.
func SkillFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return skillNameCompletion(cmd, nil, toComplete)
}

func runSkillsBrowse(cmd *cobra.Command, args []string) error {
	query := ""
	if len(args) > 0 {
		query = strings.Join(args, " ")
	}

	// Use interactive TUI by default
	if !skillsBrowseTUI {
		return skillsTui.RunBrowser(query, skillsBrowseAI)
	}

	// Fallback to simple CLI output
	registry := skills.NewRemoteRegistryClient()

	if query == "" {
		fmt.Println("Search SkillsMP.com for agent skills.")
		fmt.Println()
		fmt.Println("Usage: term-llm skills browse <search>")
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println("  term-llm skills browse code-review")
		fmt.Println("  term-llm skills browse --ai \"help me write tests\"")
		return nil
	}

	searchType := "keyword"
	if skillsBrowseAI {
		searchType = "AI semantic"
	}
	fmt.Printf("Searching SkillsMP.com (%s search) for '%s'...\n\n", searchType, query)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result *skills.RemoteSearchResult
	var err error

	if skillsBrowseAI {
		result, err = registry.AISearch(ctx, query)
	} else {
		result, err = registry.Search(ctx, query)
	}

	if err != nil {
		return fmt.Errorf("search failed: %w", err)
	}

	if len(result.Skills) == 0 {
		fmt.Println("No skills found matching your query.")
		return nil
	}

	// Sort by stars descending, then alphabetically by name
	sort.Slice(result.Skills, func(i, j int) bool {
		if result.Skills[i].Stars != result.Skills[j].Stars {
			return result.Skills[i].Stars > result.Skills[j].Stars
		}
		return strings.ToLower(result.Skills[i].Name) < strings.ToLower(result.Skills[j].Name)
	})

	fmt.Printf("Found %d skills:\n\n", len(result.Skills))

	for _, skill := range result.Skills {
		// Name and stars
		fmt.Printf("  %s", skill.Name)
		if skill.Stars > 0 {
			fmt.Printf(" (★%d)", skill.Stars)
		}
		fmt.Println()

		// Author and category
		if skill.Author != "" || skill.Category != "" {
			fmt.Print("    ")
			if skill.Author != "" {
				fmt.Printf("by %s", skill.Author)
			}
			if skill.Category != "" {
				if skill.Author != "" {
					fmt.Print(" | ")
				}
				fmt.Print(skill.Category)
			}
			fmt.Println()
		}

		// Description
		if skill.Description != "" {
			desc := skill.Description
			if len(desc) > 70 {
				desc = desc[:67] + "..."
			}
			fmt.Printf("    %s\n", desc)
		}
		fmt.Println()
	}

	fmt.Println("Use the interactive browser for installation: term-llm skills browse")
	return nil
}

func runSkillsUpdate(cmd *cobra.Command, args []string) error {
	registry, err := getSkillsRegistry()
	if err != nil {
		return fmt.Errorf("create registry: %w", err)
	}

	remoteRegistry := skills.NewRemoteRegistryClient()

	// Get skills to update
	var skillsToCheck []*skills.Skill
	if len(args) == 1 {
		// Update specific skill
		skill, err := registry.Get(args[0])
		if err != nil {
			return err
		}
		skillsToCheck = []*skills.Skill{skill}
	} else {
		// Update all skills from all sources
		allSkills, err := registry.List()
		if err != nil {
			return fmt.Errorf("list skills: %w", err)
		}
		// Load full content for each skill
		for _, s := range allSkills {
			full, err := registry.Get(s.Name)
			if err == nil {
				skillsToCheck = append(skillsToCheck, full)
			}
		}
	}

	if len(skillsToCheck) == 0 {
		fmt.Println("No skills found to update.")
		fmt.Println("Searched: term-llm, Claude Code, Codex, Gemini CLI paths")
		return nil
	}

	// Find skills with provenance
	type updateCandidate struct {
		skill      *skills.Skill
		remoteName string
		rawURL     string
		repository string
	}
	var candidates []updateCandidate

	for _, skill := range skillsToCheck {
		if skill.Metadata == nil {
			continue
		}
		source := skill.Metadata["_provenance_source"]
		if source == "" {
			continue
		}

		candidates = append(candidates, updateCandidate{
			skill:      skill,
			remoteName: skill.Metadata["_provenance_remote_name"],
			rawURL:     skill.Metadata["_provenance_raw_url"],
			repository: skill.Metadata["_provenance_repository"],
		})
	}

	if len(candidates) == 0 {
		if len(args) == 1 {
			fmt.Printf("Skill '%s' has no provenance metadata (not installed from skills browse).\n", args[0])
		} else {
			fmt.Println("No skills with provenance metadata found.")
			fmt.Println("Skills installed via 'term-llm skills browse' include provenance for updates.")
		}
		return nil
	}

	fmt.Printf("Checking %d skill(s) for updates across all paths...\n", len(candidates))
	fmt.Println("(term-llm, Claude Code, Codex, Gemini CLI)")
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	reader := bufio.NewReader(os.Stdin)
	var updated, skipped, failed int

	for _, c := range candidates {
		localName := c.skill.Name
		remoteName := c.remoteName
		if remoteName == "" {
			remoteName = localName
		}

		// Determine fetch URL
		fetchURL := c.rawURL
		if fetchURL == "" && c.repository != "" {
			fetchURL = constructRawGitHubURL(c.repository)
		}
		if fetchURL == "" {
			fmt.Printf("  %-30s SKIP (no fetch URL)\n", localName)
			skipped++
			continue
		}

		// Fetch remote content
		remoteContent, err := remoteRegistry.FetchRawURL(ctx, fetchURL)
		if err != nil {
			fmt.Printf("  %-30s FAIL (%v)\n", localName, err)
			failed++
			continue
		}

		// Parse and calculate hashes
		localHash := contentHash(c.skill.Description, c.skill.Body)
		remoteSkill, err := skills.ParseSkillMDContent(string(remoteContent), true)
		if err != nil {
			fmt.Printf("  %-30s FAIL (parse: %v)\n", localName, err)
			failed++
			continue
		}
		remoteHash := contentHash(remoteSkill.Description, remoteSkill.Body)

		// Compare
		if localHash == remoteHash && !skillsUpdateForce {
			fmt.Printf("  %-30s up to date\n", localName)
			skipped++
			continue
		}

		// Show update info
		fmt.Println()
		fmt.Printf("═══ %s ═══\n", localName)
		fmt.Printf("Source: %s (%s)\n", c.skill.Source.SourceName(), c.skill.SourcePath)
		if remoteName != localName {
			fmt.Printf("Remote name: %s\n", remoteName)
		}
		fmt.Println()

		// Show diff using existing unified diff
		localDiff := "---\ndescription: " + c.skill.Description + "\n---\n" + c.skill.Body
		remoteDiff := "---\ndescription: " + remoteSkill.Description + "\n---\n" + remoteSkill.Body
		ui.PrintUnifiedDiff("SKILL.md", localDiff, remoteDiff)

		if skillsUpdateDryRun {
			fmt.Printf("\n[dry-run] Would update %s\n", localName)
			updated++
			continue
		}

		// Ask for confirmation unless --yes
		if !skillsUpdateYes {
			fmt.Printf("\nApply this update? [y/N/q] ")
			response, _ := reader.ReadString('\n')
			response = strings.TrimSpace(strings.ToLower(response))

			if response == "q" {
				fmt.Println("Aborted.")
				return nil
			}
			if response != "y" && response != "yes" {
				fmt.Printf("  %-30s SKIPPED (user declined)\n", localName)
				skipped++
				continue
			}
		}

		// Build updated content with provenance
		updatedContent := skillsTui.InjectProvenanceFromMetadata(
			remoteContent,
			c.skill.Metadata,
			localName,
		)

		// Write updated SKILL.md
		skillPath := filepath.Join(c.skill.SourcePath, "SKILL.md")
		if err := os.WriteFile(skillPath, updatedContent, 0644); err != nil {
			fmt.Printf("  %-30s FAIL (write: %v)\n", localName, err)
			failed++
			continue
		}

		fmt.Printf("  %-30s UPDATED\n", localName)
		updated++
	}

	fmt.Println()
	if skillsUpdateDryRun {
		fmt.Printf("Dry run: %d would be updated, %d up to date, %d failed\n", updated, skipped, failed)
	} else {
		fmt.Printf("Updated: %d, Skipped: %d, Failed: %d\n", updated, skipped, failed)
	}

	return nil
}

// contentHash returns SHA256 hash of description + body (normalized)
func contentHash(description, body string) string {
	// Normalize: trim whitespace, lowercase for comparison
	content := strings.TrimSpace(description) + "\n" + strings.TrimSpace(body)
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:])
}

// constructRawGitHubURL converts a GitHub repo URL to a raw content URL
func constructRawGitHubURL(repoURL string) string {
	// Handle various GitHub URL formats
	// https://github.com/user/repo -> https://raw.githubusercontent.com/user/repo/main/SKILL.md
	if !strings.Contains(repoURL, "github.com") {
		return ""
	}

	// Extract path from URL
	repoURL = strings.TrimPrefix(repoURL, "https://")
	repoURL = strings.TrimPrefix(repoURL, "http://")
	repoURL = strings.TrimPrefix(repoURL, "github.com/")
	repoURL = strings.TrimSuffix(repoURL, "/")

	// Handle /tree/ paths
	if idx := strings.Index(repoURL, "/tree/"); idx != -1 {
		parts := repoURL[:idx]
		rest := repoURL[idx+6:] // Skip "/tree/"
		return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/SKILL.md", parts, rest)
	}

	// Simple repo URL - assume main branch
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/main/SKILL.md", repoURL)
}

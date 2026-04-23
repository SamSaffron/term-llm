package agents

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

// TemplateContext holds values for template variable expansion.
type TemplateContext struct {
	// Time-related
	Date     string // YYYY-MM-DD
	DateTime string // YYYY-MM-DD HH:MM:SS
	Time     string // HH:MM
	Year     string // YYYY

	// Directory info
	Cwd     string // Full working directory
	CwdName string // Directory name only
	Home    string // Home directory
	User    string // Username

	// Git info (empty if not a git repo)
	GitBranch   string // Current branch
	GitRepo     string // Repository name
	GitDiffStat string // Output of git diff --stat (staged + unstaged)

	// File context (from -f flags)
	Files     string // Comma-separated file names
	FileCount string // Number of files

	// System
	OS string // Operating system

	// Runtime surface (chat, console, web, telegram, jobs)
	Platform string

	// Active LLM
	Provider      string // Current provider name/key
	Model         string // Current model name
	ProviderModel string // Combined provider:model string

	// Agent context
	ResourceDir  string // Directory containing agent resources (for builtin agents)
	HandoverDir  string // XDG handover directory for the current project
	HandoverPath string // Full handover file path (date + random slug)

	// Project agent instructions (dynamically discovered)
	// Searches in priority order: AGENTS.md, CLAUDE.md, .github/copilot-instructions.md,
	// .cursor/rules, CONTRIBUTING.md - returns first found
	Agents string
}

// NewTemplateContext creates a context with current environment values.
// Deprecated: Use NewTemplateContextForTemplate instead to avoid expensive operations
// when template variables are not used.
func NewTemplateContext() TemplateContext {
	return newTemplateContext(true, true, false)
}

// NewTemplateContextForTemplate creates a context, only computing expensive values
// (like git_diff_stat, agents, handover_dir) if they are actually used in the template.
func NewTemplateContextForTemplate(template string) TemplateContext {
	needsGitDiffStat := strings.Contains(template, "{{git_diff_stat}}")
	needsAgents := strings.Contains(template, "{{agents}}")
	needsHandoverDir := strings.Contains(template, "{{handover_dir}}") || strings.Contains(template, "{{handover_path}}")
	return newTemplateContext(needsGitDiffStat, needsAgents, needsHandoverDir)
}

// newTemplateContext creates a context with optional expensive computations.
func newTemplateContext(computeGitDiffStat, computeAgents, computeHandoverDir bool) TemplateContext {
	now := time.Now()

	ctx := TemplateContext{
		Date:     now.Format("2006-01-02"),
		DateTime: now.Format("2006-01-02 15:04:05"),
		Time:     now.Format("15:04"),
		Year:     now.Format("2006"),
		OS:       runtime.GOOS,
	}

	// Working directory
	if cwd, err := os.Getwd(); err == nil {
		ctx.Cwd = cwd
		ctx.CwdName = filepath.Base(cwd)
	}

	// Home directory
	if home, err := os.UserHomeDir(); err == nil {
		ctx.Home = home
	}

	// Username
	if u, err := user.Current(); err == nil {
		ctx.User = u.Username
	}

	// Git info
	ctx.GitBranch = getGitBranch()
	ctx.GitRepo = getGitRepoName()

	// Only compute git diff stat if needed (expensive: runs two git commands)
	if computeGitDiffStat {
		ctx.GitDiffStat = getGitDiffStat()
	}

	// Only load project instructions if needed (reads files from disk)
	if computeAgents {
		ctx.Agents = loadProjectInstructions()
	}

	// Compute handover directory and path if needed
	if computeHandoverDir && ctx.Cwd != "" {
		if dir, err := session.GetHandoverDir(ctx.Cwd); err == nil {
			ctx.HandoverDir = dir
		}
		if p, err := session.GetHandoverPath(ctx.Cwd, ctx.Date); err == nil {
			ctx.HandoverPath = p
		}
	}

	return ctx
}

// WithFiles adds file context to the template context.
func (c TemplateContext) WithFiles(files []string) TemplateContext {
	if len(files) > 0 {
		// Extract just file names (not full paths)
		var names []string
		for _, f := range files {
			names = append(names, filepath.Base(f))
		}
		c.Files = strings.Join(names, ", ")
		c.FileCount = itoa(len(files))
	} else {
		c.Files = ""
		c.FileCount = "0"
	}
	return c
}

// WithResourceDir sets the resource directory for an agent.
func (c TemplateContext) WithResourceDir(resourceDir string) TemplateContext {
	c.ResourceDir = resourceDir
	return c
}

// WithHandoverDir sets the handover directory for {{handover_dir}} token expansion.
func (c TemplateContext) WithHandoverDir(dir string) TemplateContext {
	c.HandoverDir = dir
	return c
}

// WithHandoverPath sets the full handover file path for {{handover_path}} token expansion.
func (c TemplateContext) WithHandoverPath(path string) TemplateContext {
	c.HandoverPath = path
	return c
}

// WithPlatform sets the runtime surface for {{platform}} token expansion.
func (c TemplateContext) WithPlatform(platform string) TemplateContext {
	c.Platform = strings.TrimSpace(platform)
	return c
}

// WithLLM sets the active provider/model for template expansion.
func (c TemplateContext) WithLLM(provider, model string) TemplateContext {
	c.Provider = strings.TrimSpace(provider)
	c.Model = strings.TrimSpace(model)
	switch {
	case c.Provider != "" && c.Model != "":
		c.ProviderModel = c.Provider + ":" + c.Model
	case c.Provider != "":
		c.ProviderModel = c.Provider
	case c.Model != "":
		c.ProviderModel = c.Model
	default:
		c.ProviderModel = ""
	}
	return c
}

// ExpandTemplate replaces {{variable}} placeholders with values from context.
func ExpandTemplate(text string, ctx TemplateContext) string {
	// Match {{variable}} patterns
	re := regexp.MustCompile(`\{\{(\w+)\}\}`)

	return re.ReplaceAllStringFunc(text, func(match string) string {
		// Extract variable name
		varName := strings.Trim(match, "{}")

		switch varName {
		case "date":
			return ctx.Date
		case "datetime":
			return ctx.DateTime
		case "time":
			return ctx.Time
		case "year":
			return ctx.Year
		case "cwd":
			return ctx.Cwd
		case "cwd_name":
			return ctx.CwdName
		case "home":
			return ctx.Home
		case "user":
			return ctx.User
		case "git_branch":
			return ctx.GitBranch
		case "git_repo":
			return ctx.GitRepo
		case "git_diff_stat":
			return ctx.GitDiffStat
		case "files":
			return ctx.Files
		case "file_count":
			return ctx.FileCount
		case "os":
			return ctx.OS
		case "platform":
			if ctx.Platform == "" {
				// Leave token untouched when platform context is unavailable.
				return match
			}
			return ctx.Platform
		case "provider":
			if ctx.Provider == "" {
				// Leave token untouched when provider context is unavailable.
				return match
			}
			return ctx.Provider
		case "model":
			if ctx.Model == "" {
				// Leave token untouched when model context is unavailable.
				return match
			}
			return ctx.Model
		case "provider_model":
			if ctx.ProviderModel == "" {
				// Leave token untouched when provider/model context is unavailable.
				return match
			}
			return ctx.ProviderModel
		case "resource_dir":
			return ctx.ResourceDir
		case "handover_dir":
			return ctx.HandoverDir
		case "handover_path":
			return ctx.HandoverPath
		case "agents":
			return ctx.Agents
		default:
			// Unknown variables are left as-is
			return match
		}
	})
}

// getGitBranch returns the current git branch, or empty string if not in a git repo.
func getGitBranch() string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// getGitRepoName returns the repository name, or empty string if not in a git repo.
func getGitRepoName() string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return filepath.Base(strings.TrimSpace(string(output)))
}

// getGitDiffStat returns a summary of changed files and line counts.
// Combines both staged and unstaged changes.
func getGitDiffStat() string {
	// Get unstaged changes (--no-color prevents ANSI codes from leaking into prompts)
	cmd := exec.Command("git", "diff", "--stat", "--stat-width=80", "--no-color")
	unstaged, _ := cmd.Output()

	// Get staged changes
	cmd = exec.Command("git", "diff", "--cached", "--stat", "--stat-width=80", "--no-color")
	staged, _ := cmd.Output()

	var result strings.Builder
	if len(staged) > 0 {
		result.WriteString("Staged changes:\n")
		result.Write(staged)
	}
	if len(unstaged) > 0 {
		if result.Len() > 0 {
			result.WriteString("\nUnstaged changes:\n")
		}
		result.Write(unstaged)
	}
	return strings.TrimSpace(result.String())
}

// itoa is a simple int to string conversion.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var neg bool
	if n < 0 {
		neg = true
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

// fallbackInstructionFiles is the list of fallback files to search when no AGENTS.md
// is found in the project hierarchy.
var fallbackInstructionFiles = []string{
	"CLAUDE.md",                       // Claude Code specific
	".github/copilot-instructions.md", // GitHub Copilot
	".cursor/rules",                   // Cursor
	"CONTRIBUTING.md",                 // General contribution guidelines
	".github/CONTRIBUTING.md",         // GitHub-style location
}

// DiscoverProjectInstructions loads project instructions using a unified algorithm:
//
//  1. User-level: ~/.config/term-llm/AGENTS.md
//  2. Project-level: hierarchical AGENTS.md from repo root → cwd
//     At each directory level, AGENTS.override.md takes precedence over AGENTS.md.
//  3. Fallback (only if step 2 found nothing): CLAUDE.md, copilot-instructions.md, etc.
//
// All found parts are joined with "\n\n---\n\n".
// Returns empty string if nothing is found.
func DiscoverProjectInstructions() string {
	return loadProjectInstructions()
}

// loadProjectInstructions implements the unified project instructions loading.
func loadProjectInstructions() string {
	var parts []string

	// 1. User-level AGENTS.md (~/.config/term-llm/AGENTS.md)
	if configDir, err := config.GetConfigDir(); err == nil {
		userAgentsPath := filepath.Join(configDir, "AGENTS.md")
		if content, err := os.ReadFile(userAgentsPath); err == nil && len(content) > 0 {
			parts = append(parts, string(content))
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return strings.Join(parts, "\n\n---\n\n")
	}

	// 2. Project-level: hierarchical AGENTS.md from repo root → cwd
	repoRoot := findGitRoot(cwd)
	if repoRoot == "" {
		repoRoot = cwd
	}

	var projectParts []string

	// Build directory list from repo root → cwd (root first)
	dirs := []string{repoRoot}
	rel, _ := filepath.Rel(repoRoot, cwd)
	if rel != "." && rel != "" {
		current := repoRoot
		for _, segment := range strings.Split(rel, string(filepath.Separator)) {
			current = filepath.Join(current, segment)
			dirs = append(dirs, current)
		}
	}

	for _, dir := range dirs {
		// AGENTS.override.md takes precedence at each level
		if content, err := os.ReadFile(filepath.Join(dir, "AGENTS.override.md")); err == nil && len(content) > 0 {
			projectParts = append(projectParts, string(content))
			continue
		}
		if content, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")); err == nil && len(content) > 0 {
			projectParts = append(projectParts, string(content))
		}
	}

	if len(projectParts) > 0 {
		parts = append(parts, projectParts...)
	} else {
		// 3. Fallback: search cwd → root for first match
		fallback := findFallbackInstructions(cwd, repoRoot)
		if fallback != "" {
			parts = append(parts, fallback)
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// findFallbackInstructions searches from cwd up to repoRoot for the first
// matching fallback instruction file (CLAUDE.md, copilot-instructions.md, etc.).
func findFallbackInstructions(cwd, repoRoot string) string {
	// Build list: cwd first, then walk up to repo root
	dirsToSearch := []string{cwd}
	if repoRoot != cwd {
		dir := filepath.Dir(cwd)
		for dir != repoRoot && strings.HasPrefix(dir, repoRoot) {
			dirsToSearch = append(dirsToSearch, dir)
			dir = filepath.Dir(dir)
		}
		dirsToSearch = append(dirsToSearch, repoRoot)
	}

	for _, dir := range dirsToSearch {
		for _, filename := range fallbackInstructionFiles {
			path := filepath.Join(dir, filename)
			content, err := os.ReadFile(path)
			if err == nil && len(content) > 0 {
				return string(content)
			}
		}
	}
	return ""
}

// findGitRoot returns the git repository root for the given path, or empty string if not in a repo.
func findGitRoot(path string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = path
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

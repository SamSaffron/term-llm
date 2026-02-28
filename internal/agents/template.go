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

	// Agent context
	ResourceDir string // Directory containing agent resources (for builtin agents)

	// Platform context
	// Identifies the runtime surface: "telegram", "web", "chat", "console", or empty.
	// Set at startup by serve/ask/chat commands; empty when not applicable.
	Platform string

	// Project agent instructions (dynamically discovered)
	// Searches in priority order: AGENTS.md, CLAUDE.md, .github/copilot-instructions.md,
	// .cursor/rules, CONTRIBUTING.md - returns first found
	Agents string
}

// NewTemplateContext creates a context with current environment values.
// Deprecated: Use NewTemplateContextForTemplate instead to avoid expensive operations
// when template variables are not used.
func NewTemplateContext() TemplateContext {
	return newTemplateContext(true, true)
}

// NewTemplateContextForTemplate creates a context, only computing expensive values
// (like git_diff_stat, agents) if they are actually used in the template.
func NewTemplateContextForTemplate(template string) TemplateContext {
	needsGitDiffStat := strings.Contains(template, "{{git_diff_stat}}")
	needsAgents := strings.Contains(template, "{{agents}}")
	return newTemplateContext(needsGitDiffStat, needsAgents)
}

// newTemplateContext creates a context with optional expensive computations.
func newTemplateContext(computeGitDiffStat, computeAgents bool) TemplateContext {
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

	// Only discover agent instructions if needed (reads files from disk)
	if computeAgents {
		ctx.Agents = discoverAgentInstructions()
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

// WithPlatform sets the platform identifier (e.g. "telegram", "web", "chat", "console").
func (c TemplateContext) WithPlatform(platform string) TemplateContext {
	c.Platform = platform
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
		case "resource_dir":
			return ctx.ResourceDir
		case "platform":
			// Leave unexpanded when platform is unknown — caller will substitute later.
			if ctx.Platform == "" {
				return match
			}
			return ctx.Platform
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

// agentInstructionFiles lists files to search for in priority order.
// Returns content from the first file found.
var agentInstructionFiles = []string{
	"AGENTS.md",                       // Emerging standard (Linux Foundation)
	"CLAUDE.md",                       // Claude Code specific
	".github/copilot-instructions.md", // GitHub Copilot
	".cursor/rules",                   // Cursor
	"CONTRIBUTING.md",                 // General contribution guidelines
	".github/CONTRIBUTING.md",         // GitHub-style location
}

// DiscoverProjectInstructions searches for project agent instruction files.
// First checks the current directory, then walks up to the git root (if any).
// Returns the content of the first file found, with a header indicating the source.
// Returns empty string if no files are found.
// Exported for use by cmd packages when project_instructions is enabled.
func DiscoverProjectInstructions() string {
	return discoverAgentInstructions()
}

// discoverAgentInstructions searches for project agent instruction files.
// First checks the current directory, then walks up to the git root (if any).
// Returns the content of the first file found, with a header indicating the source.
// Returns empty string if no files are found.
func discoverAgentInstructions() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	// Build list of directories to search: cwd first, then walk up to git root
	dirsToSearch := []string{cwd}

	// Find git root if we're in a repo
	gitRoot := findGitRoot(cwd)
	if gitRoot != "" && gitRoot != cwd {
		// Walk up from cwd to git root, adding each directory
		dir := filepath.Dir(cwd)
		for dir != gitRoot && strings.HasPrefix(dir, gitRoot) {
			dirsToSearch = append(dirsToSearch, dir)
			dir = filepath.Dir(dir)
		}
		// Add git root last
		dirsToSearch = append(dirsToSearch, gitRoot)
	}

	// Search each directory for instruction files
	for _, dir := range dirsToSearch {
		for _, filename := range agentInstructionFiles {
			path := filepath.Join(dir, filename)
			content, err := os.ReadFile(path)
			if err == nil && len(content) > 0 {
				// Return with a header indicating the source
				relPath := filename
				if dir != cwd {
					if rel, err := filepath.Rel(cwd, path); err == nil {
						relPath = rel
					}
				}
				return "# Project Instructions (from " + relPath + ")\n\n" + string(content)
			}
		}
	}

	return "" // No agent instruction files found
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

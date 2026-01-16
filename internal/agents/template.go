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
	GitBranch string // Current branch
	GitRepo   string // Repository name

	// File context (from -f flags)
	Files     string // Comma-separated file names
	FileCount string // Number of files

	// System
	OS string // Operating system

	// Agent context
	ResourceDir string // Directory containing agent resources (for builtin agents)
}

// NewTemplateContext creates a context with current environment values.
func NewTemplateContext() TemplateContext {
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
		case "files":
			return ctx.Files
		case "file_count":
			return ctx.FileCount
		case "os":
			return ctx.OS
		case "resource_dir":
			return ctx.ResourceDir
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

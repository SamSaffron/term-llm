package skills

import (
	"os"
	"path/filepath"
	"strings"
)

// buildSearchPaths constructs the ordered list of search paths.
func (r *Registry) buildSearchPaths() error {
	cwd := strings.TrimSpace(r.config.ProjectDir)
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			cwd = "."
		}
	} else if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
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
	// Running from HOME can discover the same installation in both scopes.
	for _, userPath := range r.searchPaths {
		if userPath.userInstalled {
			for i := range r.searchPaths {
				if r.searchPaths[i].path == userPath.path {
					r.searchPaths[i].userInstalled = true
				}
			}
		}
	}

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

	// Codex / open agent skills (current convention)
	r.searchPaths = append(r.searchPaths, searchPath{
		path:   filepath.Join(dir, ".agents", "skills"),
		source: SourceCodex,
	})

	// Codex (legacy convention)
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
			path:          filepath.Join(configDir, "term-llm", "skills"),
			userInstalled: true,
			source:        SourceUser,
		})
	}

	if home == "" {
		return
	}

	// Universal user skills - always included
	r.searchPaths = append(r.searchPaths, searchPath{
		path:          filepath.Join(home, ".skills"),
		userInstalled: true,
		source:        SourceUser,
	})

	// Ecosystem paths are gated by IncludeEcosystemPaths
	if !r.config.IncludeEcosystemPaths {
		return
	}

	// Claude Code user skills
	r.searchPaths = append(r.searchPaths, searchPath{
		path:          filepath.Join(home, ".claude", "skills"),
		userInstalled: true,
		source:        SourceClaude,
	})

	// Codex / open agent skills user skills (current convention)
	r.searchPaths = append(r.searchPaths, searchPath{
		path:          filepath.Join(home, ".agents", "skills"),
		userInstalled: true,
		source:        SourceCodex,
	})

	// Codex user skills (legacy convention)
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	r.searchPaths = append(r.searchPaths, searchPath{
		path:          filepath.Join(codexHome, "skills"),
		userInstalled: true,
		source:        SourceCodex,
	})

	// Gemini CLI user skills
	r.searchPaths = append(r.searchPaths, searchPath{
		path:          filepath.Join(home, ".gemini", "skills"),
		userInstalled: true,
		source:        SourceGemini,
	})

	// Cursor user skills
	r.searchPaths = append(r.searchPaths, searchPath{
		path:          filepath.Join(home, ".cursor", "skills"),
		userInstalled: true,
		source:        SourceCursor,
	})
}

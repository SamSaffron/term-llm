package project

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/samsaffron/term-llm/internal/worktree"
)

// Resolved is the canonical identity of a registered project directory.
type Resolved struct {
	CanonicalDir string `json:"canonical_dir"`
	DefaultName  string `json:"default_name"`
	Git          bool   `json:"git"`
}

// CanonicalStoragePath normalizes a project path for durable identity.
func CanonicalStoragePath(path string) string {
	return CanonicalStoragePathForOS(path, runtime.GOOS)
}

// CanonicalStoragePathForOS exposes deterministic platform normalization for tests.
func CanonicalStoragePathForOS(path, goos string) string {
	path = filepath.Clean(path)
	if goos == "windows" {
		return strings.ToLower(path)
	}
	return path
}

// SameIdentity compares two already-canonical project paths using platform identity rules.
func SameIdentity(actual, stored string) bool {
	actual = filepath.Clean(actual)
	stored = filepath.Clean(stored)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(actual, stored)
	}
	return actual == stored
}

// Resolve validates path and reduces Git checkouts and linked worktrees to the
// canonical main repository root. Non-Git directories retain their exact path.
func Resolve(ctx context.Context, path string) (Resolved, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Resolved{}, fmt.Errorf("path is required")
	}
	for _, r := range path {
		if unicode.IsControl(r) {
			return Resolved{}, fmt.Errorf("path contains control characters")
		}
	}
	if strings.HasPrefix(path, "~") || strings.Contains(path, "$") {
		return Resolved{}, fmt.Errorf("path must not use shell expansion")
	}
	if !filepath.IsAbs(path) {
		return Resolved{}, fmt.Errorf("path must be absolute")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve absolute path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve path: %w", err)
	}
	resolved = filepath.Clean(resolved)
	if filepath.Dir(resolved) == resolved {
		return Resolved{}, fmt.Errorf("filesystem root cannot be registered; use --no-projects for container-wide mode")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return Resolved{}, fmt.Errorf("inspect path: %w", err)
	}
	if !info.IsDir() {
		return Resolved{}, fmt.Errorf("path is not a directory")
	}
	dir, err := os.Open(resolved)
	if err != nil {
		return Resolved{}, fmt.Errorf("open directory: %w", err)
	}
	_ = dir.Close()

	isGit := worktree.IsGitRepoContext(ctx, resolved)
	if isGit {
		resolved, err = worktree.MainRepoRootContext(ctx, resolved)
		if err != nil {
			return Resolved{}, fmt.Errorf("resolve main repository: %w", err)
		}
		resolved, err = filepath.EvalSymlinks(resolved)
		if err != nil {
			return Resolved{}, fmt.Errorf("canonicalize main repository: %w", err)
		}
		resolved = filepath.Clean(resolved)
		if filepath.Dir(resolved) == resolved {
			return Resolved{}, fmt.Errorf("filesystem root cannot be registered; use --no-projects for container-wide mode")
		}
	}
	defaultName := filepath.Base(resolved)
	return Resolved{CanonicalDir: CanonicalStoragePath(resolved), DefaultName: defaultName, Git: isGit}, nil
}

package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// DefaultIncludeMaxDepth limits recursive {{file:...}} expansion depth.
	DefaultIncludeMaxDepth = 10
)

var fileIncludePattern = regexp.MustCompile(`\{\{\s*file\s*:\s*([^}]+?)\s*\}\}`)

// IncludeOptions controls {{file:...}} expansion behavior.
type IncludeOptions struct {
	// BaseDir is used to resolve relative include paths.
	BaseDir string
	// MaxDepth limits recursive include expansion.
	MaxDepth int
	// AllowAbsolute allows absolute include paths.
	AllowAbsolute bool
}

// ExpandFileIncludes replaces {{file:path}} directives with file contents.
// Includes are expanded recursively with cycle and depth guards.
func ExpandFileIncludes(input string, opts IncludeOptions) (string, error) {
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = DefaultIncludeMaxDepth
	}

	baseDir := strings.TrimSpace(opts.BaseDir)
	if baseDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve include base directory: %w", err)
		}
		baseDir = cwd
	}

	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve include base directory %q: %w", baseDir, err)
	}

	return expandFileIncludesRecursive(input, absBase, opts.AllowAbsolute, maxDepth, 0, nil)
}

func expandFileIncludesRecursive(input, baseDir string, allowAbsolute bool, maxDepth, depth int, stack []string) (string, error) {
	if depth > maxDepth {
		return "", fmt.Errorf("include depth %d exceeds maximum depth %d", depth, maxDepth)
	}

	matches := fileIncludePattern.FindAllStringSubmatchIndex(input, -1)
	if len(matches) == 0 {
		return input, nil
	}

	var out strings.Builder
	last := 0
	for _, m := range matches {
		fullStart, fullEnd := m[0], m[1]
		pathStart, pathEnd := m[2], m[3]

		out.WriteString(input[last:fullStart])

		rawPath := strings.TrimSpace(input[pathStart:pathEnd])
		resolvedPath, err := resolveIncludePath(rawPath, baseDir, allowAbsolute)
		if err != nil {
			return "", err
		}

		if cycle := findCycleStart(stack, resolvedPath); cycle >= 0 {
			chain := append(append([]string{}, stack[cycle:]...), resolvedPath)
			return "", fmt.Errorf("include cycle detected: %s", strings.Join(chain, " -> "))
		}

		data, err := os.ReadFile(resolvedPath)
		if err != nil {
			return "", fmt.Errorf("read include %q (%s): %w", rawPath, resolvedPath, err)
		}

		nextStack := append(append([]string{}, stack...), resolvedPath)
		expanded, err := expandFileIncludesRecursive(string(data), filepath.Dir(resolvedPath), allowAbsolute, maxDepth, depth+1, nextStack)
		if err != nil {
			return "", fmt.Errorf("expand include %q (%s): %w", rawPath, resolvedPath, err)
		}

		out.WriteString(expanded)
		last = fullEnd
	}

	out.WriteString(input[last:])
	return out.String(), nil
}

func resolveIncludePath(rawPath, baseDir string, allowAbsolute bool) (string, error) {
	if rawPath == "" {
		return "", fmt.Errorf("include path is empty")
	}

	var full string
	if filepath.IsAbs(rawPath) {
		if !allowAbsolute {
			return "", fmt.Errorf("absolute include path is not allowed: %s", rawPath)
		}
		full = filepath.Clean(rawPath)
	} else {
		full = filepath.Join(baseDir, rawPath)
	}

	abs, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("resolve include path %q: %w", rawPath, err)
	}
	return abs, nil
}

func findCycleStart(stack []string, target string) int {
	for i, s := range stack {
		if s == target {
			return i
		}
	}
	return -1
}

package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sahilm/fuzzy"
)

// FileAttachment represents an attached file
type FileAttachment struct {
	Path    string
	Name    string
	Content string
	Size    int64
}

// AttachFile reads a file and creates an attachment
func AttachFile(path string) (*FileAttachment, error) {
	// Resolve path
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve path: %w", err)
	}

	// Check if file exists
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	if info.IsDir() {
		return nil, fmt.Errorf("cannot attach directory: %s", path)
	}

	// Read file content
	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	return &FileAttachment{
		Path:    absPath,
		Name:    filepath.Base(absPath),
		Content: string(content),
		Size:    info.Size(),
	}, nil
}

// FileCompletion represents a file path completion item
type FileCompletion struct {
	Path    string
	Name    string
	IsDir   bool
	RelPath string
}

// FileCompletionSource implements fuzzy.Source for file completions
type FileCompletionSource []FileCompletion

func (f FileCompletionSource) String(i int) string {
	return f[i].Name
}

func (f FileCompletionSource) Len() int {
	return len(f)
}

// ListFiles returns files in a directory for completion
func ListFiles(dir string, query string) []FileCompletion {
	if dir == "" {
		dir = "."
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil
	}

	var files []FileCompletion
	for _, entry := range entries {
		// Skip hidden files unless query starts with .
		if strings.HasPrefix(entry.Name(), ".") && !strings.HasPrefix(query, ".") {
			continue
		}

		relPath := filepath.Join(dir, entry.Name())
		if dir == "." {
			relPath = entry.Name()
		}

		files = append(files, FileCompletion{
			Path:    filepath.Join(absDir, entry.Name()),
			Name:    entry.Name(),
			IsDir:   entry.IsDir(),
			RelPath: relPath,
		})
	}

	// Filter by query if provided
	if query != "" {
		source := FileCompletionSource(files)
		matches := fuzzy.FindFrom(query, source)

		var filtered []FileCompletion
		for _, match := range matches {
			filtered = append(filtered, files[match.Index])
		}

		// Also include prefix matches
		queryLower := strings.ToLower(query)
		for _, f := range files {
			if strings.HasPrefix(strings.ToLower(f.Name), queryLower) {
				// Check if already included
				found := false
				for _, existing := range filtered {
					if existing.Path == f.Path {
						found = true
						break
					}
				}
				if !found {
					filtered = append(filtered, f)
				}
			}
		}

		return filtered
	}

	return files
}

// ExpandGlob expands a glob pattern to matching files
func ExpandGlob(pattern string) ([]string, error) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid glob pattern: %w", err)
	}

	if len(matches) == 0 {
		// Try as a literal path
		if _, err := os.Stat(pattern); err == nil {
			return []string{pattern}, nil
		}
		return nil, fmt.Errorf("no files match pattern: %s", pattern)
	}

	// Filter out directories
	var files []string
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			files = append(files, match)
		}
	}

	return files, nil
}

// FormatFileSize returns a human-readable file size
func FormatFileSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.1fGB", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.1fMB", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.1fKB", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

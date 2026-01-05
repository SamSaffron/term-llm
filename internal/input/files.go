package input

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/clipboard"
	"golang.org/x/term"
)

// FileContent represents content read from a file or other source
type FileContent struct {
	Path    string // File path or special identifier (e.g., "clipboard")
	Content string // The text content
}

// ReadFiles reads content from the given paths
// Special values:
//   - "clipboard": reads text from system clipboard
//   - Glob patterns (e.g., "*.go"): expands and reads all matching files
//   - Regular paths: reads file content directly
func ReadFiles(paths []string) ([]FileContent, error) {
	var result []FileContent

	for _, path := range paths {
		// Handle special "clipboard" value
		if strings.ToLower(path) == "clipboard" {
			content, err := clipboard.ReadText()
			if err != nil {
				return nil, fmt.Errorf("failed to read clipboard: %w", err)
			}
			result = append(result, FileContent{
				Path:    "clipboard",
				Content: content,
			})
			continue
		}

		// Expand ~ to home directory
		expandedPath := expandPath(path)

		// Try glob expansion
		matches, err := filepath.Glob(expandedPath)
		if err != nil {
			return nil, fmt.Errorf("invalid glob pattern %q: %w", path, err)
		}

		// If no matches but no wildcard chars, treat as literal path
		if len(matches) == 0 {
			if !containsGlobChars(path) {
				matches = []string{expandedPath}
			} else {
				// Glob pattern matched nothing
				continue
			}
		}

		// Read each matched file
		for _, match := range matches {
			// Skip directories
			info, err := os.Stat(match)
			if err != nil {
				return nil, fmt.Errorf("failed to stat %q: %w", match, err)
			}
			if info.IsDir() {
				continue
			}

			content, err := os.ReadFile(match)
			if err != nil {
				return nil, fmt.Errorf("failed to read %q: %w", match, err)
			}

			result = append(result, FileContent{
				Path:    match,
				Content: string(content),
			})
		}
	}

	return result, nil
}

// HasStdin returns true if stdin has data available (not a TTY)
func HasStdin() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	// Check if stdin is a pipe or has data
	return (fi.Mode()&os.ModeCharDevice) == 0 || fi.Size() > 0
}

// ReadStdin reads all content from stdin
// Returns empty string if stdin is a TTY or has no data
func ReadStdin() (string, error) {
	if !HasStdin() {
		return "", nil
	}

	// Check if stdin is a terminal
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return "", nil
	}

	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("failed to read stdin: %w", err)
	}

	return string(data), nil
}

// FormatFilesXML formats file contents with prompt-safe delimiters.
func FormatFilesXML(files []FileContent, stdin string) string {
	if len(files) == 0 && stdin == "" {
		return ""
	}

	var sb strings.Builder

	for _, f := range files {
		sb.WriteString("<<<<< FILE: ")
		sb.WriteString(f.Path)
		sb.WriteString(" >>>>>\n")
		sb.WriteString(f.Content)
		if !strings.HasSuffix(f.Content, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("<<<<< END FILE >>>>>\n")
	}

	if stdin != "" {
		sb.WriteString("<<<<< STDIN >>>>>\n")
		sb.WriteString(stdin)
		if !strings.HasSuffix(stdin, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("<<<<< END STDIN >>>>>\n")
	}

	return strings.TrimSuffix(sb.String(), "\n")
}

// expandPath expands ~ to home directory
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// containsGlobChars returns true if the path contains glob metacharacters
func containsGlobChars(path string) bool {
	return strings.ContainsAny(path, "*?[")
}

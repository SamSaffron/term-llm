package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/sahilm/fuzzy"
)

// FileAttachment represents an attached file
type FileAttachment struct {
	Path    string
	Name    string
	Content string
	Size    int64
}

// maxAttachmentSize is the maximum file size allowed for attachments (2MB).
// This prevents accidental attachment of very large files that could hang the TUI
// or consume excessive memory.
const maxAttachmentSize = 2 * 1024 * 1024

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

	// Check file size before reading
	if info.Size() > maxAttachmentSize {
		return nil, fmt.Errorf("file too large: %s (%s, max %s)",
			path, FormatFileSize(info.Size()), FormatFileSize(maxAttachmentSize))
	}

	// Read file content
	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	// Check for binary content (NUL bytes indicate binary file)
	if isBinaryContent(content) {
		return nil, fmt.Errorf("cannot attach binary file: %s", path)
	}

	return &FileAttachment{
		Path:    absPath,
		Name:    filepath.Base(absPath),
		Content: string(content),
		Size:    info.Size(),
	}, nil
}

// isBinaryContent checks if content appears to be binary (contains NUL bytes).
// Only checks the first 8KB for efficiency.
func isBinaryContent(content []byte) bool {
	checkLen := len(content)
	if checkLen > 8192 {
		checkLen = 8192
	}
	for i := 0; i < checkLen; i++ {
		if content[i] == 0 {
			return true
		}
	}
	return false
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

	// Sort for deterministic ordering across filesystems
	sort.Strings(files)

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

// attachFile attempts to attach a file, prompting for directory approval if needed
func (m *Model) attachFile(path string) (tea.Model, tea.Cmd) {
	// Check if the path is approved
	if !m.approvedDirs.IsPathApproved(path) {
		// Need approval - show dialog
		options := GetParentOptions(path)
		m.pendingFilePath = path
		m.dialog.ShowDirApproval(path, options)
		return m, nil
	}

	// Path is approved, attach the file
	attachment, err := AttachFile(path)
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to attach file: %v", err))
	}

	// Check if already attached
	for _, f := range m.files {
		if f.Path == attachment.Path {
			return m.showSystemMessage(fmt.Sprintf("File already attached: %s", attachment.Name))
		}
	}

	m.files = append(m.files, *attachment)
	return m.showSystemMessage(fmt.Sprintf("Attached: %s (%s)", attachment.Name, FormatFileSize(attachment.Size)))
}

// attachFiles attaches multiple files from a glob pattern
func (m *Model) attachFiles(pattern string) (tea.Model, tea.Cmd) {
	// Expand the glob pattern
	paths, err := ExpandGlob(pattern)
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to expand pattern: %v", err))
	}

	if len(paths) == 0 {
		return m.showSystemMessage(fmt.Sprintf("No files match pattern: %s", pattern))
	}

	// For multiple files, check approval first
	for _, path := range paths {
		if !m.approvedDirs.IsPathApproved(path) {
			// Need approval for this path - show dialog for first unapproved
			options := GetParentOptions(path)
			m.pendingFilePath = path
			m.dialog.ShowDirApproval(path, options)
			return m, nil
		}
	}

	// All paths approved, attach them
	var attached []string
	var totalSize int64
	for _, path := range paths {
		attachment, err := AttachFile(path)
		if err != nil {
			continue // Skip files that can't be read
		}

		// Check if already attached
		alreadyAttached := false
		for _, f := range m.files {
			if f.Path == attachment.Path {
				alreadyAttached = true
				break
			}
		}
		if !alreadyAttached {
			m.files = append(m.files, *attachment)
			attached = append(attached, attachment.Name)
			totalSize += attachment.Size
		}
	}

	if len(attached) == 0 {
		return m.showSystemMessage("No new files attached (all may already be attached or unreadable).")
	}

	if len(attached) == 1 {
		return m.showSystemMessage(fmt.Sprintf("Attached: %s (%s)", attached[0], FormatFileSize(totalSize)))
	}
	return m.showSystemMessage(fmt.Sprintf("Attached %d files (%s):\n- %s",
		len(attached), FormatFileSize(totalSize), strings.Join(attached, "\n- ")))
}

// clearFiles removes all attached files
func (m *Model) clearFiles() {
	m.files = nil
}

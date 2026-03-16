package image

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/clipboard"
	"github.com/samsaffron/term-llm/internal/pathutil"
)

// SaveImage saves image data to the configured output directory
// Returns the path where the image was saved
func SaveImage(data []byte, outputDir, prompt string) (string, error) {
	dir := ExpandPath(outputDir)

	// Ensure directory exists
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create output directory: %w", err)
	}

	filename := generateFilename(prompt)
	path := filepath.Join(dir, filename)

	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write image: %w", err)
	}

	return path, nil
}

// DisplayImage displays the image in terminal using rasterm
// Returns nil if terminal doesn't support images (graceful degradation)
func DisplayImage(imagePath string) error {
	cap := DetectCapability()
	if cap == CapNone {
		return nil // graceful degradation
	}
	if err := RenderImageToWriter(os.Stdout, imagePath); err != nil {
		return err
	}
	// Print CR+LF to reset cursor position after image
	fmt.Fprint(os.Stdout, "\r\n")
	return nil
}

// CopyToClipboard copies image to clipboard (platform-aware)
// Attempts to copy actual image data, not just the path
func CopyToClipboard(imagePath string, imageData []byte) error {
	return clipboard.CopyImage(imagePath, imageData)
}

// ReadFromClipboard reads image data from the system clipboard
// Returns the image data and an error if clipboard doesn't contain an image
func ReadFromClipboard() ([]byte, error) {
	return clipboard.ReadImage()
}

// ExpandPath expands ~ and ~username to the appropriate home directory.
// It delegates to pathutil.Expand and returns the original path on error.
// Deprecated: prefer pathutil.Expand or pathutil.MustExpand directly.
func ExpandPath(path string) string {
	return pathutil.MustExpand(path)
}

// generateFilename creates a filename from timestamp and sanitized prompt
func generateFilename(prompt string) string {
	timestamp := time.Now().Format("20060102-150405")
	safe := sanitizeForFilename(prompt)
	if len(safe) > 30 {
		safe = safe[:30]
	}
	if safe == "" {
		safe = "image"
	}
	return fmt.Sprintf("%s-%s.png", timestamp, safe)
}

// sanitizeForFilename removes/replaces characters unsafe for filenames
func sanitizeForFilename(s string) string {
	// Replace spaces with underscores
	s = strings.ReplaceAll(s, " ", "_")
	// Remove non-alphanumeric except underscore and hyphen
	reg := regexp.MustCompile(`[^a-zA-Z0-9_-]`)
	s = reg.ReplaceAllString(s, "")
	// Collapse multiple underscores
	reg = regexp.MustCompile(`_+`)
	s = reg.ReplaceAllString(s, "_")
	// Trim leading/trailing underscores
	s = strings.Trim(s, "_")
	return strings.ToLower(s)
}

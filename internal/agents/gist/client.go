// Package gist provides GitHub Gist operations through the gh CLI.
// It is used by explicit Gist import/export and the built-in sharing provider.
package gist

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/samsaffron/term-llm/internal/procutil"
)

var (
	ErrDependencyMissing = errors.New("gist dependency missing")
	ErrAuthRequired      = errors.New("gist authentication required")
	ErrProvider          = errors.New("gist provider error")
)

const ProviderID = "github"

// Gist represents a GitHub Gist.
type Gist struct {
	ID          string
	URL         string
	Description string
	Public      bool
	Files       map[string]string // filename -> content
}

// Client wraps gh CLI operations for gist management.
type Client struct {
	// For testing: override the command executor
	execCommand func(name string, args ...string) *exec.Cmd
}

// NewClient creates a new gist client.
// Returns an error if gh CLI is not available.
func NewClient() (*Client, error) {
	c := &Client{
		execCommand: exec.Command,
	}

	if err := c.checkGH(); err != nil {
		return nil, err
	}

	return c, nil
}

// checkGH verifies gh CLI is installed and the active account can access GitHub.
func (c *Client) checkGH() error {
	// Checking the API mirrors the account selection used by "gh gist". In
	// contrast, "gh auth status" fails when any configured account is invalid,
	// including inactive accounts that will not be used for the Gist operation.
	cmd := c.execCommand("gh", "api", "user", "--silent")
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return fmt.Errorf("%w: gh CLI not found. Install from: https://cli.github.com", ErrDependencyMissing)
		}
		return fmt.Errorf("%w: gh CLI not authenticated. Run: gh auth login --hostname github.com", ErrAuthRequired)
	}

	return nil
}

// ParseGistRef extracts a gist ID from a URL or raw ID.
// Supports:
//   - abc123def456
//   - https://gist.github.com/user/abc123def456
//   - gist.github.com/user/abc123def456
func ParseGistRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)

	// Raw gist ID (alphanumeric)
	if matched, _ := regexp.MatchString(`^[a-f0-9]+$`, ref); matched {
		return ref, nil
	}

	// URL format
	re := regexp.MustCompile(`(?:https?://)?gist\.github\.com/[^/]+/([a-f0-9]+)`)
	matches := re.FindStringSubmatch(ref)
	if len(matches) == 2 {
		return matches[1], nil
	}

	return "", fmt.Errorf("invalid gist reference: %s", ref)
}

// Create creates a new gist with the given files.
// Returns the created gist with ID and URL populated.
func (c *Client) Create(description string, public bool, files map[string]string) (*Gist, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("no files to upload")
	}

	// Create temp directory for files
	tmpDir, err := os.MkdirTemp("", "gist-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure temp dir: %w", err)
	}

	// Write files to temp directory
	var filePaths []string
	for name, content := range files {
		path := filepath.Join(tmpDir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return nil, fmt.Errorf("write temp file %s: %w", name, err)
		}
		filePaths = append(filePaths, path)
	}

	// Build command
	args := []string{"gist", "create"}
	if description != "" {
		args = append(args, "--desc", description)
	}
	if public {
		args = append(args, "--public")
	}
	args = append(args, filePaths...)

	cmd := c.execCommand("gh", args...)
	var stdout bytes.Buffer
	stderr := procutil.NewLimitedBuffer(64 << 10)
	cmd.Stdout = &stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		logGistDiagnostic("create", stderr.String())
		return nil, fmt.Errorf("%w: gh gist create failed", ErrProvider)
	}

	// Parse output URL
	url := strings.TrimSpace(stdout.String())
	id, err := ParseGistRef(url)
	if err != nil {
		return nil, fmt.Errorf("parse gist URL from output: %w", err)
	}

	return &Gist{
		ID:          id,
		URL:         url,
		Description: description,
		Public:      public,
		Files:       files,
	}, nil
}

// Update updates an existing gist with new file contents.
func (c *Client) Update(gistID string, files map[string]string) error {
	if len(files) == 0 {
		return fmt.Errorf("no files to update")
	}

	// Create temp directory for files
	tmpDir, err := os.MkdirTemp("", "gist-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		return fmt.Errorf("secure temp dir: %w", err)
	}

	// Update each file using gh gist edit --add
	for name, content := range files {
		path := filepath.Join(tmpDir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return fmt.Errorf("write temp file %s: %w", name, err)
		}

		cmd := c.execCommand("gh", "gist", "edit", gistID, "--add", path)
		stderr := procutil.NewLimitedBuffer(64 << 10)
		cmd.Stderr = stderr

		if err := cmd.Run(); err != nil {
			logGistDiagnostic("update", stderr.String())
			return fmt.Errorf("%w: gh gist edit failed for %s", ErrProvider, name)
		}
	}

	return nil
}

// gistAPIResponse represents the JSON response from GitHub API
type gistAPIResponse struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Public      bool   `json:"public"`
	HTMLURL     string `json:"html_url"`
	Files       map[string]struct {
		Filename string `json:"filename"`
		Content  string `json:"content"`
	} `json:"files"`
}

// Get fetches a gist by ID.
func (c *Client) Get(gistID string) (*Gist, error) {
	cmd := c.execCommand("gh", "api", fmt.Sprintf("/gists/%s", gistID))
	var stdout bytes.Buffer
	stderr := procutil.NewLimitedBuffer(64 << 10)
	cmd.Stdout = &stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		logGistDiagnostic("get", stderr.String())
		return nil, fmt.Errorf("%w: gh api failed", ErrProvider)
	}

	var resp gistAPIResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("parse gist response: %w", err)
	}

	files := make(map[string]string)
	for _, f := range resp.Files {
		files[f.Filename] = f.Content
	}

	return &Gist{
		ID:          resp.ID,
		URL:         resp.HTMLURL,
		Description: resp.Description,
		Public:      resp.Public,
		Files:       files,
	}, nil
}

func logGistDiagnostic(operation, diagnostic string) {
	if diagnostic == "" {
		return
	}
	const maxBytes = 4096
	if len(diagnostic) > maxBytes {
		diagnostic = diagnostic[:maxBytes] + "…(truncated)"
	}
	log.Printf("[gist] gh %s diagnostic: %q", operation, diagnostic)
}

// GetURL returns the web URL for a gist ID.
func GetURL(gistID string) string {
	return fmt.Sprintf("https://gist.github.com/%s", gistID)
}

// PreviewURL returns the first-party rich transcript URL for a valid Gist ID.
func PreviewURL(gistID string) string {
	if matched, _ := regexp.MatchString(`^[a-f0-9]+$`, gistID); !matched {
		return ""
	}
	return "https://gisthost.github.io/?" + gistID + "/index.html"
}

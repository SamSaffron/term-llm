package chat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ApprovedDirs stores the list of approved directories
type ApprovedDirs struct {
	ApprovedAt  time.Time `json:"approved_at"`
	Directories []string  `json:"directories"`
}

// getApprovedDirsPath returns the path to the approved_dirs.json file
func getApprovedDirsPath() (string, error) {
	// Use XDG_DATA_HOME if set, otherwise ~/.local/share
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		dataHome = filepath.Join(home, ".local", "share")
	}

	dir := filepath.Join(dataHome, "term-llm")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create data directory: %w", err)
	}

	return filepath.Join(dir, "approved_dirs.json"), nil
}

// LoadApprovedDirs loads the approved directories from disk
func LoadApprovedDirs() (*ApprovedDirs, error) {
	path, err := getApprovedDirsPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ApprovedDirs{
				ApprovedAt:  time.Now(),
				Directories: []string{},
			}, nil
		}
		return nil, fmt.Errorf("failed to read approved dirs: %w", err)
	}

	var dirs ApprovedDirs
	if err := json.Unmarshal(data, &dirs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal approved dirs: %w", err)
	}

	return &dirs, nil
}

// SaveApprovedDirs saves the approved directories to disk
func SaveApprovedDirs(dirs *ApprovedDirs) error {
	path, err := getApprovedDirsPath()
	if err != nil {
		return err
	}

	dirs.ApprovedAt = time.Now()

	data, err := json.MarshalIndent(dirs, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal approved dirs: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write approved dirs: %w", err)
	}

	return nil
}

// IsPathApproved checks if a path is within an approved directory
func (d *ApprovedDirs) IsPathApproved(path string) bool {
	// Resolve to absolute path
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	// Resolve symlinks
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// If file doesn't exist yet, use the parent directory
		realPath = absPath
	}

	// Check against approved directories
	for _, dir := range d.Directories {
		// Resolve the approved directory too
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}

		// Check if path is under this directory
		if strings.HasPrefix(realPath, absDir+string(filepath.Separator)) || realPath == absDir {
			return true
		}
	}

	return false
}

// AddDirectory adds a directory to the approved list
func (d *ApprovedDirs) AddDirectory(dir string) error {
	// Resolve to absolute path
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("failed to resolve path: %w", err)
	}

	// Block root directory
	if absDir == "/" {
		return fmt.Errorf("cannot approve root directory")
	}

	// Check if already approved
	for _, existing := range d.Directories {
		existingAbs, _ := filepath.Abs(existing)
		if existingAbs == absDir {
			return nil // Already approved
		}
	}

	d.Directories = append(d.Directories, absDir)
	return SaveApprovedDirs(d)
}

// RemoveDirectory removes a directory from the approved list
func (d *ApprovedDirs) RemoveDirectory(dir string) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("failed to resolve path: %w", err)
	}

	var newDirs []string
	found := false
	for _, existing := range d.Directories {
		existingAbs, _ := filepath.Abs(existing)
		if existingAbs != absDir {
			newDirs = append(newDirs, existing)
		} else {
			found = true
		}
	}

	if !found {
		return fmt.Errorf("directory not in approved list: %s", dir)
	}

	d.Directories = newDirs
	return SaveApprovedDirs(d)
}

// GetParentOptions returns the directory and its parents for approval choices
func GetParentOptions(path string) []string {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return []string{path}
	}

	// Get the directory containing the file
	dir := filepath.Dir(absPath)

	var options []string
	options = append(options, dir)

	// Add parent directories (up to 3 levels)
	for i := 0; i < 3; i++ {
		parent := filepath.Dir(dir)
		if parent == dir || parent == "/" {
			break
		}
		options = append(options, parent)
		dir = parent
	}

	return options
}

// DirApprovalRequest represents a pending directory approval request
type DirApprovalRequest struct {
	Path       string   // The file path that triggered the request
	Options    []string // Directory options to approve
	OnApprove  func(dir string)
	OnDeny     func()
}

package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	claudeConfigDirEnv    = "CLAUDE_CONFIG_DIR"
	claudeProjectsDirName = "projects"
)

// claudeEntry represents a single entry in a Claude Code JSONL file
type claudeEntry struct {
	Timestamp string  `json:"timestamp"`
	SessionID string  `json:"sessionId"`
	RequestID string  `json:"requestId"`
	CostUSD   float64 `json:"costUSD"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	IsAPIErrorMessage bool `json:"isApiErrorMessage"`
}

// LoadClaudeUsage loads usage data from Claude Code JSONL files
func LoadClaudeUsage() LoadResult {
	var result LoadResult

	paths, err := getClaudePaths()
	if err != nil {
		result.Errors = append(result.Errors, err)
		return result
	}

	if len(paths) == 0 {
		result.MissingDirectories = append(result.MissingDirectories, "~/.config/claude/projects")
		return result
	}

	seen := make(map[string]bool) // Deduplication by messageID + requestID

	for _, basePath := range paths {
		projectsPath := filepath.Join(basePath, claudeProjectsDirName)
		if _, err := os.Stat(projectsPath); os.IsNotExist(err) {
			result.MissingDirectories = append(result.MissingDirectories, projectsPath)
			continue
		}

		// Find all JSONL files
		err := filepath.Walk(projectsPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // Skip errors
			}
			if info.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".jsonl") {
				return nil
			}

			entries, errs := loadClaudeFile(path, seen)
			result.Entries = append(result.Entries, entries...)
			result.Errors = append(result.Errors, errs...)
			return nil
		})
		if err != nil {
			result.Errors = append(result.Errors, err)
		}
	}

	return result
}

// getClaudePaths returns the directories to search for Claude usage data
func getClaudePaths() ([]string, error) {
	var paths []string
	seen := make(map[string]bool)

	// Check environment variable first (supports comma-separated paths)
	if envPaths := os.Getenv(claudeConfigDirEnv); envPaths != "" {
		for _, p := range strings.Split(envPaths, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			absPath, err := filepath.Abs(p)
			if err != nil {
				continue
			}
			projectsPath := filepath.Join(absPath, claudeProjectsDirName)
			if info, err := os.Stat(projectsPath); err == nil && info.IsDir() {
				if !seen[absPath] {
					seen[absPath] = true
					paths = append(paths, absPath)
				}
			}
		}
		if len(paths) > 0 {
			return paths, nil
		}
	}

	// Check default paths
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	defaultPaths := []string{
		filepath.Join(homeDir, ".config", "claude"),
		filepath.Join(homeDir, ".claude"),
	}

	for _, p := range defaultPaths {
		projectsPath := filepath.Join(p, claudeProjectsDirName)
		if info, err := os.Stat(projectsPath); err == nil && info.IsDir() {
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}

	return paths, nil
}

// loadClaudeFile loads entries from a single JSONL file
func loadClaudeFile(path string, seen map[string]bool) ([]UsageEntry, []error) {
	var entries []UsageEntry
	var errors []error

	file, err := os.Open(path)
	if err != nil {
		return nil, []error{err}
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// Increase buffer for large lines
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		var entry claudeEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue // Skip invalid lines
		}

		// Skip API error messages
		if entry.IsAPIErrorMessage {
			continue
		}

		// Skip entries without usage data
		if entry.Message.Usage.InputTokens == 0 && entry.Message.Usage.OutputTokens == 0 {
			continue
		}

		// Deduplication by messageID + requestID
		dedupeKey := entry.Message.ID + ":" + entry.RequestID
		if dedupeKey != ":" && seen[dedupeKey] {
			continue
		}
		if dedupeKey != ":" {
			seen[dedupeKey] = true
		}

		// Parse timestamp
		ts, err := time.Parse(time.RFC3339, entry.Timestamp)
		if err != nil {
			// Try alternative formats
			ts, err = time.Parse("2006-01-02T15:04:05.000Z", entry.Timestamp)
			if err != nil {
				continue
			}
		}

		entries = append(entries, UsageEntry{
			Timestamp:        ts,
			SessionID:        entry.SessionID,
			Model:            entry.Message.Model,
			InputTokens:      entry.Message.Usage.InputTokens,
			OutputTokens:     entry.Message.Usage.OutputTokens,
			CacheWriteTokens: entry.Message.Usage.CacheCreationInputTokens,
			CacheReadTokens:  entry.Message.Usage.CacheReadInputTokens,
			CostUSD:          entry.CostUSD,
			Provider:         ProviderClaudeCode,
		})
	}

	if err := scanner.Err(); err != nil {
		errors = append(errors, err)
	}

	return entries, errors
}

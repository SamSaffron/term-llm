package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	geminiBaseDir  = ".gemini"
	geminiTmpDir   = "tmp"
	geminiChatsDir = "chats"
)

// geminiConversation represents a Gemini CLI session file
type geminiConversation struct {
	SessionID   string          `json:"sessionId"`
	ProjectHash string          `json:"projectHash"`
	StartTime   string          `json:"startTime"`
	LastUpdated string          `json:"lastUpdated"`
	Messages    []geminiMessage `json:"messages"`
}

// geminiMessage represents a message in a Gemini conversation
type geminiMessage struct {
	ID        string        `json:"id"`
	Timestamp string        `json:"timestamp"`
	Type      string        `json:"type"` // "user", "gemini", "info", "error"
	Model     string        `json:"model"`
	Tokens    *geminiTokens `json:"tokens"`
}

// geminiTokens represents token usage for a Gemini message
type geminiTokens struct {
	Input    int `json:"input"`
	Output   int `json:"output"`
	Cached   int `json:"cached"`
	Thoughts int `json:"thoughts"`
	Tool     int `json:"tool"`
	Total    int `json:"total"`
}

// LoadGeminiUsage loads usage data from Gemini CLI session files
func LoadGeminiUsage() LoadResult {
	var result LoadResult

	homeDir, err := os.UserHomeDir()
	if err != nil {
		result.Errors = append(result.Errors, err)
		return result
	}

	geminiPath := filepath.Join(homeDir, geminiBaseDir, geminiTmpDir)
	if _, err := os.Stat(geminiPath); os.IsNotExist(err) {
		result.MissingDirectories = append(result.MissingDirectories, geminiPath)
		return result
	}

	// Iterate over project hash directories
	projectDirs, err := os.ReadDir(geminiPath)
	if err != nil {
		result.Errors = append(result.Errors, err)
		return result
	}

	for _, projectDir := range projectDirs {
		if !projectDir.IsDir() {
			continue
		}

		chatsPath := filepath.Join(geminiPath, projectDir.Name(), geminiChatsDir)
		if _, err := os.Stat(chatsPath); os.IsNotExist(err) {
			continue
		}

		// Find session files
		files, err := os.ReadDir(chatsPath)
		if err != nil {
			result.Errors = append(result.Errors, err)
			continue
		}

		for _, file := range files {
			if file.IsDir() {
				continue
			}
			if !strings.HasPrefix(file.Name(), "session-") || !strings.HasSuffix(file.Name(), ".json") {
				continue
			}

			filePath := filepath.Join(chatsPath, file.Name())
			entries, errs := loadGeminiFile(filePath)
			result.Entries = append(result.Entries, entries...)
			result.Errors = append(result.Errors, errs...)
		}
	}

	return result
}

// loadGeminiFile loads entries from a single Gemini session file
func loadGeminiFile(path string) ([]UsageEntry, []error) {
	var entries []UsageEntry
	var errors []error

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, []error{err}
	}

	var conversation geminiConversation
	if err := json.Unmarshal(data, &conversation); err != nil {
		return nil, []error{err}
	}

	for _, msg := range conversation.Messages {
		// Only process gemini messages with token data
		if msg.Type != "gemini" || msg.Tokens == nil {
			continue
		}

		// Skip messages without actual token usage
		if msg.Tokens.Input == 0 && msg.Tokens.Output == 0 {
			continue
		}

		// Parse timestamp
		ts, err := time.Parse(time.RFC3339, msg.Timestamp)
		if err != nil {
			ts, err = time.Parse("2006-01-02T15:04:05.000Z", msg.Timestamp)
			if err != nil {
				continue
			}
		}

		entries = append(entries, UsageEntry{
			Timestamp:       ts,
			SessionID:       conversation.SessionID,
			Model:           msg.Model,
			InputTokens:     msg.Tokens.Input,
			OutputTokens:    msg.Tokens.Output,
			CacheReadTokens: msg.Tokens.Cached,
			ReasoningTokens: msg.Tokens.Thoughts, // Map thoughts to reasoning
			Provider:        ProviderGeminiCLI,
		})
	}

	return entries, errors
}

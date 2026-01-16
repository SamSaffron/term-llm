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
	codexHomeEnv       = "CODEX_HOME"
	codexDefaultDir    = ".codex"
	codexSessionSubdir = "sessions"
	codexFallbackModel = "gpt-5"
)

// codexEntry represents an entry in a Codex JSONL file
type codexEntry struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// codexTurnContext represents a turn_context payload
type codexTurnContext struct {
	Model string `json:"model"`
}

// codexTokenPayload represents an event_msg payload with token_count
type codexTokenPayload struct {
	Type string `json:"type"`
	Info struct {
		Model           string         `json:"model"`
		ModelName       string         `json:"model_name"`
		LastTokenUsage  *codexRawUsage `json:"last_token_usage"`
		TotalTokenUsage *codexRawUsage `json:"total_token_usage"`
		Metadata        struct {
			Model string `json:"model"`
		} `json:"metadata"`
	} `json:"info"`
}

// codexRawUsage represents raw token usage from Codex
type codexRawUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

// LoadCodexUsage loads usage data from Codex CLI JSONL files
func LoadCodexUsage() LoadResult {
	var result LoadResult

	codexPath, err := getCodexPath()
	if err != nil {
		result.Errors = append(result.Errors, err)
		return result
	}

	sessionsPath := filepath.Join(codexPath, codexSessionSubdir)
	if _, err := os.Stat(sessionsPath); os.IsNotExist(err) {
		result.MissingDirectories = append(result.MissingDirectories, sessionsPath)
		return result
	}

	// Find all JSONL files
	err = filepath.Walk(sessionsPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		if info.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".jsonl") {
			return nil
		}

		// Extract session ID from path
		relPath, _ := filepath.Rel(sessionsPath, path)
		sessionID := strings.TrimSuffix(relPath, ".jsonl")
		sessionID = strings.ReplaceAll(sessionID, string(filepath.Separator), "/")

		entries, errs := loadCodexFile(path, sessionID)
		result.Entries = append(result.Entries, entries...)
		result.Errors = append(result.Errors, errs...)
		return nil
	})
	if err != nil {
		result.Errors = append(result.Errors, err)
	}

	return result
}

// getCodexPath returns the Codex home directory
func getCodexPath() (string, error) {
	if envPath := os.Getenv(codexHomeEnv); envPath != "" {
		return filepath.Abs(envPath)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(homeDir, codexDefaultDir), nil
}

// loadCodexFile loads entries from a single Codex JSONL file
func loadCodexFile(path string, sessionID string) ([]UsageEntry, []error) {
	var entries []UsageEntry
	var errors []error

	file, err := os.Open(path)
	if err != nil {
		return nil, []error{err}
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

	var currentModel string
	var previousTotals *codexRawUsage

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		var entry codexEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		switch entry.Type {
		case "turn_context":
			// Extract model from turn_context
			var ctx codexTurnContext
			if err := json.Unmarshal(entry.Payload, &ctx); err == nil && ctx.Model != "" {
				currentModel = ctx.Model
			}

		case "event_msg":
			// Check if this is a token_count event
			var payload codexTokenPayload
			if err := json.Unmarshal(entry.Payload, &payload); err != nil {
				continue
			}
			if payload.Type != "token_count" {
				continue
			}
			if entry.Timestamp == "" {
				continue
			}

			// Extract model from various locations
			model := extractCodexModel(&payload)
			if model != "" {
				currentModel = model
			}
			if currentModel == "" {
				currentModel = codexFallbackModel
			}

			// Get token delta
			var delta *codexRawUsage
			if payload.Info.LastTokenUsage != nil {
				delta = payload.Info.LastTokenUsage
			} else if payload.Info.TotalTokenUsage != nil {
				delta = subtractUsage(payload.Info.TotalTokenUsage, previousTotals)
			}

			if payload.Info.TotalTokenUsage != nil {
				previousTotals = payload.Info.TotalTokenUsage
			}

			if delta == nil {
				continue
			}

			// Skip zero deltas
			if delta.InputTokens == 0 && delta.OutputTokens == 0 && delta.CachedInputTokens == 0 {
				continue
			}

			// Parse timestamp
			ts, err := time.Parse(time.RFC3339, entry.Timestamp)
			if err != nil {
				ts, err = time.Parse("2006-01-02T15:04:05.000Z", entry.Timestamp)
				if err != nil {
					continue
				}
			}

			entries = append(entries, UsageEntry{
				Timestamp:       ts,
				SessionID:       sessionID,
				Model:           currentModel,
				InputTokens:     delta.InputTokens,
				OutputTokens:    delta.OutputTokens,
				CacheReadTokens: delta.CachedInputTokens,
				ReasoningTokens: delta.ReasoningOutputTokens,
				Provider:        ProviderCodex,
			})
		}
	}

	if err := scanner.Err(); err != nil {
		errors = append(errors, err)
	}

	return entries, errors
}

// extractCodexModel extracts the model name from various locations in the payload
func extractCodexModel(payload *codexTokenPayload) string {
	if payload.Info.Model != "" {
		return payload.Info.Model
	}
	if payload.Info.ModelName != "" {
		return payload.Info.ModelName
	}
	if payload.Info.Metadata.Model != "" {
		return payload.Info.Metadata.Model
	}
	return ""
}

// subtractUsage calculates the delta between current and previous totals
func subtractUsage(current, previous *codexRawUsage) *codexRawUsage {
	if current == nil {
		return nil
	}
	if previous == nil {
		return current
	}

	return &codexRawUsage{
		InputTokens:           max(0, current.InputTokens-previous.InputTokens),
		CachedInputTokens:     max(0, current.CachedInputTokens-previous.CachedInputTokens),
		OutputTokens:          max(0, current.OutputTokens-previous.OutputTokens),
		ReasoningOutputTokens: max(0, current.ReasoningOutputTokens-previous.ReasoningOutputTokens),
		TotalTokens:           max(0, current.TotalTokens-previous.TotalTokens),
	}
}

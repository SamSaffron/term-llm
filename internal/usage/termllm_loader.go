package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LoadTermLLMUsage loads usage data from term-llm's own log files
func LoadTermLLMUsage() LoadResult {
	var result LoadResult

	usageDir := getUsageDir()
	if _, err := os.Stat(usageDir); os.IsNotExist(err) {
		result.MissingDirectories = append(result.MissingDirectories, usageDir)
		return result
	}

	// Find all JSONL files in the usage directory
	files, err := os.ReadDir(usageDir)
	if err != nil {
		result.Errors = append(result.Errors, err)
		return result
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}
		if !strings.HasSuffix(file.Name(), ".jsonl") {
			continue
		}

		filePath := filepath.Join(usageDir, file.Name())
		entries, errs := loadTermLLMFile(filePath)
		result.Entries = append(result.Entries, entries...)
		result.Errors = append(result.Errors, errs...)
	}

	return result
}

// loadTermLLMFile loads entries from a single term-llm JSONL file
func loadTermLLMFile(path string) ([]UsageEntry, []error) {
	var entries []UsageEntry
	var errors []error

	file, err := os.Open(path)
	if err != nil {
		return nil, []error{err}
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		var entry LogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue // Skip invalid lines
		}

		// Skip entries with no usage data
		if entry.InputTokens == 0 && entry.OutputTokens == 0 {
			continue
		}

		// Use Provider as Model fallback when Model is empty
		// (the Provider field contains the LLM provider name like "Anthropic")
		model := entry.Model
		if model == "" {
			model = entry.Provider
		}

		entries = append(entries, UsageEntry{
			Timestamp:           entry.Timestamp,
			SessionID:           entry.SessionID,
			Model:               model,
			InputTokens:         entry.InputTokens,
			OutputTokens:        entry.OutputTokens,
			CacheWriteTokens:    entry.CacheWriteTokens,
			CacheReadTokens:     entry.CacheReadTokens,
			CostUSD:             entry.CostUSD,
			Provider:            ProviderTermLLM,
			TrackedExternallyBy: entry.TrackedExternallyBy,
		})
	}

	if err := scanner.Err(); err != nil {
		errors = append(errors, err)
	}

	return entries, errors
}

// LoadTermLLMUsageForDateRange loads term-llm usage data for a specific date range
// This is more efficient than loading all data and filtering, as it only reads
// files within the date range.
func LoadTermLLMUsageForDateRange(since, until time.Time) LoadResult {
	var result LoadResult

	usageDir := getUsageDir()
	if _, err := os.Stat(usageDir); os.IsNotExist(err) {
		result.MissingDirectories = append(result.MissingDirectories, usageDir)
		return result
	}

	// Generate list of dates in range
	for d := since; !d.After(until); d = d.AddDate(0, 0, 1) {
		filename := d.Format("2006-01-02") + ".jsonl"
		filePath := filepath.Join(usageDir, filename)

		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			continue // File doesn't exist for this date, skip
		}

		entries, errs := loadTermLLMFile(filePath)
		result.Entries = append(result.Entries, entries...)
		result.Errors = append(result.Errors, errs...)
	}

	return result
}

package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LogEntry represents a single usage log entry written by term-llm
type LogEntry struct {
	Timestamp           time.Time `json:"timestamp"`
	SessionID           string    `json:"session_id,omitempty"`
	Model               string    `json:"model"`
	Provider            string    `json:"provider"`
	InputTokens         int       `json:"input_tokens"`
	OutputTokens        int       `json:"output_tokens"`
	CacheWriteTokens    int       `json:"cache_write_tokens,omitempty"`
	CacheReadTokens     int       `json:"cache_read_tokens,omitempty"`
	CostUSD             float64   `json:"cost_usd,omitempty"`
	TrackedExternallyBy string    `json:"tracked_externally_by,omitempty"`
}

// Logger writes usage entries to daily JSONL files
type Logger struct {
	baseDir string
	mu      sync.Mutex
}

var (
	defaultLogger     *Logger
	defaultLoggerOnce sync.Once
)

// DefaultLogger returns the singleton logger instance
func DefaultLogger() *Logger {
	defaultLoggerOnce.Do(func() {
		defaultLogger = NewLogger()
	})
	return defaultLogger
}

// NewLogger creates a new Logger with the default XDG data directory
func NewLogger() *Logger {
	return &Logger{
		baseDir: getUsageDir(),
	}
}

// Log writes a usage entry to the appropriate daily file
func (l *Logger) Log(entry LogEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Ensure directory exists
	if err := os.MkdirAll(l.baseDir, 0755); err != nil {
		return err
	}

	// Determine filename based on date
	date := entry.Timestamp.Format("2006-01-02")
	filename := filepath.Join(l.baseDir, date+".jsonl")

	// Open file for appending
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	// Write JSON line
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	w := bufio.NewWriter(f)
	if _, err := w.Write(data); err != nil {
		return err
	}
	if _, err := w.WriteString("\n"); err != nil {
		return err
	}
	return w.Flush()
}

// getUsageDir returns the XDG data directory for term-llm usage logs
func getUsageDir() string {
	// Check XDG_DATA_HOME first
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		return filepath.Join(xdgData, "term-llm", "usage")
	}

	// Default to ~/.local/share/term-llm/usage
	homeDir, err := os.UserHomeDir()
	if err != nil {
		// Fallback to current directory if home not available
		return filepath.Join(".", ".term-llm", "usage")
	}

	return filepath.Join(homeDir, ".local", "share", "term-llm", "usage")
}

// GetTrackedExternallyBy returns the external tracking provider for a given term-llm provider name
func GetTrackedExternallyBy(providerName string) string {
	switch providerName {
	case "claude-bin":
		return ProviderClaudeCode
	case "gemini-cli":
		return ProviderGeminiCLI
	default:
		return ""
	}
}

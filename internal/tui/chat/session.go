package chat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Session represents a saved chat session
type Session struct {
	Name      string        `json:"name"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Provider  string        `json:"provider"`
	Model     string        `json:"model"`
	Messages  []ChatMessage `json:"messages"`
}

// getSessionsDir returns the XDG data directory for sessions
func getSessionsDir() (string, error) {
	// Use XDG_DATA_HOME if set, otherwise ~/.local/share
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		dataHome = filepath.Join(home, ".local", "share")
	}

	sessionsDir := filepath.Join(dataHome, "term-llm", "sessions")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create sessions directory: %w", err)
	}

	return sessionsDir, nil
}

// SaveSession saves a session to disk
func SaveSession(session *Session, filename string) error {
	dir, err := getSessionsDir()
	if err != nil {
		return err
	}

	session.UpdatedAt = time.Now()

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal session: %w", err)
	}

	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write session: %w", err)
	}

	return nil
}

// LoadSession loads a session from disk
func LoadSession(filename string) (*Session, error) {
	dir, err := getSessionsDir()
	if err != nil {
		return nil, err
	}

	path := filepath.Join(dir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No session exists
		}
		return nil, fmt.Errorf("failed to read session: %w", err)
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session: %w", err)
	}

	return &session, nil
}

// SaveCurrentSession saves the current session for auto-restore
func SaveCurrentSession(session *Session) error {
	return SaveSession(session, "current.json")
}

// LoadCurrentSession loads the current session if it exists
func LoadCurrentSession() (*Session, error) {
	return LoadSession("current.json")
}

// ListSessions returns all saved session names
func ListSessions() ([]string, error) {
	dir, err := getSessionsDir()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read sessions directory: %w", err)
	}

	var sessions []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if filepath.Ext(name) == ".json" && name != "current.json" {
			sessions = append(sessions, name[:len(name)-5]) // Remove .json extension
		}
	}

	return sessions, nil
}

// DeleteSession removes a saved session
func DeleteSession(name string) error {
	dir, err := getSessionsDir()
	if err != nil {
		return err
	}

	path := filepath.Join(dir, name+".json")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to delete session: %w", err)
	}

	return nil
}

// NewSession creates a new empty session
func NewSession(provider, model string) *Session {
	return &Session{
		Name:      "",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Provider:  provider,
		Model:     model,
		Messages:  []ChatMessage{},
	}
}

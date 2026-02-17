package credentials

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAnthropicOAuthCredentialsRoundTrip(t *testing.T) {
	// Use a temp dir for credentials
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Initially should not exist
	if AnthropicOAuthCredentialsExist() {
		t.Fatal("expected no credentials initially")
	}

	// Save credentials
	creds := &AnthropicOAuthCredentials{
		AccessToken: "sk-ant-oat01-test-token-123",
	}
	if err := SaveAnthropicOAuthCredentials(creds); err != nil {
		t.Fatalf("failed to save credentials: %v", err)
	}

	// Should now exist
	if !AnthropicOAuthCredentialsExist() {
		t.Fatal("expected credentials to exist after save")
	}

	// Load and verify
	loaded, err := GetAnthropicOAuthCredentials()
	if err != nil {
		t.Fatalf("failed to load credentials: %v", err)
	}
	if loaded.AccessToken != "sk-ant-oat01-test-token-123" {
		t.Fatalf("access_token=%q, want %q", loaded.AccessToken, "sk-ant-oat01-test-token-123")
	}

	// Verify file permissions
	credPath := filepath.Join(tmpDir, "term-llm", "anthropic_oauth.json")
	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("failed to stat credentials file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("expected 0600 permissions, got %o", info.Mode().Perm())
	}

	// Clear credentials
	if err := ClearAnthropicOAuthCredentials(); err != nil {
		t.Fatalf("failed to clear credentials: %v", err)
	}

	// Should no longer exist
	if AnthropicOAuthCredentialsExist() {
		t.Fatal("expected no credentials after clear")
	}
}

func TestAnthropicOAuthCredentialsEmptyToken(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	// Save credentials with empty token
	creds := &AnthropicOAuthCredentials{
		AccessToken: "",
	}
	if err := SaveAnthropicOAuthCredentials(creds); err != nil {
		t.Fatalf("failed to save credentials: %v", err)
	}

	// Loading should fail because token is empty
	_, err := GetAnthropicOAuthCredentials()
	if err == nil {
		t.Fatal("expected error for empty access token")
	}
}

func TestAnthropicOAuthCredentialsNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	_, err := GetAnthropicOAuthCredentials()
	if err == nil {
		t.Fatal("expected error when credentials don't exist")
	}
}

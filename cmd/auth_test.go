package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
)

func TestFormatExpiry(t *testing.T) {
	cases := []struct {
		name    string
		unix    int64
		expired bool
		want    string
	}{
		{"no expiry", 0, false, "signed in (no expiry)"},
		{"expired", time.Now().Add(-1 * time.Hour).Unix(), true, "expired (will refresh on next use)"},
		{"future", time.Date(2030, 1, 2, 15, 4, 0, 0, time.UTC).Unix(), false, "signed in; expires 2030-01-02 15:04 UTC"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatExpiry(c.unix, c.expired); got != c.want {
				t.Fatalf("formatExpiry = %q, want %q", got, c.want)
			}
		})
	}
}

func TestFormatAuthStatusLine(t *testing.T) {
	line := formatAuthStatusLine("ChatGPT (Codex)", "signed in", "account abc")
	if !strings.Contains(line, "ChatGPT (Codex)") || !strings.Contains(line, "signed in") || !strings.Contains(line, "(account abc)") {
		t.Fatalf("missing parts in %q", line)
	}
	noDetail := formatAuthStatusLine("X", "y", "")
	if strings.Contains(noDetail, "()") {
		t.Fatalf("empty detail should not render parens: %q", noDetail)
	}
}

func TestResolveAuthProviderByArg(t *testing.T) {
	p, err := resolveAuthProvider([]string{"chatgpt"}, "")
	if err != nil {
		t.Fatalf("chatgpt: %v", err)
	}
	if p.id != "chatgpt" {
		t.Fatalf("got %q, want chatgpt", p.id)
	}

	// Case-insensitive arg.
	p, err = resolveAuthProvider([]string{"COPILOT"}, "")
	if err != nil {
		t.Fatalf("COPILOT: %v", err)
	}
	if p.id != "copilot" {
		t.Fatalf("got %q, want copilot", p.id)
	}

	// Unknown.
	if _, err := resolveAuthProvider([]string{"bogus"}, ""); err == nil {
		t.Fatal("expected error for unknown provider")
	} else if !strings.Contains(err.Error(), "valid:") {
		t.Fatalf("error should list valid providers, got: %v", err)
	}
}

func TestAuthProviderCompletion(t *testing.T) {
	out, _ := authProviderCompletion(nil, nil, "")
	want := map[string]bool{"chatgpt": true, "copilot": true}
	if len(out) != len(want) {
		t.Fatalf("completion = %v", out)
	}
	for _, id := range out {
		if !want[id] {
			t.Fatalf("unexpected completion id %q", id)
		}
	}
	// Once an arg is set, no more completions.
	out2, _ := authProviderCompletion(nil, []string{"chatgpt"}, "")
	if len(out2) != 0 {
		t.Fatalf("expected no completions after arg, got %v", out2)
	}
}

func TestRunAuthLogoutNoCredentials(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cmd := authLogoutCmd
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)

	if err := runAuthLogout(cmd, []string{"chatgpt"}); err != nil {
		t.Fatalf("logout with no creds should not error: %v", err)
	}
	if !strings.Contains(stdout.String(), "No ChatGPT (Codex) credentials stored.") {
		t.Fatalf("unexpected output: %q", stdout.String())
	}
}

func TestRunAuthLogoutClearsCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	creds := &credentials.ChatGPTCredentials{
		AccessToken:  "tok",
		RefreshToken: "ref",
		ExpiresAt:    time.Now().Add(1 * time.Hour).Unix(),
		AccountID:    "acct-1",
	}
	if err := credentials.SaveChatGPTCredentials(creds); err != nil {
		t.Fatalf("save: %v", err)
	}
	credPath := filepath.Join(dir, "term-llm", "chatgpt_oauth.json")
	if _, err := os.Stat(credPath); err != nil {
		t.Fatalf("creds file should exist before logout: %v", err)
	}

	cmd := authLogoutCmd
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)

	if err := runAuthLogout(cmd, []string{"chatgpt"}); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if !strings.Contains(stdout.String(), "Cleared ChatGPT (Codex) credentials.") {
		t.Fatalf("unexpected output: %q", stdout.String())
	}
	if _, err := os.Stat(credPath); !os.IsNotExist(err) {
		t.Fatalf("creds file should be removed after logout, stat err = %v", err)
	}
}

func TestRunAuthStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	// Pre-seed ChatGPT creds with a known future expiry; leave Copilot blank.
	expires := time.Date(2030, 1, 2, 15, 4, 0, 0, time.UTC).Unix()
	if err := credentials.SaveChatGPTCredentials(&credentials.ChatGPTCredentials{
		AccessToken: "tok",
		ExpiresAt:   expires,
		AccountID:   "acct-xyz",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	cmd := authStatusCmd
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)

	if err := runAuthStatus(cmd, nil); err != nil {
		t.Fatalf("status: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		"ChatGPT (Codex)",
		"signed in; expires 2030-01-02 15:04 UTC",
		"(account acct-xyz)",
		"GitHub Copilot",
		"not signed in",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q\nfull output:\n%s", want, out)
		}
	}
}

func TestRunAuthStatusInvalidStoredFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	// Write a malformed creds file so GetChatGPTCredentials returns an error
	// even though Exists() reports true. status should surface it without
	// aborting the whole command.
	credDir := filepath.Join(dir, "term-llm")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "chatgpt_oauth.json"), []byte("not json"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cmd := authStatusCmd
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)

	if err := runAuthStatus(cmd, nil); err != nil {
		t.Fatalf("status: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "ERROR") {
		t.Fatalf("expected per-provider ERROR line for corrupt creds, got:\n%s", out)
	}
	if !strings.Contains(out, "GitHub Copilot") {
		t.Fatalf("status should still report Copilot even when ChatGPT line errors, got:\n%s", out)
	}
}

// guard against silent contract drift: each registered provider must
// expose a non-nil hook for every responsibility.
func TestAuthProvidersRegistryComplete(t *testing.T) {
	for _, p := range authProviders() {
		if p.id == "" || p.name == "" {
			t.Fatalf("provider missing id/name: %+v", p)
		}
		if p.exists == nil || p.clear == nil || p.login == nil || p.describe == nil {
			t.Fatalf("provider %q has nil hook", p.id)
		}
	}
}

// sanity check that the JSON shape on disk hasn't drifted from what we
// document in the status output; a future schema change should make this
// test fail loudly.
func TestStoredCredentialsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	orig := &credentials.ChatGPTCredentials{
		AccessToken:  "a",
		RefreshToken: "r",
		ExpiresAt:    1234567890,
		AccountID:    "acct",
	}
	if err := credentials.SaveChatGPTCredentials(orig); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "term-llm", "chatgpt_oauth.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"access_token", "refresh_token", "expires_at", "account_id"} {
		if _, ok := generic[k]; !ok {
			t.Fatalf("stored creds missing field %q", k)
		}
	}
}

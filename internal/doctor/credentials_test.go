package doctor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestCredentialsCheckMissing(t *testing.T) {
	providers := []string{"chatgpt", "grok", "copilot"}
	tests := []struct {
		name string
		cfg  *config.Config
		warn string
	}{
		{name: "unused", cfg: &config.Config{}},
		{name: "default chatgpt", cfg: &config.Config{DefaultProvider: "chatgpt"}, warn: "chatgpt"},
		{name: "default grok", cfg: &config.Config{DefaultProvider: "grok"}, warn: "grok"},
		{name: "default copilot", cfg: &config.Config{DefaultProvider: "copilot"}, warn: "copilot"},
		{name: "configured chatgpt", cfg: &config.Config{Providers: map[string]config.ProviderConfig{"chatgpt": {}}}, warn: "chatgpt"},
		{name: "configured grok", cfg: &config.Config{Providers: map[string]config.ProviderConfig{"grok": {}}}, warn: "grok"},
		{name: "configured copilot", cfg: &config.Config{Providers: map[string]config.ProviderConfig{"copilot": {}}}, warn: "copilot"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			check := CredentialsCheck{Config: tc.cfg}
			findings := check.Run(context.Background())
			if len(findings) != len(providers) {
				t.Fatalf("got %d findings, want %d: %+v", len(findings), len(providers), findings)
			}

			for _, provider := range providers {
				finding := credentialsFindingWithTitle(t, findings, provider+": not signed in")
				want := SeverityInfo
				if provider == tc.warn {
					want = SeverityWarn
				}
				if finding.Severity != want {
					t.Errorf("%s severity = %s, want %s", provider, finding.Severity, want)
				}
			}
		})
	}
}

func TestCredentialsCheckPermissionFix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file modes do not expose POSIX permission bits")
	}

	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	path := writeCredFile(t, root, "chatgpt_oauth.json", map[string]any{
		"access_token":  "access",
		"refresh_token": "refresh",
		"expires_at":    time.Now().Add(time.Hour).Unix(),
		"account_id":    "account",
	}, 0o644)

	check := CredentialsCheck{Config: &config.Config{}}
	findings := check.Run(context.Background())
	finding := credentialsFindingContaining(t, findings, "chatgpt: credential file is readable")
	if finding.Severity != SeverityWarn {
		t.Fatalf("severity = %s, want %s", finding.Severity, SeverityWarn)
	}
	if finding.Fix == nil {
		t.Fatal("permission finding has no fix")
	}
	if err := finding.Fix(context.Background()); err != nil {
		t.Fatalf("fix permissions: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credential file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("credential mode = %04o, want 0600", got)
	}
}

func TestCredentialsCheckExpiry(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		file     string
		contents map[string]any
		mode     os.FileMode
		title    string
		severity Severity
	}{
		{
			name: "expired chatgpt can refresh",
			file: "chatgpt_oauth.json",
			contents: map[string]any{
				"access_token": "access", "refresh_token": "refresh",
				"expires_at": now.Add(-time.Hour).Unix(), "account_id": "account",
			},
			mode: 0o600, title: "chatgpt: access token expired", severity: SeverityInfo,
		},
		{
			name: "expired copilot cannot refresh",
			file: "copilot_oauth.json",
			contents: map[string]any{
				"access_token": "access", "expires_at": now.Add(-time.Hour).Unix(),
			},
			mode: 0o600, title: "copilot: access token expired and cannot refresh", severity: SeverityWarn,
		},
		{
			name: "unexpired chatgpt",
			file: "chatgpt_oauth.json",
			contents: map[string]any{
				"access_token": "access", "refresh_token": "refresh",
				"expires_at": now.Add(time.Hour).Unix(), "account_id": "account",
			},
			mode: 0o600, title: "chatgpt: signed in", severity: SeverityOK,
		},
		{
			name: "unexpired grok",
			file: "grok_oauth.json",
			contents: map[string]any{
				"access_token": "access", "refresh_token": "refresh",
				"expires_at": now.Add(time.Hour).Unix(), "account_id": "account",
			},
			mode: 0o600, title: "grok: signed in", severity: SeverityOK,
		},
		{
			name: "unexpired copilot",
			file: "copilot_oauth.json",
			contents: map[string]any{
				"access_token": "access", "expires_at": now.Add(time.Hour).Unix(),
			},
			mode: 0o600, title: "copilot: signed in", severity: SeverityOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", root)
			writeCredFile(t, root, tc.file, tc.contents, tc.mode)

			check := CredentialsCheck{Config: &config.Config{}}
			finding := credentialsFindingWithTitle(t, check.Run(context.Background()), tc.title)
			if finding.Severity != tc.severity {
				t.Fatalf("severity = %s, want %s", finding.Severity, tc.severity)
			}
		})
	}
}

func writeCredFile(t *testing.T, root, name string, contents any, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(root, "term-llm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create credential directory: %v", err)
	}
	data, err := json.Marshal(contents)
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write credential file: %v", err)
	}
	// A restrictive umask may alter creation permissions, so set the test mode explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod credential file: %v", err)
	}
	return path
}

func credentialsFindingWithTitle(t *testing.T, findings []Finding, title string) Finding {
	t.Helper()
	for _, finding := range findings {
		if finding.Title == title {
			return finding
		}
	}
	t.Fatalf("no finding with title %q in %+v", title, findings)
	return Finding{}
}

func credentialsFindingContaining(t *testing.T, findings []Finding, text string) Finding {
	t.Helper()
	for _, finding := range findings {
		if strings.Contains(finding.Title, text) {
			return finding
		}
	}
	t.Fatalf("no finding containing %q in %+v", text, findings)
	return Finding{}
}

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/spf13/cobra"
)

func TestCursorAgentAccessTokenPrefersAgentAndFallsBackToCursorAgent(t *testing.T) {
	originalLookPath, originalCommand := cursorUsageLookPath, cursorUsageCommand
	originalStoredToken := cursorUsageStoredToken
	defer func() {
		cursorUsageLookPath = originalLookPath
		cursorUsageCommand = originalCommand
		cursorUsageStoredToken = originalStoredToken
	}()

	var lookedUp []string
	cursorUsageLookPath = func(name string) (string, error) {
		lookedUp = append(lookedUp, name)
		if name == "cursor-agent" {
			return "/bin/cursor-agent", nil
		}
		return "", errors.New("not found")
	}
	cursorUsageCommand = func(_ context.Context, binary string) ([]byte, error) {
		if binary != "/bin/cursor-agent" {
			t.Fatalf("binary = %q", binary)
		}
		return []byte(`{"authenticated":true,"auth":{"accessToken":"secret-token"}}`), nil
	}

	token, err := cursorAgentAccessToken(context.Background())
	if err != nil {
		t.Fatalf("cursorAgentAccessToken: %v", err)
	}
	if token != "secret-token" {
		t.Fatalf("token = %q", token)
	}
	if strings.Join(lookedUp, ",") != "agent,cursor-agent" {
		t.Fatalf("lookups = %v", lookedUp)
	}
}

func TestCursorAgentAccessTokenUsesCredentialStoreForRedactedStatus(t *testing.T) {
	originalLookPath, originalCommand := cursorUsageLookPath, cursorUsageCommand
	originalStoredToken := cursorUsageStoredToken
	defer func() {
		cursorUsageLookPath = originalLookPath
		cursorUsageCommand = originalCommand
		cursorUsageStoredToken = originalStoredToken
	}()
	cursorUsageLookPath = func(name string) (string, error) {
		if name == "agent" {
			return "/bin/agent", nil
		}
		return "", errors.New("not found")
	}
	cursorUsageCommand = func(context.Context, string) ([]byte, error) {
		return []byte(`{"status":"authenticated","isAuthenticated":true,"hasAccessToken":true,"hasRefreshToken":true}`), nil
	}
	cursorUsageStoredToken = func() (string, error) { return "stored-token", nil }

	token, err := cursorAgentAccessToken(context.Background())
	if err != nil {
		t.Fatalf("cursorAgentAccessToken: %v", err)
	}
	if token != "stored-token" {
		t.Fatalf("token = %q", token)
	}
}

func TestCursorAgentAccessTokenFromFile(t *testing.T) {
	path := t.TempDir() + "/auth.json"
	if err := os.WriteFile(path, []byte(`{"accessToken":"file-token","refreshToken":"not-read"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := cursorAgentAccessTokenFromFiles([]string{t.TempDir() + "/missing.json", path})
	if err != nil {
		t.Fatalf("cursorAgentAccessTokenFromFiles: %v", err)
	}
	if token != "file-token" {
		t.Fatalf("token = %q", token)
	}
}

func TestCursorAgentAccessTokenErrorsDoNotExposeStatus(t *testing.T) {
	originalLookPath, originalCommand := cursorUsageLookPath, cursorUsageCommand
	originalStoredToken := cursorUsageStoredToken
	defer func() {
		cursorUsageLookPath = originalLookPath
		cursorUsageCommand = originalCommand
		cursorUsageStoredToken = originalStoredToken
	}()
	cursorUsageLookPath = func(string) (string, error) { return "/bin/agent", nil }
	cursorUsageCommand = func(context.Context, string) ([]byte, error) {
		return []byte(`{"auth":{"accessToken":"should-not-leak"}}`), errors.New("status failed")
	}
	cursorUsageStoredToken = func() (string, error) { return "", nil }

	_, err := cursorAgentAccessToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "agent login") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "should-not-leak") {
		t.Fatalf("error leaked status output: %v", err)
	}
}

func TestFetchCursorBinUsageAndWrite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Connect-Protocol-Version"); got != "1" {
			t.Errorf("connect protocol = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"billingCycleStart":"2026-08-01T00:00:00Z",
			"billingCycleEnd":"2026-09-01T00:00:00Z",
			"planUsage":{"used":"125","limit":500,"remaining":375,"totalPercentUsed":"25"},
			"spendLimitUsage":{"used":10,"limit":100,"remaining":90,"totalPercentUsed":10}
		}`))
	}))
	defer server.Close()

	originalEndpoint, originalClient := cursorUsageEndpoint, cursorUsageClient
	defer func() {
		cursorUsageEndpoint = originalEndpoint
		cursorUsageClient = originalClient
	}()
	cursorUsageEndpoint = server.URL
	cursorUsageClient = server.Client()

	raw, report, err := fetchCursorBinUsage(context.Background(), "secret-token")
	if err != nil {
		t.Fatalf("fetchCursorBinUsage: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatal("raw response is not JSON")
	}

	var out bytes.Buffer
	now := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	if err := writeCursorBinUsageWithOptions(&out, report, now, llm.ProviderUsageFormatOptions{ASCII: true}); err != nil {
		t.Fatalf("writeCursorBinUsageWithOptions: %v", err)
	}
	for _, want := range []string{
		"Cursor", "Plan usage", "25% used", "125 of 500 used · 375 remaining",
		"Spend limit", "10% used", "10 of 100 used · 90 remaining", "Resets in 15d",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestFetchCursorBinUsageErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "authentication", status: http.StatusUnauthorized, body: `{}`, want: "agent login"},
		{name: "server", status: http.StatusBadGateway, body: `{}`, want: "502 Bad Gateway"},
		{name: "malformed JSON", status: http.StatusOK, body: `{`, want: "decode Cursor usage API response"},
		{name: "missing plan", status: http.StatusOK, body: `{}`, want: "did not contain plan usage"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			originalEndpoint, originalClient := cursorUsageEndpoint, cursorUsageClient
			defer func() {
				cursorUsageEndpoint = originalEndpoint
				cursorUsageClient = originalClient
			}()
			cursorUsageEndpoint = server.URL
			cursorUsageClient = server.Client()

			_, _, err := fetchCursorBinUsage(context.Background(), "secret-token")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRunCursorBinUsageJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"billingCycleEnd":"2026-09-01T00:00:00Z","planUsage":{"totalPercentUsed":12.5}}`))
	}))
	defer server.Close()

	originalLookPath, originalCommand := cursorUsageLookPath, cursorUsageCommand
	originalEndpoint, originalClient := cursorUsageEndpoint, cursorUsageClient
	defer func() {
		cursorUsageLookPath = originalLookPath
		cursorUsageCommand = originalCommand
		cursorUsageEndpoint = originalEndpoint
		cursorUsageClient = originalClient
	}()
	cursorUsageLookPath = func(string) (string, error) { return "/bin/agent", nil }
	cursorUsageCommand = func(context.Context, string) ([]byte, error) {
		return []byte(`{"auth":{"accessToken":"secret-token"}}`), nil
	}
	cursorUsageEndpoint = server.URL
	cursorUsageClient = server.Client()

	var out bytes.Buffer
	if err := runCursorBinUsage(context.Background(), &out, true); err != nil {
		t.Fatalf("runCursorBinUsage: %v", err)
	}
	if !strings.Contains(out.String(), `"totalPercentUsed": 12.5`) {
		t.Fatalf("unexpected JSON output:\n%s", out.String())
	}
}

func TestValidateCursorBinUsageFlags(t *testing.T) {
	command := &cobra.Command{}
	command.Flags().String("provider", "", "")
	command.Flags().Bool("json", false, "")
	command.Flags().String("since", "", "")
	if err := command.Flags().Set("provider", "cursor-bin"); err != nil {
		t.Fatal(err)
	}
	if err := command.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	if err := validateCursorBinUsageFlags(command); err != nil {
		t.Fatalf("provider and JSON flags should be valid: %v", err)
	}
	if err := command.Flags().Set("since", "20260801"); err != nil {
		t.Fatal(err)
	}
	if err := validateCursorBinUsageFlags(command); err == nil || !strings.Contains(err.Error(), "--since") {
		t.Fatalf("error = %v", err)
	}
}

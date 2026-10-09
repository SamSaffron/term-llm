package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/mcp"
	mcpoauth "github.com/samsaffron/term-llm/internal/mcp/oauth"
	"golang.org/x/oauth2"
)

func TestMCPPickerForcesScopeStepUp(t *testing.T) {
	// The picker uses the global coordinator; isolate its store from test order.
	const childEnv = "TERM_LLM_TEST_TUI_MCP_STEP_UP"
	if os.Getenv(childEnv) == "" {
		home := t.TempDir()
		cmd := exec.Command(os.Args[0], "-test.run=^TestMCPPickerForcesScopeStepUp$", "-test.v")
		cmd.Env = append(os.Environ(), childEnv+"=1", "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "config"))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated MCP picker test: %v\n%s", err, output)
		}
		return
	}
	var server *httptest.Server
	var registrations atomic.Int32
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer error="insufficient_scope", resource_metadata=%q`, server.URL+"/.well-known/oauth-protected-resource/mcp"))
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"resource": server.URL + "/mcp", "authorization_servers": []string{server.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token", "registration_endpoint": server.URL + "/register", "response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"}})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		// Deliberately stop before the browser: reaching DCR proves Start
		// bypassed the already-signed-in check without opening a real browser.
		registrations.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})
	server = httptest.NewServer(mux)
	defer server.Close()
	endpoint := server.URL + "/mcp"
	path, err := mcpoauth.DefaultStorePath()
	if err != nil {
		t.Fatal(err)
	}
	_, err = mcpoauth.NewFileStore(path).Update(endpoint, func(*mcpoauth.Session) (*mcpoauth.Session, error) {
		return &mcpoauth.Session{Endpoint: endpoint, Config: mcpoauth.OAuth2Config{ClientID: "client", RedirectURL: "https://old.example/callback"}, Token: &oauth2.Token{AccessToken: "valid", Expiry: time.Now().Add(time.Hour)}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m := newTestChatModel(false)
	m.mcpManager = mcp.NewManagerWithConfig(&mcp.Config{Servers: map[string]mcp.ServerConfig{"protected": {URL: endpoint}}})
	defer m.mcpManager.StopAll()
	updates := make(chan mcp.StatusUpdate, 10)
	m.mcpManager.SetStatusChannel(updates)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.mcpManager.Enable(ctx, "protected"); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case update := <-updates:
			if update.Status == mcp.StatusStarting {
				continue
			}
			if update.Status != mcp.StatusAuthRequired {
				t.Fatalf("startup status = %s: %v", update.Status, update.Error)
			}
			goto ready
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
ready:
	m.showMCPPicker()
	_, cmd := m.handleMCPPickerDialogKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("picker did not start sign-in")
	}
	result, ok := cmd().(mcpOAuthResultMsg)
	if !ok || result.err == nil || strings.Contains(result.err.Error(), "already signed in") || registrations.Load() == 0 {
		t.Fatalf("picker did not force step-up: %v", result.err)
	}
}

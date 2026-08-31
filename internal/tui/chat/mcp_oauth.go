package chat

import (
	"context"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/mcp"
	mcpoauth "github.com/samsaffron/term-llm/internal/mcp/oauth"
	internalauth "github.com/samsaffron/term-llm/internal/oauth"
	"github.com/samsaffron/term-llm/internal/terminaltext"
)

type mcpOAuthResultMsg struct {
	name   string
	logout bool
	err    error
}

func (m *Model) startMCPOAuthCmd(name string, force bool) tea.Cmd {
	return func() tea.Msg {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return mcpOAuthResultMsg{name: name, err: fmt.Errorf("start callback listener: %w", err)}
		}
		defer listener.Close()
		server := &http.Server{ReadHeaderTimeout: 5 * time.Second}
		server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			_, ok := mcpoauth.DefaultCoordinator().CompleteCallback(
				r.URL.Query().Get("state"), r.URL.Query().Get("code"),
				r.URL.Query().Get("iss"), r.URL.Query().Get("error"),
			)
			if !ok {
				http.Error(w, "This authorization callback is invalid, expired, or was already used.", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_ = template.Must(template.New("done").Parse(`<!doctype html><meta charset="utf-8"><title>Connected</title><h1>Connected</h1><p>You can close this window.</p>`)).Execute(w, nil)
		})
		go func() { _ = server.Serve(listener) }()
		defer server.Shutdown(context.Background())

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		flow, err := m.mcpManager.StartOAuth(ctx, name, mcp.OAuthStartOptions{
			RedirectURL: "http://" + listener.Addr().String() + "/callback",
			Force:       force,
		})
		if err != nil {
			return mcpOAuthResultMsg{name: name, err: err}
		}
		if err := internalauth.OpenBrowser(flow.AuthorizationURL); err != nil {
			_ = m.mcpManager.CancelOAuth(name, flow.ID)
			return mcpOAuthResultMsg{name: name, err: fmt.Errorf("open browser: %w; use `term-llm mcp login %s --no-browser`", err, name)}
		}
		completed, err := mcpoauth.DefaultCoordinator().Wait(ctx, flow.ID)
		if err != nil {
			return mcpOAuthResultMsg{name: name, err: err}
		}
		if completed.State != mcpoauth.FlowSucceeded {
			if completed.Error == "" {
				completed.Error = "authorization did not complete"
			}
			return mcpOAuthResultMsg{name: name, err: fmt.Errorf("%s", completed.Error)}
		}
		return mcpOAuthResultMsg{name: name}
	}
}

func (m *Model) logoutMCPOAuthCmd(name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := m.mcpManager.LogoutOAuth(ctx, name, false)
		return mcpOAuthResultMsg{name: name, logout: true, err: err}
	}
}

func safeMCPOAuthMessage(err error) string {
	if err == nil {
		return ""
	}
	return terminaltext.SanitizeSingleLine(err.Error())
}

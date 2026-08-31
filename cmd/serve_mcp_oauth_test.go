package cmd

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServeMCPOAuthRoutesProtectFlowButNotStateGatedCallback(t *testing.T) {
	s := &serveServer{cfg: serveServerConfig{basePath: "/ui", requireAuth: true, token: "serve-token"}}
	handler := s.httpHandler()

	flowReq := httptest.NewRequest("GET", "http://example.test/ui/v1/mcp/oauth/flows/unknown", nil)
	flowRec := httptest.NewRecorder()
	handler.ServeHTTP(flowRec, flowReq)
	if flowRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated flow status = %d, want 401", flowRec.Code)
	}

	callbackReq := httptest.NewRequest("GET", "http://hostile.test/ui/v1/mcp/oauth/callback?state=invalid&code=secret-code&redirect_uri=https://evil.example", nil)
	callbackRec := httptest.NewRecorder()
	handler.ServeHTTP(callbackRec, callbackReq)
	if callbackRec.Code != http.StatusBadRequest {
		t.Fatalf("state-gated callback status = %d, want 400 (not auth rejection)", callbackRec.Code)
	}
	body := callbackRec.Body.String()
	if strings.Contains(body, "secret-code") || strings.Contains(body, "evil.example") {
		t.Fatalf("callback reflected request secrets: %q", body)
	}
}

func TestServeMCPOAuthCallbackURL(t *testing.T) {
	tests := []struct {
		name      string
		publicURL string
		basePath  string
		host      string
		tls       bool
		want      string
	}{
		{name: "derived http", basePath: "/ui", host: "127.0.0.1:8080", want: "http://127.0.0.1:8080/ui/v1/mcp/oauth/callback"},
		{name: "derived https", basePath: "/chat", host: "chat.example", tls: true, want: "https://chat.example/chat/v1/mcp/oauth/callback"},
		{name: "explicit hub mount", publicURL: "https://hub.example/node/demo", basePath: "/ui", host: "internal:8080", want: "https://hub.example/node/demo/v1/mcp/oauth/callback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &serveServer{cfg: serveServerConfig{basePath: tt.basePath, publicURL: tt.publicURL}}
			r := httptest.NewRequest("POST", "http://"+tt.host+"/", nil)
			r.Host = tt.host
			if tt.tls {
				r.TLS = &tls.ConnectionState{}
			}
			got, err := s.mcpOAuthCallbackURL(r)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("callback URL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeServePublicURL(t *testing.T) {
	if got, err := normalizeServePublicURL(" https://example.com/node/demo/ "); err != nil || got != "https://example.com/node/demo" {
		t.Fatalf("normalize = %q, %v", got, err)
	}
	for _, raw := range []string{"javascript:alert(1)", "https://example.com/path?redirect=evil", "https://user@example.com"} {
		if _, err := normalizeServePublicURL(raw); err == nil {
			t.Errorf("normalizeServePublicURL(%q) succeeded", raw)
		}
	}
}

func TestParseSessionMCPOAuthSuffix(t *testing.T) {
	server, action, ok := parseSessionMCPOAuthSuffix("mcp/github/oauth/start")
	if !ok || server != "github" || action != "start" {
		t.Fatalf("parse = %q, %q, %v", server, action, ok)
	}
	if _, _, ok := parseSessionMCPOAuthSuffix("mcp/github/oauth/start/extra"); ok {
		t.Fatal("accepted extra path segment")
	}
}

package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/llm"
	mcpoauth "github.com/samsaffron/term-llm/internal/mcp/oauth"
	"golang.org/x/oauth2"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCreateStdioTransport_InheritsEnv(t *testing.T) {
	// Server with custom env should inherit parent PATH
	client := &Client{
		name: "test",
		config: ServerConfig{
			Command: "echo",
			Args:    []string{"hello"},
			Env: map[string]string{
				"CUSTOM_VAR": "custom_value",
			},
		},
	}

	transport, stdioErr := client.createStdioTransport(context.Background())
	if stdioErr != nil {
		t.Fatalf("createStdioTransport: %v", stdioErr)
	}
	ct, ok := transport.(*sdkmcp.CommandTransport)
	if !ok {
		t.Fatal("expected sdkmcp.CommandTransport")
	}

	env := ct.Command.Env
	if env == nil {
		t.Fatal("expected non-nil env when config has env vars")
	}

	// Check that parent PATH is inherited
	hasPath := false
	hasCustom := false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			hasPath = true
		}
		if e == "CUSTOM_VAR=custom_value" {
			hasCustom = true
		}
	}

	if !hasPath {
		t.Error("parent PATH not inherited in subprocess env")
	}
	if !hasCustom {
		t.Error("custom env var not set")
	}
}

func TestCreateStdioTransport_NoEnvNil(t *testing.T) {
	// Server with no custom env should leave cmd.Env nil (inherit all)
	client := &Client{
		name: "test",
		config: ServerConfig{
			Command: "echo",
			Args:    []string{"hello"},
		},
	}

	transport, stdioErr := client.createStdioTransport(context.Background())
	if stdioErr != nil {
		t.Fatalf("createStdioTransport: %v", stdioErr)
	}
	ct, ok := transport.(*sdkmcp.CommandTransport)
	if !ok {
		t.Fatal("expected sdkmcp.CommandTransport")
	}

	if ct.Command.Env != nil {
		t.Error("expected nil env when no config env vars (inherits parent automatically)")
	}
}

func TestCreateStdioTransport_EmptyEnvNil(t *testing.T) {
	// Server with empty env map should also leave cmd.Env nil
	client := &Client{
		name: "test",
		config: ServerConfig{
			Command: "echo",
			Args:    []string{"hello"},
			Env:     map[string]string{},
		},
	}

	transport, stdioErr := client.createStdioTransport(context.Background())
	if stdioErr != nil {
		t.Fatalf("createStdioTransport: %v", stdioErr)
	}
	ct, ok := transport.(*sdkmcp.CommandTransport)
	if !ok {
		t.Fatal("expected sdkmcp.CommandTransport")
	}

	if ct.Command.Env != nil {
		t.Error("expected nil env when env map is empty")
	}
}

func TestClientStartIncludesServerStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires sh")
	}
	client := NewClient("broken", ServerConfig{
		Command: "sh",
		Args:    []string{"-c", "echo 'missing DISCOURSE_API_KEY' >&2; exit 23"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Start(ctx)
	if err == nil {
		t.Fatal("Start unexpectedly succeeded")
	}
	for _, want := range []string{"connect to MCP server broken", "MCP server stderr", "missing DISCOURSE_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Start error %q does not contain %q", err, want)
		}
	}
}

func TestCreateStdioTransport_EnvOverridesParent(t *testing.T) {
	// Set a known env var, then override it
	os.Setenv("TEST_MCP_VAR", "original")
	defer os.Unsetenv("TEST_MCP_VAR")

	client := &Client{
		name: "test",
		config: ServerConfig{
			Command: "echo",
			Env: map[string]string{
				"TEST_MCP_VAR": "overridden",
			},
		},
	}

	transport, stdioErr := client.createStdioTransport(context.Background())
	if stdioErr != nil {
		t.Fatalf("createStdioTransport: %v", stdioErr)
	}
	ct := transport.(*sdkmcp.CommandTransport)

	// The overridden value should appear (last wins in exec.Cmd)
	found := false
	for _, e := range ct.Command.Env {
		if e == "TEST_MCP_VAR=overridden" {
			found = true
		}
	}
	if !found {
		t.Error("expected overridden env var in subprocess env")
	}
}

// TestClientStart_StatelessServerSubscriptionsListen covers both halves of the
// SEP-2575 listen handshake against a stateless server.
//
// The SDK only issues subscriptions/listen when the server advertises the
// matching list-changed capability, so the advertised flag decides whether the
// call is made at all. Either way Start must succeed: when the capability is
// absent the call is skipped, and when it is present but the server rejects the
// call the connection must survive it (the SDK wraps a discover/listen failure
// with jsonrpc2.ErrRejected precisely so the rejection does not break the
// connection for subsequent RPCs).
func TestClientStart_StatelessServerSubscriptionsListen(t *testing.T) {
	tests := []struct {
		name        string
		listChanged bool
		wantListen  bool
	}{
		{
			name:        "capability absent: listen is skipped",
			listChanged: false,
			wantListen:  false,
		},
		{
			name:        "capability advertised but rejected: Start survives",
			listChanged: true,
			wantListen:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var listenSeen atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					ID     any    `json:"id"`
					Method string `json:"method"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch request.Method {
				case "server/discover":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"jsonrpc": "2.0", "id": request.ID,
						"result": map[string]any{
							"resultType":        "complete",
							"supportedVersions": []string{"2026-07-28"},
							"capabilities": map[string]any{
								"tools": map[string]any{"listChanged": tt.listChanged},
							},
							"_meta": map[string]any{
								"io.modelcontextprotocol/serverInfo": map[string]any{"name": "stateless-test", "version": "1"},
							},
						},
					})
				case "subscriptions/listen":
					listenSeen.Store(true)
					w.WriteHeader(http.StatusNotFound)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"jsonrpc": "2.0", "id": request.ID,
						"error": map[string]any{"code": -32601, "message": "Method not found"},
					})
				case "tools/list":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"jsonrpc": "2.0", "id": request.ID,
						"result": map[string]any{"tools": []any{}, "resultType": "complete"},
					})
				default:
					w.WriteHeader(http.StatusNotFound)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"jsonrpc": "2.0", "id": request.ID,
						"error": map[string]any{"code": -32601, "message": "Method not found"},
					})
				}
			}))
			defer server.Close()

			client := NewClient("stateless-test", ServerConfig{
				URL:   server.URL,
				OAuth: &OAuthConfig{Disabled: true},
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := client.Start(ctx); err != nil {
				t.Fatalf("Start with listChanged=%v: %v", tt.listChanged, err)
			}
			defer client.Stop()
			if got := listenSeen.Load(); got != tt.wantListen {
				t.Fatalf("subscriptions/listen attempted = %v, want %v", got, tt.wantListen)
			}
			if snapshot := client.ToolSnapshot(); snapshot == nil || len(snapshot.Tools) != 0 {
				t.Fatalf("tool snapshot = %+v, want an empty published snapshot", snapshot)
			}
		})
	}
}

func TestCreateHTTPTransport_UsesTransportLevelTimeouts(t *testing.T) {
	client := &Client{
		name: "test",
		config: ServerConfig{
			URL:   "https://example.com/mcp",
			OAuth: &OAuthConfig{Disabled: true},
		},
	}

	transport, err := client.createHTTPTransport()
	if err != nil {
		t.Fatalf("createHTTPTransport: %v", err)
	}
	st, ok := transport.(*sdkmcp.StreamableClientTransport)
	if !ok {
		t.Fatal("expected sdkmcp.StreamableClientTransport")
	}
	if st.HTTPClient == nil {
		t.Fatal("expected HTTP client")
	}
	if st.HTTPClient.Timeout != 0 {
		t.Fatalf("HTTPClient.Timeout = %v, want 0 so context controls long-running calls", st.HTTPClient.Timeout)
	}

	ht, ok := st.HTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("HTTPClient.Transport = %T, want *http.Transport", st.HTTPClient.Transport)
	}
	if ht.DialContext == nil {
		t.Fatal("expected DialContext timeout transport")
	}
	if ht.TLSHandshakeTimeout == 0 {
		t.Fatal("expected TLS handshake timeout")
	}
	if ht.ResponseHeaderTimeout == 0 {
		t.Fatal("expected response header timeout")
	}
	if ht.IdleConnTimeout == 0 {
		t.Fatal("expected idle connection timeout")
	}
	if ht.Proxy == nil {
		t.Fatal("expected cloned default transport to preserve proxy configuration")
	}
	if !ht.ForceAttemptHTTP2 {
		t.Fatal("expected cloned default transport to preserve HTTP/2 support")
	}
}

func TestCreateHTTPTransport_HeadersWrapTimeoutTransport(t *testing.T) {
	client := &Client{
		name: "test",
		config: ServerConfig{
			URL:     "https://example.com/mcp",
			Headers: map[string]string{"Authorization": "Bearer token"},
		},
	}

	transport, err := client.createHTTPTransport()
	if err != nil {
		t.Fatalf("createHTTPTransport: %v", err)
	}
	st := transport.(*sdkmcp.StreamableClientTransport)
	if st.HTTPClient.Timeout != 0 {
		t.Fatalf("HTTPClient.Timeout = %v, want 0 so context controls long-running calls", st.HTTPClient.Timeout)
	}

	ht, ok := st.HTTPClient.Transport.(*headerTransport)
	if !ok {
		t.Fatalf("HTTPClient.Transport = %T, want *headerTransport", st.HTTPClient.Transport)
	}
	if got := ht.headers["Authorization"]; got != "Bearer token" {
		t.Fatalf("Authorization header = %q, want %q", got, "Bearer token")
	}

	base, ok := ht.base.(*http.Transport)
	if !ok {
		t.Fatalf("headerTransport.base = %T, want *http.Transport", ht.base)
	}
	if base.ResponseHeaderTimeout == 0 {
		t.Fatal("expected wrapped transport to keep response header timeout")
	}
}

func TestCreateHTTPTransport_OAuthWiringAndAuthorizationHeaderPrecedence(t *testing.T) {
	tests := []struct {
		name        string
		config      ServerConfig
		wantHandler bool
	}{
		{name: "automatic OAuth", config: ServerConfig{URL: "https://example.com/mcp"}, wantHandler: true},
		{name: "explicit Authorization header", config: ServerConfig{URL: "https://example.com/mcp", Headers: map[string]string{"authorization": "Bearer static"}}},
		{name: "OAuth disabled", config: ServerConfig{URL: "https://example.com/mcp", OAuth: &OAuthConfig{Disabled: true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{
				name: "test", config: tt.config,
				oauthCoordinator: mcpoauth.NewCoordinator(mcpoauth.NewFileStore(filepath.Join(t.TempDir(), "oauth.json"))),
			}
			transport, err := client.createHTTPTransport()
			if err != nil {
				t.Fatal(err)
			}
			streamable := transport.(*sdkmcp.StreamableClientTransport)
			if got := streamable.OAuthHandler != nil; got != tt.wantHandler {
				t.Fatalf("OAuthHandler attached = %v, want %v", got, tt.wantHandler)
			}
		})
	}
}

func TestCreateHTTPTransport_BackgroundOAuthDoesNotDiscover(t *testing.T) {
	// A background challenge must not cause discovery or registration, even
	// when the MCP endpoint is configured with custom headers such as API keys.
	var server *httptest.Server
	var leakedHeader atomic.Bool
	var oauthRequests atomic.Int32
	record := func(r *http.Request) {
		oauthRequests.Add(1)
		if r.Header.Get("X-Api-Key") != "" {
			leakedHeader.Store(true)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"resource":%q,"authorization_servers":[%q]}`, server.URL+"/mcp", server.URL)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"registration_endpoint":%q,"response_types_supported":["code"],"code_challenge_methods_supported":["S256"]}`,
			server.URL, server.URL+"/authorize", server.URL+"/token", server.URL+"/register")
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"client_id":"dynamic-client","redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`)
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	client := &Client{
		name: "test",
		config: ServerConfig{
			URL:     server.URL + "/mcp",
			Headers: map[string]string{"X-Api-Key": "super-secret"},
		},
		oauthCoordinator: mcpoauth.NewCoordinator(mcpoauth.NewFileStore(filepath.Join(t.TempDir(), "oauth.json"))),
	}
	transport, err := client.createHTTPTransport()
	if err != nil {
		t.Fatal(err)
	}
	streamable := transport.(*sdkmcp.StreamableClientTransport)
	if streamable.OAuthHandler == nil {
		t.Fatal("expected OAuth handler")
	}

	req, err := http.NewRequest(http.MethodPost, server.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	challenge := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header: http.Header{"Www-Authenticate": []string{
			fmt.Sprintf(`Bearer resource_metadata=%q`, server.URL+"/.well-known/oauth-protected-resource/mcp"),
		}},
		Body: io.NopCloser(strings.NewReader("")),
	}
	err = streamable.OAuthHandler.Authorize(context.Background(), req, challenge)
	if !errors.Is(err, mcpoauth.ErrAuthenticationRequired) {
		t.Fatalf("background Authorize error = %v, want ErrAuthenticationRequired", err)
	}
	if oauthRequests.Load() != 0 {
		t.Fatal("background OAuth must not contact authorization-server endpoints")
	}
	if leakedHeader.Load() {
		t.Fatal("custom MCP header leaked to authorization-server endpoints")
	}
}

func TestHeaderTransportAddsHeadersWithoutMutatingRequest(t *testing.T) {
	req, err := http.NewRequest("GET", "https://example.com/mcp", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "caller")

	var observedAuth string
	transport := &headerTransport{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			observedAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       http.NoBody,
				Request:    req,
			}, nil
		}),
		headers: map[string]string{"Authorization": "Bearer token"},
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()

	if observedAuth != "Bearer token" {
		t.Fatalf("observed Authorization = %q, want %q", observedAuth, "Bearer token")
	}
	if got := req.Header.Get("Authorization"); got != "caller" {
		t.Fatalf("original request Authorization mutated to %q", got)
	}
}

func TestCreateStdioTransport_ConfiguresDetachedProcessGroupCancellation(t *testing.T) {
	client := &Client{
		name: "test",
		config: ServerConfig{
			Command: "echo",
			Args:    []string{"hello"},
		},
	}

	transport, stdioErr := client.createStdioTransport(context.Background())
	if stdioErr != nil {
		t.Fatalf("createStdioTransport: %v", stdioErr)
	}
	ct := transport.(*sdkmcp.CommandTransport)

	if ct.Command.Cancel == nil {
		t.Fatalf("expected subprocess cancel hook to be configured")
	}
	if ct.Command.SysProcAttr == nil || !ct.Command.SysProcAttr.Setsid {
		t.Fatalf("expected subprocess to run in a detached session")
	}
	if ct.Command.WaitDelay != time.Second {
		t.Fatalf("WaitDelay = %v, want %v", ct.Command.WaitDelay, time.Second)
	}
	if client.processCancel == nil {
		t.Fatalf("expected client to retain a stdio process cancel func")
	}
}

func TestClientStop_CancelsStdioProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires sh")
	}

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	client := NewClient("greeter", ServerConfig{
		Command: "sh",
		Args: []string{
			"-c",
			"sleep 30 >/dev/null 2>&1 & echo $! > \"$1\"; exec \"$2\"",
			"sh",
			pidFile,
			os.Args[0],
		},
		Env: map[string]string{
			runMCPManagerTestServerEnv: "1",
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	pid := waitForRecordedPID(t, pidFile)
	defer killProcessIfRunning(pid)

	if err := client.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	waitForMCPProcessExit(t, pid)
}

func waitForRecordedPID(t *testing.T, path string) int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pidText := strings.TrimSpace(string(data))
			if pidText != "" {
				pid, convErr := strconv.Atoi(pidText)
				if convErr != nil {
					t.Fatalf("parse pid %q: %v", pidText, convErr)
				}
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for pid file %s", path)
	return 0
}

func waitForMCPProcessExit(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mcpProcessHasExited(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for process %d to exit", pid)
}

func killProcessIfRunning(pid int) {
	if pid <= 0 || mcpProcessHasExited(pid) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func mcpProcessHasExited(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err != nil {
		return errors.Is(err, syscall.ESRCH)
	}
	if runtime.GOOS == "linux" {
		state, ok := linuxMCPProcState(pid)
		return ok && state == 'Z'
	}
	return false
}

func linuxMCPProcState(pid int) (byte, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	return mcpProcStatState(data)
}

func mcpProcStatState(data []byte) (byte, bool) {
	stat := string(data)
	end := strings.LastIndex(stat, ")")
	if end == -1 || end+2 >= len(stat) {
		return 0, false
	}
	return stat[end+2], true
}

func TestFormatContent(t *testing.T) {
	imagePart := func(mimeType string, data []byte) llm.ToolContentPart {
		return llm.ToolContentPart{
			Type: llm.ToolContentPartImageData,
			ImageData: &llm.ToolImageData{
				MediaType: mimeType,
				Base64:    base64.StdEncoding.EncodeToString(data),
			},
		}
	}
	textPart := func(text string) llm.ToolContentPart {
		return llm.ToolContentPart{Type: llm.ToolContentPartText, Text: text}
	}

	tests := []struct {
		name    string
		content []sdkmcp.Content
		want    llm.ToolOutput
	}{
		{
			name:    "text only compatibility",
			content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "hello"}, &sdkmcp.TextContent{Text: " world"}},
			want: llm.ToolOutput{
				Content:      "hello world",
				ContentParts: []llm.ToolContentPart{textPart("hello"), textPart(" world")},
			},
		},
		{
			name:    "image only",
			content: []sdkmcp.Content{&sdkmcp.ImageContent{MIMEType: "image/png", Data: []byte("png bytes")}},
			want: llm.ToolOutput{
				ContentParts: []llm.ToolContentPart{imagePart("image/png", []byte("png bytes"))},
			},
		},
		{
			name:    "image MIME type is case normalized",
			content: []sdkmcp.Content{&sdkmcp.ImageContent{MIMEType: "Image/PNG", Data: []byte("png bytes")}},
			want: llm.ToolOutput{
				ContentParts: []llm.ToolContentPart{imagePart("image/png", []byte("png bytes"))},
			},
		},
		{
			name:    "image MIME parameters are removed",
			content: []sdkmcp.Content{&sdkmcp.ImageContent{MIMEType: `image/png; profile="example"`, Data: []byte("png bytes")}},
			want: llm.ToolOutput{
				ContentParts: []llm.ToolContentPart{imagePart("image/png", []byte("png bytes"))},
			},
		},
		{
			name: "mixed content preserves ordering",
			content: []sdkmcp.Content{
				&sdkmcp.TextContent{Text: "before"},
				&sdkmcp.ImageContent{MIMEType: "image/webp", Data: []byte("webp bytes")},
				&sdkmcp.TextContent{Text: "after"},
			},
			want: llm.ToolOutput{
				Content: "beforeafter",
				ContentParts: []llm.ToolContentPart{
					textPart("before"),
					imagePart("image/webp", []byte("webp bytes")),
					textPart("after"),
				},
			},
		},
		{
			name: "unsupported content is retained as text",
			content: []sdkmcp.Content{
				&sdkmcp.AudioContent{MIMEType: "audio/wav", Data: []byte("audio")},
				&sdkmcp.ResourceLink{URI: "https://example.com/file", Name: "file"},
			},
			want: llm.ToolOutput{
				Content: `{"type":"audio","mimeType":"audio/wav","data":"YXVkaW8="}{"type":"resource_link","uri":"https://example.com/file","name":"file"}`,
				ContentParts: []llm.ToolContentPart{
					textPart(`{"type":"audio","mimeType":"audio/wav","data":"YXVkaW8="}`),
					textPart(`{"type":"resource_link","uri":"https://example.com/file","name":"file"}`),
				},
			},
		},
		{
			name:    "empty image falls back to MCP JSON",
			content: []sdkmcp.Content{&sdkmcp.ImageContent{MIMEType: "image/png"}},
			want: llm.ToolOutput{
				Content:      `{"type":"image","mimeType":"image/png","data":""}`,
				ContentParts: []llm.ToolContentPart{textPart(`{"type":"image","mimeType":"image/png","data":""}`)},
			},
		},
		{
			name:    "unsupported image MIME type falls back to MCP JSON",
			content: []sdkmcp.Content{&sdkmcp.ImageContent{MIMEType: "image/svg+xml", Data: []byte("svg")}},
			want: llm.ToolOutput{
				Content:      `{"type":"image","mimeType":"image/svg+xml","data":"c3Zn"}`,
				ContentParts: []llm.ToolContentPart{textPart(`{"type":"image","mimeType":"image/svg+xml","data":"c3Zn"}`)},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := formatContent(test.content)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("formatContent() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestFormatContent_JSONFailureHasVisibleFallback(t *testing.T) {
	output := formatContent([]sdkmcp.Content{&sdkmcp.AudioContent{
		MIMEType: "audio/wav",
		Data:     []byte("audio"),
		Meta:     sdkmcp.Meta{"unencodable": make(chan int)},
	}})

	if len(output.ContentParts) != 1 || output.ContentParts[0].Type != llm.ToolContentPartText {
		t.Fatalf("ContentParts = %#v, want one text fallback", output.ContentParts)
	}
	if !strings.Contains(output.Content, "unsupported MCP content *mcp.AudioContent") ||
		!strings.Contains(output.Content, "JSON encoding failed") {
		t.Fatalf("Content = %q, want visible JSON failure fallback", output.Content)
	}
}

func TestBackgroundOAuthConnection(t *testing.T) {
	for _, tc := range []struct {
		name              string
		token             *oauth2.Token
		reject            bool
		forbidden         bool
		insufficientScope bool
		reconnect         bool
		wantStatus        ServerStatus
		wantRefresh       int32
	}{
		{name: "no grant", wantStatus: StatusAuthRequired},
		{name: "valid grant", token: &oauth2.Token{AccessToken: "valid", Expiry: time.Now().Add(time.Hour)}, wantStatus: StatusReady},
		{name: "rejected grant", token: &oauth2.Token{AccessToken: "rejected", Expiry: time.Now().Add(time.Hour)}, reject: true, wantStatus: StatusAuthRequired},
		{name: "expired grant refreshes", token: &oauth2.Token{AccessToken: "expired", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour)}, wantStatus: StatusReady, wantRefresh: 1},
		{name: "forbidden", forbidden: true, wantStatus: StatusFailed},
		{name: "insufficient scope", insufficientScope: true, wantStatus: StatusAuthRequired},
		{name: "rejected refreshable grant", token: &oauth2.Token{AccessToken: "rejected", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)}, reject: true, reconnect: true, wantStatus: StatusAuthRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var discovery, registration, refreshes atomic.Int32
			var bearer, refreshedBearer atomic.Bool
			server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "protected", Version: "1"}, nil)
			sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "greet"}, func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, any, error) {
				return &sdkmcp.CallToolResult{}, nil, nil
			})
			handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{Stateless: true})
			var httpServer *httptest.Server
			mux := http.NewServeMux()
			mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
				auth := r.Header.Get("Authorization")
				if auth == "Bearer rotated" {
					refreshedBearer.Store(true)
				}
				if !tc.reject && !tc.forbidden && !tc.insufficientScope && (auth == "Bearer valid" || auth == "Bearer rotated") {
					bearer.Store(true)
					handler.ServeHTTP(w, r)
					return
				}
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, httpServer.URL+"/.well-known/oauth-protected-resource/mcp"))
				if tc.insufficientScope {
					w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
					w.WriteHeader(http.StatusForbidden)
				} else if tc.forbidden {
					w.WriteHeader(http.StatusForbidden)
				} else {
					w.WriteHeader(http.StatusUnauthorized)
				}
			})
			writeJSON := func(w http.ResponseWriter, value any) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(value)
			}
			mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
				discovery.Add(1)
				writeJSON(w, map[string]any{"resource": httpServer.URL + "/mcp", "authorization_servers": []string{httpServer.URL}})
			})
			mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
				discovery.Add(1)
				writeJSON(w, map[string]any{"issuer": httpServer.URL, "authorization_endpoint": httpServer.URL + "/authorize", "token_endpoint": httpServer.URL + "/token", "registration_endpoint": httpServer.URL + "/register", "response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"}})
			})
			mux.HandleFunc("/.well-known/", func(w http.ResponseWriter, r *http.Request) { discovery.Add(1); http.NotFound(w, r) })
			mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
				registration.Add(1)
				writeJSON(w, map[string]any{"client_id": "dynamic", "redirect_uris": []string{"http://127.0.0.1/callback"}, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
			})
			mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
				refreshes.Add(1)
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh" {
					t.Errorf("refresh form = %v", r.Form)
				}
				writeJSON(w, map[string]any{"access_token": "rotated", "refresh_token": "rotated-refresh", "token_type": "Bearer", "expires_in": 3600})
			})
			httpServer = httptest.NewServer(mux)
			defer httpServer.Close()
			endpoint := httpServer.URL + "/mcp"
			store := mcpoauth.NewFileStore(filepath.Join(t.TempDir(), "oauth.json"))
			if tc.token != nil {
				_, err := store.Update(endpoint, func(*mcpoauth.Session) (*mcpoauth.Session, error) {
					return &mcpoauth.Session{Endpoint: endpoint, Issuer: httpServer.URL, Config: mcpoauth.OAuth2Config{ClientID: "stored", Endpoint: oauth2.Endpoint{TokenURL: httpServer.URL + "/token", AuthURL: httpServer.URL + "/authorize", AuthStyle: oauth2.AuthStyleInParams}}, Token: tc.token}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			manager := NewManagerWithConfig(&Config{Servers: map[string]ServerConfig{"protected": {Type: "http", URL: endpoint}}})
			manager.oauthCoordinator = mcpoauth.NewCoordinator(store)
			defer manager.StopAll()
			updates := make(chan StatusUpdate, 10)
			manager.SetStatusChannel(updates)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := manager.Enable(ctx, "protected"); err != nil {
				t.Fatal(err)
			}
			attempt := 0
			for {
				select {
				case update := <-updates:
					if update.Status == StatusStarting || update.Status == StatusStopped {
						continue
					}
					if update.Status != tc.wantStatus {
						t.Fatalf("status = %v (%v), want %v", update.Status, update.Error, tc.wantStatus)
					}
					if tc.wantStatus == StatusAuthRequired && !errors.Is(update.Error, mcpoauth.ErrAuthenticationRequired) {
						t.Errorf("error = %v, want ErrAuthenticationRequired", update.Error)
					}
					if tc.forbidden && errors.Is(update.Error, mcpoauth.ErrAuthenticationRequired) {
						t.Errorf("403 misclassified: %v", update.Error)
					}
					if got := discovery.Load(); got != 0 {
						t.Errorf("metadata requests = %d, want 0", got)
					}
					if got := registration.Load(); got != 0 {
						t.Errorf("registrations = %d, want 0", got)
					}
					wantRefresh := tc.wantRefresh + int32(attempt)
					if got := refreshes.Load(); got != wantRefresh {
						t.Errorf("refreshes = %d, want %d", got, wantRefresh)
					}
					if tc.wantStatus == StatusReady && !bearer.Load() {
						t.Error("resource did not receive stored/refreshed bearer")
					}
					if wantRefresh > 0 {
						if !refreshedBearer.Load() {
							t.Error("refreshed bearer was not sent to the resource")
						}
						stored, err := store.Load(endpoint)
						if err != nil {
							t.Fatal(err)
						}
						if stored.Token.AccessToken != "rotated" || stored.Token.RefreshToken != "rotated-refresh" {
							t.Error("rotated grant not persisted")
						}
					}
					if tc.reject {
						wantState := mcpoauth.AuthRequired
						if tc.token.RefreshToken != "" {
							wantState = mcpoauth.AuthExpired
						}
						if got := manager.oauthCoordinator.Status(endpoint).State; got != wantState {
							t.Errorf("rejected grant state = %s, want %s", got, wantState)
						}
					}
					if tc.reconnect && attempt == 0 {
						attempt++
						if err := manager.Restart(ctx, "protected"); err != nil {
							t.Fatal(err)
						}
						continue
					}
					return
				case <-ctx.Done():
					t.Fatal("startup did not settle")
				}
			}
		})
	}
}

func TestBackgroundInsufficientScopePreservesOtherToolCalls(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "step-up", Version: "1"}, nil)
	for _, name := range []string{"restricted", "normal"} {
		sdkmcp.AddTool(server, &sdkmcp.Tool{Name: name}, func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{}, nil, nil
		})
	}
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{Stateless: true})
	var normalCalls atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer valid" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(strings.NewReader(string(data)))
			var message struct {
				Method string
				Params struct{ Name string }
			}
			if err := json.Unmarshal(data, &message); err != nil {
				t.Error(err)
				return
			}
			if message.Method == "tools/call" && message.Params.Name == "restricted" {
				w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="write"`)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if message.Method == "tools/call" && message.Params.Name == "normal" {
				normalCalls.Add(1)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer httpServer.Close()
	store := mcpoauth.NewFileStore(filepath.Join(t.TempDir(), "oauth.json"))
	before, err := store.Update(httpServer.URL, func(*mcpoauth.Session) (*mcpoauth.Session, error) {
		return &mcpoauth.Session{Endpoint: httpServer.URL, Config: mcpoauth.OAuth2Config{ClientID: "client"}, Token: &oauth2.Token{AccessToken: "valid", Expiry: time.Now().Add(time.Hour)}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient("step-up", ServerConfig{URL: httpServer.URL})
	client.oauthCoordinator = mcpoauth.NewCoordinator(store)
	defer client.Stop()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CallTool(ctx, "restricted", nil); !errors.Is(err, mcpoauth.ErrAuthenticationRequired) {
		t.Fatalf("restricted tool error = %v", err)
	}
	if got := client.oauthCoordinator.Status(httpServer.URL).State; got != mcpoauth.AuthSignedIn {
		t.Errorf("scope challenge changed auth state to %s", got)
	}
	after, err := store.Load(httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version || !after.Token.Expiry.Equal(before.Token.Expiry) {
		t.Error("scope challenge changed stored grant")
	}
	if _, err := client.CallTool(ctx, "normal", nil); err != nil {
		t.Fatalf("unrelated tool after scope challenge: %v", err)
	}
	if normalCalls.Load() != 1 {
		t.Error("subsequent request did not send the valid stored bearer")
	}
}

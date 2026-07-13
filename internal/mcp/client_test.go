package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/llm"
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

	transport := client.createStdioTransport(context.Background())
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

	transport := client.createStdioTransport(context.Background())
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

	transport := client.createStdioTransport(context.Background())
	ct, ok := transport.(*sdkmcp.CommandTransport)
	if !ok {
		t.Fatal("expected sdkmcp.CommandTransport")
	}

	if ct.Command.Env != nil {
		t.Error("expected nil env when env map is empty")
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

	transport := client.createStdioTransport(context.Background())
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

func TestCreateHTTPTransport_UsesTransportLevelTimeouts(t *testing.T) {
	client := &Client{
		name: "test",
		config: ServerConfig{
			URL: "https://example.com/mcp",
		},
	}

	transport := client.createHTTPTransport()
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

	transport := client.createHTTPTransport()
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

func TestCreateStdioTransport_ConfiguresProcessGroupCancellation(t *testing.T) {
	client := &Client{
		name: "test",
		config: ServerConfig{
			Command: "echo",
			Args:    []string{"hello"},
		},
	}

	transport := client.createStdioTransport(context.Background())
	ct := transport.(*sdkmcp.CommandTransport)

	if ct.Command.Cancel == nil {
		t.Fatalf("expected subprocess cancel hook to be configured")
	}
	if ct.Command.SysProcAttr == nil || !ct.Command.SysProcAttr.Setpgid {
		t.Fatalf("expected subprocess to run in its own process group")
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

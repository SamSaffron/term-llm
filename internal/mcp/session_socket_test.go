//go:build unix

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/mcpsession"
)

type counterIn struct {
	By int `json:"by,omitempty"`
}

func TestSessionSocketSharesLiveServerState(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mcps")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)

	var counter atomic.Int64
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "counter", Version: "1"}, nil)
	sdkmcp.AddTool(srv, &sdkmcp.Tool{Name: "increment", Description: "increment the counter"},
		func(ctx context.Context, req *sdkmcp.CallToolRequest, in counterIn) (*sdkmcp.CallToolResult, any, error) {
			by := in.By
			if by == 0 {
				by = 1
			}
			v := counter.Add(int64(by))
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: fmt.Sprintf("counter=%d", v)}}}, nil, nil
		})
	httpSrv := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil))
	t.Cleanup(httpSrv.Close)

	manager := NewManagerWithConfig(&Config{Servers: map[string]ServerConfig{
		"counter": {Type: "http", URL: httpSrv.URL},
	}})
	t.Cleanup(manager.StopAll) // runs before httpSrv.Close (LIFO)
	manager.SetSessionID("sess-counter")
	if err := manager.Enable(context.Background(), "counter"); err != nil {
		t.Fatal(err)
	}
	waitForServerStatus(t, manager, "counter", StatusReady, 10*time.Second)

	path := manager.SessionSocketPath()
	if path == "" {
		t.Fatal("expected a session socket")
	}
	if got := mcpsession.Lookup("sess-counter"); got != path {
		t.Fatalf("registry lookup = %q, want %q", got, path)
	}
	if got := mcpsession.Lookup("other"); got != "" {
		t.Fatalf("bound socket leaked to another session: %q", got)
	}

	// Direct (agent) call, then socket (shell) calls: one counter.
	if out, err := manager.CallCatalogTool(context.Background(), "counter", "increment", "counter__increment", json.RawMessage(`{}`)); err != nil || !strings.Contains(out.Content, "counter=1") {
		t.Fatalf("direct call = %q, %v", out.Content, err)
	}
	client, err := mcpsession.Dial("sess-counter")
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Call(context.Background(), "counter", "increment", json.RawMessage(`{"by":5}`))
	if err != nil || !strings.Contains(res.Content, "counter=6") {
		t.Fatalf("socket call = %+v, %v", res, err)
	}
	info, err := client.Session(context.Background())
	if err != nil || info.SessionID != "sess-counter" || len(info.Servers) != 1 || info.Servers[0].Status != string(StatusReady) || len(info.Servers[0].Tools) != 1 {
		t.Fatalf("session info = %+v, %v", info, err)
	}
	if _, err := client.Call(context.Background(), "not-enabled", "increment", nil); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("socket must not reach servers the session has not enabled: %v", err)
	}

	manager.StopAll()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket should be removed on StopAll, stat err = %v", err)
	}
	if got := mcpsession.Lookup("sess-counter"); got != "" {
		t.Fatalf("registry still points at stopped socket: %q", got)
	}
}

func TestUnboundManagerNeverListens(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mcps")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)

	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "noop", Version: "1"}, nil)
	httpSrv := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil))
	t.Cleanup(httpSrv.Close)
	manager := NewManagerWithConfig(&Config{Servers: map[string]ServerConfig{"noop": {Type: "http", URL: httpSrv.URL}}})
	t.Cleanup(manager.StopAll)
	if err := manager.Enable(context.Background(), "noop"); err != nil {
		t.Fatal(err)
	}
	waitForServerStatus(t, manager, "noop", StatusReady, 10*time.Second)
	if path := manager.SessionSocketPath(); path != "" {
		t.Fatalf("unbound manager must not listen, got %s", path)
	}
	// Marking it as the process session afterwards starts the socket for the
	// already-running servers.
	manager.UseAsProcessSession()
	path := manager.SessionSocketPath()
	if path == "" {
		t.Fatal("process-session manager should listen once marked")
	}
	if got := mcpsession.Lookup(""); got != path {
		t.Fatalf("process default lookup = %q, want %q", got, path)
	}
	if got := mcpsession.Lookup("subagent-session"); got != "" {
		t.Fatalf("process default leaked to a caller with its own session: %q", got)
	}
}

func startNoopMCP(t *testing.T) string {
	t.Helper()
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "noop", Version: "1"}, nil)
	httpSrv := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil))
	t.Cleanup(httpSrv.Close)
	return httpSrv.URL
}

func enabledManager(t *testing.T, url string) *Manager {
	t.Helper()
	m := NewManagerWithConfig(&Config{Servers: map[string]ServerConfig{"noop": {Type: "http", URL: url}}})
	t.Cleanup(m.StopAll)
	if err := m.Enable(context.Background(), "noop"); err != nil {
		t.Fatal(err)
	}
	waitForServerStatus(t, m, "noop", StatusReady, 10*time.Second)
	return m
}

// Serve builds a replacement runtime (model swap, idle refresh) while the old
// one for the same session is still alive. Both must listen, the candidate
// must win, and retiring the old runtime must not disturb the candidate.
func TestSessionSocketSurvivesRuntimeReplacement(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mcps")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	url := startNoopMCP(t)

	old := enabledManager(t, url)
	old.SetSessionID("swap-session")
	oldPath := old.SessionSocketPath()

	candidate := enabledManager(t, url)
	candidate.SetSessionID("swap-session")
	newPath := candidate.SessionSocketPath()
	if oldPath == "" || newPath == "" || oldPath == newPath {
		t.Fatalf("both runtimes must listen on distinct sockets: old=%q new=%q", oldPath, newPath)
	}
	if got := mcpsession.Lookup("swap-session"); got != newPath {
		t.Fatalf("candidate should win during overlap, got %q", got)
	}
	old.StopAll() // commit: retire the previous runtime
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("retiring the old runtime removed the candidate socket: %v", err)
	}
	if got := mcpsession.Lookup("swap-session"); got != newPath {
		t.Fatalf("candidate lost its registration after retirement, got %q", got)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old socket should be removed, stat err = %v", err)
	}
}

func TestSessionSocketRollbackRestoresRetainedRuntime(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mcps")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	url := startNoopMCP(t)

	retained := enabledManager(t, url)
	retained.SetSessionID("rollback-session")
	candidate := enabledManager(t, url)
	candidate.SetSessionID("rollback-session")
	candidate.StopAll() // rollback: discard the candidate
	if got := mcpsession.Lookup("rollback-session"); got != retained.SessionSocketPath() || got == "" {
		t.Fatalf("rollback should route back to the retained runtime, got %q", got)
	}
}

func TestSetSessionIDRebindMovesSocketLabel(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mcps")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	m := enabledManager(t, startNoopMCP(t))
	m.SetSessionID("first")
	first := m.SessionSocketPath()
	m.SetSessionID("second")
	second := m.SessionSocketPath()
	if mcpsession.LabelFromPath(second) != "second" {
		t.Fatalf("rebound socket label = %q", mcpsession.LabelFromPath(second))
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("previous socket should be removed on rebind, stat err = %v", err)
	}
	if mcpsession.Lookup("first") != "" || mcpsession.Lookup("second") != second {
		t.Fatalf("registry not moved: first=%q second=%q", mcpsession.Lookup("first"), mcpsession.Lookup("second"))
	}
	if path, err := mcpsession.ResolveTarget("second"); err != nil || path != second {
		t.Fatalf("ResolveTarget(second) = %q, %v", path, err)
	}
}

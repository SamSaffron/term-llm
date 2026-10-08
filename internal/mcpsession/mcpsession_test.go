package mcpsession

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func useTempRuntimeDir(t *testing.T) string {
	t.Helper()
	// Short base keeps socket paths under sun_path limits on macOS.
	dir, err := os.MkdirTemp("/tmp", "mcps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)
	return dir
}

func TestLookupBoundSessionsDoNotLeak(t *testing.T) {
	unA := Register("session-a", "/a.sock")
	defer unA()
	unB := Register("session-b", "/b.sock")
	defer unB()

	if got := Lookup("session-a"); got != "/a.sock" {
		t.Fatalf("Lookup(session-a) = %q", got)
	}
	if got := Lookup("session-c"); got != "" {
		t.Fatalf("unrelated session must not receive another session's socket, got %q", got)
	}
	if got := Lookup(""); got != "" {
		t.Fatalf("empty session must not fall back to a bound socket, got %q", got)
	}
}

func TestLookupProcessDefaultRules(t *testing.T) {
	un := RegisterProcessDefault("/cli.sock")
	if got := Lookup("some-cli-session"); got != "/cli.sock" {
		t.Fatalf("Lookup with sole process default = %q", got)
	}
	if got := Lookup(""); got != "/cli.sock" {
		t.Fatalf("Lookup without session context = %q", got)
	}
	bound := Register("bound", "/bound.sock")
	if got := Lookup("bound"); got != "/bound.sock" {
		t.Fatalf("exact binding must win over process default, got %q", got)
	}
	bound()
	un2 := RegisterProcessDefault("/cli2.sock")
	if got := Lookup("some-cli-session"); got != "" {
		t.Fatalf("ambiguous process defaults must not resolve, got %q", got)
	}
	un2()
	un()
	if got := Lookup("some-cli-session"); got != "" {
		t.Fatalf("unregistered default still resolves: %q", got)
	}
	// An empty session ID is not a binding.
	unEmpty := Register("", "/empty.sock")
	defer unEmpty()
	if got := Lookup("anything"); got != "" {
		t.Fatalf("empty binding must never resolve, got %q", got)
	}
}

func TestSocketPathSanitizesAndBoundsLength(t *testing.T) {
	dir := useTempRuntimeDir(t)
	path, err := SocketPath("abc/../def ghi")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Join(dir, "term-llm") || strings.Contains(filepath.Base(path), "/") {
		t.Fatalf("unexpected path %q", path)
	}
	long, err := SocketPath(strings.Repeat("x", 300))
	if err != nil {
		t.Fatal(err)
	}
	if len(long) > maxSocketPath {
		t.Fatalf("socket path too long: %d", len(long))
	}
	info, err := os.Stat(filepath.Join(dir, "term-llm"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode = %v, want 0700", info.Mode().Perm())
	}
}

func TestResolveTarget(t *testing.T) {
	useTempRuntimeDir(t)
	if got, _ := ResolveTarget("/x/y.sock"); got != "/x/y.sock" {
		t.Fatalf("path target = %q", got)
	}
	got, err := ResolveTarget("sess-1")
	if err != nil || filepath.Base(got) != "mcp-sess-1.sock" {
		t.Fatalf("id target = %q, %v", got, err)
	}
	if LabelFromPath(got) != "sess-1" {
		t.Fatalf("LabelFromPath = %q", LabelFromPath(got))
	}
}

func TestListenReplacesStaleSocketAndRefusesLiveOne(t *testing.T) {
	useTempRuntimeDir(t)
	path, err := SocketPath("stale")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("second Listen on live socket = %v, want in-use error", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v", info.Mode().Perm(), err)
	}
	// Simulate a crashed owner: close the listener but leave the file behind.
	if ul, ok := ln.(interface{ SetUnlinkOnClose(bool) }); ok {
		ul.SetUnlinkOnClose(false)
	}
	ln.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("expected stale socket file to remain: %v", err)
	}
	ln2, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	ln2.Close()
}

func TestClientRoundTrip(t *testing.T) {
	useTempRuntimeDir(t)
	path, err := SocketPath("rt")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/session", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, SessionInfo{SessionID: "rt", PID: 7, Servers: []ServerInfo{{Name: "srv", Status: "ready"}}})
	})
	mux.HandleFunc("POST /v1/call", func(w http.ResponseWriter, r *http.Request) {
		var req CallRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Server != "srv" {
			WriteError(w, http.StatusConflict, "MCP server "+req.Server+" is not running")
			return
		}
		WriteJSON(w, http.StatusOK, CallResult{Content: req.Tool + ":" + string(req.Arguments)})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	client, err := Dial("rt")
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.Session(context.Background())
	if err != nil || info.PID != 7 || len(info.Servers) != 1 {
		t.Fatalf("Session = %+v, %v", info, err)
	}
	res, err := client.Call(context.Background(), "srv", "echo", json.RawMessage(`{"a":1}`))
	if err != nil || res.Content != `echo:{"a":1}` {
		t.Fatalf("Call = %+v, %v", res, err)
	}
	if _, err := client.Call(context.Background(), "nope", "echo", nil); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("Call to missing server = %v, want server error", err)
	}
	if _, err := Dial("missing-session"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("Dial missing = %v", err)
	}
}

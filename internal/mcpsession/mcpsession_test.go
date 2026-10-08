//go:build unix

package mcpsession

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestLookupNewestBindingWinsAndUnregisterRestoresPrevious(t *testing.T) {
	unOld := Register("swap", "/old.sock")
	defer unOld()
	unNew := Register("swap", "/new.sock")
	if got := Lookup("swap"); got != "/new.sock" {
		t.Fatalf("replacement runtime should win, got %q", got)
	}
	unNew() // rollback: candidate discarded
	if got := Lookup("swap"); got != "/old.sock" {
		t.Fatalf("rollback should restore the retained runtime, got %q", got)
	}
}

func TestLookupProcessDefaultOnlyForSessionlessCallers(t *testing.T) {
	un := RegisterProcessDefault("/cli.sock")
	if got := Lookup(""); got != "/cli.sock" {
		t.Fatalf("session-less caller should get the process default, got %q", got)
	}
	if got := Lookup("child-subagent"); got != "" {
		t.Fatalf("a caller with its own session must not inherit the process default, got %q", got)
	}
	un2 := RegisterProcessDefault("/cli2.sock")
	if got := Lookup(""); got != "" {
		t.Fatalf("ambiguous process defaults must not resolve, got %q", got)
	}
	un2()
	un()
	if got := Lookup(""); got != "" {
		t.Fatalf("unregistered default still resolves: %q", got)
	}
	unEmpty := Register("", "/empty.sock")
	defer unEmpty()
	if got := Lookup(""); got != "" {
		t.Fatalf("an empty binding is not a process default, got %q", got)
	}
}

func TestNewSocketPathIsUniqueSanitizedAndBounded(t *testing.T) {
	dir := useTempRuntimeDir(t)
	a, err := NewSocketPath("abc/../def ghi")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSocketPath("abc/../def ghi")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("socket paths must be unique per manager, both %q", a)
	}
	if filepath.Dir(a) != filepath.Join(dir, "term-llm") {
		t.Fatalf("unexpected dir for %q", a)
	}
	if got := LabelFromPath(a); got != "abc_.._def_ghi" {
		t.Fatalf("LabelFromPath = %q", got)
	}
	long, err := NewSocketPath(strings.Repeat("x", 300))
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

func TestNewSocketPathRejectsOverlongDirectory(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "mcps")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	deep := filepath.Join(base, strings.Repeat("d", 90))
	t.Setenv("XDG_RUNTIME_DIR", deep)
	if _, err := NewSocketPath("s"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected path-length error, got %v", err)
	}
}

func TestListenOnlyReplacesDefinitivelyStaleSockets(t *testing.T) {
	useTempRuntimeDir(t)
	path, err := NewSocketPath("stale")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path); err == nil {
		t.Fatal("Listen over a live socket must fail")
	}
	if Stale(path) {
		t.Fatal("a live socket is not stale")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v", info.Mode().Perm(), err)
	}
	// Simulate a crashed owner: close the listener but leave the file behind.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if !Stale(path) {
		t.Fatal("an orphaned socket should be stale")
	}
	ln2, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	ln2.Close()

	// Regular files are never treated as stale sockets.
	plain := filepath.Join(filepath.Dir(path), "mcp-plain@00000000.sock")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Stale(plain) {
		t.Fatal("regular file must not be stale")
	}
	if _, err := Listen(plain); err == nil {
		t.Fatal("Listen must not replace a non-socket file")
	}
	SweepStale()
	if _, err := os.Stat(plain); err != nil {
		t.Fatalf("SweepStale removed a regular file: %v", err)
	}
}

func serveFake(t *testing.T, path, sessionID string) func() {
	t.Helper()
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/session", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, SessionInfo{SessionID: sessionID, PID: 7, Servers: []ServerInfo{{Name: "srv", Status: "ready"}}})
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
	return func() { srv.Close() }
}

func TestClientRoundTripAndResolveNewestLiveSocket(t *testing.T) {
	useTempRuntimeDir(t)
	older, _ := NewSocketPath("rt")
	stopOld := serveFake(t, older, "rt")
	defer stopOld()
	time.Sleep(20 * time.Millisecond) // distinct mtimes
	newer, _ := NewSocketPath("rt")
	stopNew := serveFake(t, newer, "rt")
	defer stopNew()

	if got, err := ResolveTarget("rt"); err != nil || got != newer {
		t.Fatalf("ResolveTarget(rt) = %q, %v; want newest %q", got, err, newer)
	}
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

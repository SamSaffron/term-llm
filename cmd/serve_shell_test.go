//go:build !windows && !plan9 && !js && !wasip1

package cmd

import (
	"bufio"
	"bytes"
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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/worktree"
)

func newShellTestServer(t *testing.T, requireAuth bool) (*serveServer, session.Store, string) {
	t.Helper()
	t.Setenv("SHELL", "/bin/sh")
	store, err := session.NewStore(session.Config{Enabled: true, Path: t.TempDir() + "/sessions.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cwd := t.TempDir()
	now := time.Now().UTC()
	if err := store.Create(context.Background(), &session.Session{
		ID: "shell-session", Provider: "test", Model: "test", Mode: session.ModeChat,
		Origin: session.OriginWeb, Status: session.StatusActive, CWD: cwd, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{
		cfg:   serveServerConfig{ui: true, basePath: "/ui", requireAuth: requireAuth, token: "secret"},
		store: store, startupDir: cwd,
	}
	srv.shutdownCh = make(chan struct{})
	srv.shells = newServeShellManager(time.Minute, srv.shellSessionExists)
	t.Cleanup(srv.closeShellManager)
	return srv, store, cwd
}

func shellJSONRequest(t *testing.T, client *http.Client, method, target, token string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeShellResponse[T any](t *testing.T, response *http.Response) T {
	t.Helper()
	defer response.Body.Close()
	var value T
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatalf("decode status %d: %v", response.StatusCode, err)
	}
	return value
}

type shellCreateResponse struct {
	ShellID string `json:"shell_id"`
	CWD     string `json:"cwd"`
	Created bool   `json:"created"`
}

type shellSSEEvent struct {
	Event string
	Data  map[string]any
}

func readShellSSE(t *testing.T, reader *bufio.Reader) shellSSEEvent {
	t.Helper()
	event := shellSSEEvent{Data: map[string]any{}}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if event.Event != "" {
				return event
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			event.Event = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event.Data); err != nil {
				t.Fatalf("decode SSE data: %v", err)
			}
		}
	}
}

func TestServeShellHTTPCreateInputOutputResizeAttachDeleteAndStaleID(t *testing.T) {
	srv, _, cwd := newShellTestServer(t, true)
	ts := httptest.NewServer(srv.httpHandler())
	defer ts.Close()
	base := ts.URL + "/ui/v1/sessions/shell-session/shell"

	createdResp := shellJSONRequest(t, ts.Client(), http.MethodPost, base, "secret", map[string]any{"cols": 80, "rows": 24})
	if createdResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createdResp.Body)
		createdResp.Body.Close()
		t.Fatalf("create status=%d body=%s", createdResp.StatusCode, body)
	}
	created := decodeShellResponse[shellCreateResponse](t, createdResp)
	if !created.Created || created.ShellID == "" || created.CWD != cwd {
		t.Fatalf("create response = %+v, cwd %q", created, cwd)
	}

	attachedResp := shellJSONRequest(t, ts.Client(), http.MethodPost, base, "secret", map[string]any{"cols": 81, "rows": 25})
	attached := decodeShellResponse[shellCreateResponse](t, attachedResp)
	if attachedResp.StatusCode != http.StatusOK || attached.Created || attached.ShellID != created.ShellID {
		t.Fatalf("attach status=%d response=%+v", attachedResp.StatusCode, attached)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	streamURL := fmt.Sprintf("%s/stream?shell_id=%s&offset=0", base, created.ShellID)
	streamReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	streamReq.Header.Set("Authorization", "Bearer secret")
	streamReq.Header.Set("Accept", "text/event-stream")
	streamResp, err := ts.Client().Do(streamReq)
	if err != nil {
		t.Fatal(err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(streamResp.Body)
		t.Fatalf("stream status=%d body=%s", streamResp.StatusCode, body)
	}
	reader := bufio.NewReader(streamResp.Body)
	if ready := readShellSSE(t, reader); ready.Event != "ready" || ready.Data["shell_id"] != created.ShellID {
		t.Fatalf("ready event = %+v", ready)
	}

	// Exercise the collaboration transition and command contract over the real
	// HTTP SSE stream, not only through direct state snapshots.
	shell, err := srv.shells.get("shell-session", created.ShellID)
	if err != nil {
		t.Fatal(err)
	}
	shell.mu.Lock()
	shell.shellToolAvailable = true
	shell.resetActivityCursorLocked()
	shell.transitionCollaborationLocked(serveShellCollaborationReady, true, "collaboration", "")
	shell.mu.Unlock()
	controller := &serveCollaborativeShellController{manager: func() (*serveShellManager, error) { return srv.shells, nil }}
	sharedResult := make(chan tools.ShellResult, 1)
	sharedErr := make(chan error, 1)
	go func() {
		result, err := controller.Execute(ctx, "shell-session", tools.SharedShellArgs{
			Command: "printf '__shared_sse__\\n'", TimeoutSeconds: 3,
			ExpectedShellID: created.ShellID, OutputLimit: 1 << 20,
		})
		sharedResult <- result
		sharedErr <- err
	}()
	var sharedOutput strings.Builder
	started, finished := false, false
	for !finished || !strings.Contains(sharedOutput.String(), "__shared_sse__") {
		event := readShellSSE(t, reader)
		switch event.Event {
		case "output":
			data, decodeErr := base64.StdEncoding.DecodeString(fmt.Sprint(event.Data["data"]))
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			sharedOutput.Write(data)
		case "agent_command_started":
			_, hasStart := event.Data["start_offset"]
			started = event.Data["command_id"] != "" && hasStart
		case "agent_command_finished":
			end, hasEnd := event.Data["end_offset"].(float64)
			finished = event.Data["command_id"] != "" && hasEnd && end > 0 && event.Data["result_kind"] == "completed"
		}
	}
	result, err := <-sharedResult, <-sharedErr
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "__shared_sse__") || !started || !finished {
		t.Fatalf("shared result=%+v err=%v started=%v finished=%v browser=%q", result, err, started, finished, sharedOutput.String())
	}

	resizeResp := shellJSONRequest(t, ts.Client(), http.MethodPost, base+"/resize", "secret", map[string]any{
		"shell_id": created.ShellID, "cols": 91, "rows": 37,
	})
	if resizeResp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resizeResp.Body)
		t.Fatalf("resize status=%d body=%s", resizeResp.StatusCode, body)
	}
	resizeResp.Body.Close()

	command := "printf '__shell_pwd__%s\\n' \"$PWD\"; stty size; printf '__shell_'done'__\\n'\n"
	inputResp := shellJSONRequest(t, ts.Client(), http.MethodPost, base+"/input", "secret", map[string]any{
		"shell_id": created.ShellID, "data": base64.StdEncoding.EncodeToString([]byte(command)),
	})
	if inputResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(inputResp.Body)
		t.Fatalf("input status=%d body=%s", inputResp.StatusCode, body)
	}
	inputResp.Body.Close()

	var output strings.Builder
	var nextOffset int64
	for !strings.Contains(output.String(), "__shell_done__") {
		event := readShellSSE(t, reader)
		if event.Event != "output" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(fmt.Sprint(event.Data["data"]))
		if err != nil {
			t.Fatal(err)
		}
		output.Write(data)
		nextOffset = int64(event.Data["next_offset"].(float64))
	}
	if !strings.Contains(output.String(), "__shell_pwd__"+cwd) || !strings.Contains(output.String(), "37 91") {
		t.Fatalf("shell output did not use bound cwd/resize: %q", output.String())
	}
	if nextOffset <= 0 {
		t.Fatal("stream did not advance its monotonic offset")
	}

	deleteResp := shellJSONRequest(t, ts.Client(), http.MethodDelete, base, "secret", map[string]any{"shell_id": created.ShellID})
	if deleteResp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(deleteResp.Body)
		t.Fatalf("delete status=%d body=%s", deleteResp.StatusCode, body)
	}
	deleteResp.Body.Close()
	staleResp := shellJSONRequest(t, ts.Client(), http.MethodPost, base+"/input", "secret", map[string]any{
		"shell_id": created.ShellID, "data": base64.StdEncoding.EncodeToString([]byte("echo stale\n")),
	})
	defer staleResp.Body.Close()
	if staleResp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(staleResp.Body)
		t.Fatalf("stale input status=%d body=%s", staleResp.StatusCode, body)
	}
}

func TestServeShellReplayResetAndExit(t *testing.T) {
	shell := newServeShell("sh_test", "session", t.TempDir())
	payload := bytes.Repeat([]byte("x"), serveShellReplayBytes+123)
	shell.appendOutput(payload)
	snapshot := shell.snapshot(0)
	if !snapshot.reset || snapshot.baseOffset != 123 || snapshot.nextOffset != int64(len(payload)) {
		t.Fatalf("reset snapshot = %+v", snapshot)
	}
	snapshot = shell.snapshot(snapshot.baseOffset)
	if snapshot.reset || snapshot.dataOffset != 123 || len(snapshot.data) != serveShellChunkBytes {
		t.Fatalf("replay snapshot = offset %d len %d reset %t", snapshot.dataOffset, len(snapshot.data), snapshot.reset)
	}
	shell.mu.Lock()
	shell.exited = true
	shell.exitCode = 7
	shell.notifyLocked()
	shell.mu.Unlock()
	exit := shell.snapshot(shell.nextOffset)
	if !exit.exited || exit.exitCode != 7 {
		t.Fatalf("exit snapshot = %+v", exit)
	}
}

func TestResolveShellCWDUsesPersistedProjectBindings(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ctx := context.Background()
	srv, store := newServeProjectTestServer(t)
	now := time.Now().UTC()

	root := newGitRepoForBindingTest(t)
	project := &session.Project{Name: "Shell project", CanonicalDir: root}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	managed, err := worktree.Create(ctx, root, worktree.CreateOptions{Name: "shell-cwd"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = worktree.Remove(context.Background(), managed.Dir, worktree.RemoveOptions{Force: true})
	})

	for _, tc := range []struct {
		name        string
		sessionID   string
		cwd         string
		worktreeDir string
	}{
		{name: "project root", sessionID: "shell-project-root", cwd: root},
		{name: "managed worktree", sessionID: "shell-project-worktree", cwd: managed.Dir, worktreeDir: managed.Dir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			persisted := &session.Session{
				ID: tc.sessionID, Provider: "mock", Model: "mock", Mode: session.ModeChat,
				Origin: session.OriginWeb, Status: session.StatusActive, CreatedAt: now, UpdatedAt: now,
			}
			if err := store.Create(ctx, persisted); err != nil {
				t.Fatal(err)
			}
			if _, err := store.BindSessionWorkspace(ctx, tc.sessionID, session.SessionWorkspaceBinding{
				ProjectID: project.ID, CWD: tc.cwd, WorktreeDir: tc.worktreeDir,
			}); err != nil {
				t.Fatal(err)
			}
			got, err := srv.resolveShellCWD(ctx, tc.sessionID)
			if err != nil {
				t.Fatalf("resolveShellCWD: %v", err)
			}
			if !sameServePath(got, tc.cwd) {
				t.Fatalf("resolveShellCWD() = %q, want %q", got, tc.cwd)
			}
		})
	}
}

func TestResolveShellCWDUsesPersistedNoProjectWorkspace(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	cwd := t.TempDir()
	now := time.Now().UTC()
	if err := store.Create(context.Background(), &session.Session{
		ID: "shell-no-project", Provider: "mock", Model: "mock", Mode: session.ModeChat,
		Origin: session.OriginWeb, Status: session.StatusActive, CWD: cwd, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := srv.resolveShellCWD(context.Background(), "shell-no-project")
	if err != nil {
		t.Fatal(err)
	}
	if !sameServePath(got, cwd) {
		t.Fatalf("resolveShellCWD() = %q, want %q", got, cwd)
	}
}

func TestServeShellProcessDrainsHighVolumeOutputBeforeExit(t *testing.T) {
	dir := t.TempDir()
	const repetitions = 4_096
	const block = "0123456789abcdef0123456789abcdef"
	payload := strings.Repeat(block, repetitions)
	if err := os.WriteFile(filepath.Join(dir, "output.txt"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "write-and-exit.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat output.txt\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", script)

	var outputMu sync.Mutex
	var output bytes.Buffer
	process, err := startServeShellProcess(dir, 80, 24, func(data []byte) {
		// Simulate a temporarily busy consumer so output can still be queued in
		// the PTY when the producer exits.
		time.Sleep(125 * time.Millisecond)
		outputMu.Lock()
		_, _ = output.Write(data)
		outputMu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(process.Close)
	select {
	case exit := <-process.Done():
		if exit.Code != 0 {
			t.Fatalf("exit = %+v", exit)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shell process did not finish")
	}
	outputMu.Lock()
	got := output.Len()
	outputMu.Unlock()
	if want := repetitions * len(block); got != want {
		t.Fatalf("drained output bytes = %d, want %d", got, want)
	}
}

func TestServeShellManagerShutdownPermanentlyDisablesLazyCreation(t *testing.T) {
	srv := &serveServer{}
	const callers = 32
	start := make(chan struct{})
	results := make(chan *serveShellManager, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			manager, err := srv.shellManager()
			if err == nil {
				results <- manager
				return
			}
			if !errors.Is(err, errServeShellClosed) {
				t.Errorf("shellManager error = %v", err)
			}
		}()
	}
	close(start)
	srv.closeShellManager()
	wg.Wait()
	close(results)

	for manager := range results {
		manager.mu.Lock()
		closed := manager.closed
		manager.mu.Unlock()
		if !closed {
			t.Fatal("manager returned during shutdown remained open")
		}
	}
	if manager, err := srv.shellManager(); manager != nil || !errors.Is(err, errServeShellClosed) {
		t.Fatalf("shellManager after shutdown = (%p, %v), want nil shell manager closed", manager, err)
	}
	srv.shellsMu.Lock()
	closed, manager := srv.shellsClosed, srv.shells
	srv.shellsMu.Unlock()
	if !closed || manager != nil {
		t.Fatalf("shutdown state = closed %t manager %p", closed, manager)
	}
}

func TestServeShellCleanupOnIdleSessionDeletionAndShutdown(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	var exists atomic.Bool
	exists.Store(true)
	manager := newServeShellManager(10*time.Millisecond, func(string) bool { return exists.Load() })
	cwd := t.TempDir()
	first, _, err := manager.create("idle", cwd, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	first.mu.Lock()
	first.lastUsed = time.Now().Add(-time.Hour)
	first.mu.Unlock()
	manager.evictExpired()
	waitForShellExit(t, first)

	second, _, err := manager.create("deleted", cwd, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	exists.Store(false)
	manager.evictExpired()
	waitForShellExit(t, second)

	exists.Store(true)
	third, _, err := manager.create("shutdown", cwd, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	manager.Close()
	waitForShellExit(t, third)
	if _, _, err := manager.create("late", cwd, 80, 24); !errors.Is(err, errServeShellClosed) {
		t.Fatalf("create after shutdown error = %v", err)
	}
}

func waitForShellExit(t *testing.T, shell *serveShell) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !shell.alive() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("shell process did not exit")
}

func TestServeShellServerStopTerminatesProcess(t *testing.T) {
	srv, _, cwd := newShellTestServer(t, false)
	manager, err := srv.shellManager()
	if err != nil {
		t.Fatal(err)
	}
	shell, _, err := manager.create("shell-session", cwd, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForShellExit(t, shell)
}

func TestServeShellProcessGroupCleanup(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	manager := newServeShellManager(time.Minute, nil)
	defer manager.Close()
	shell, _, err := manager.create("group", t.TempDir(), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := shell.write([]byte("sleep 30 & child=$!; printf '__child__%s\\n' \"$child\"\n")); err != nil {
		t.Fatal(err)
	}
	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := shell.snapshot(0)
		text := string(snapshot.data)
		if marker := strings.LastIndex(text, "__child__"); marker >= 0 {
			_, _ = fmt.Sscanf(text[marker:], "__child__%d", &childPID)
			if childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID <= 0 {
		t.Fatalf("did not capture background child PID; shell output: %q", shell.snapshot(0).data)
	}
	manager.closeSession("group")
	waitForShellExit(t, shell)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !serveShellTestProcessRunning(childPID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("background process %d survived shell cleanup", childPID)
}

func serveShellTestProcessRunning(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	// Linux keeps a killed orphan visible briefly as a zombie until PID 1
	// reaps it. It can no longer execute and therefore counts as terminated.
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		fields := strings.Fields(string(stat))
		if len(fields) > 2 && fields[2] == "Z" {
			return false
		}
	}
	return true
}

func TestServeShellMethodsAuthOriginAndPathBinding(t *testing.T) {
	srv, _, cwd := newShellTestServer(t, true)
	ts := httptest.NewServer(srv.httpHandler())
	defer ts.Close()
	base := ts.URL + "/ui/v1/sessions/shell-session/shell"

	unauthorized := shellJSONRequest(t, ts.Client(), http.MethodPost, base, "", map[string]any{"cols": 80, "rows": 24})
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.StatusCode)
	}

	request, _ := http.NewRequest(http.MethodPost, base, strings.NewReader(`{"cols":80,"rows":24}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://evil.example")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	crossOrigin, err := ts.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	crossOrigin.Body.Close()
	if crossOrigin.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status=%d", crossOrigin.StatusCode)
	}

	arbitrary := shellJSONRequest(t, ts.Client(), http.MethodPost, base, "secret", map[string]any{"cols": 80, "rows": 24, "cwd": t.TempDir()})
	arbitrary.Body.Close()
	if arbitrary.StatusCode != http.StatusBadRequest {
		t.Fatalf("client path status=%d", arbitrary.StatusCode)
	}

	missing := shellJSONRequest(t, ts.Client(), http.MethodPost, strings.Replace(base, "shell-session", "missing", 1), "secret", map[string]any{"cols": 80, "rows": 24})
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing session status=%d", missing.StatusCode)
	}

	method := shellJSONRequest(t, ts.Client(), http.MethodPatch, base, "secret", nil)
	method.Body.Close()
	if method.StatusCode != http.StatusMethodNotAllowed || method.Header.Get("Allow") != "POST, DELETE" {
		t.Fatalf("method status=%d allow=%q", method.StatusCode, method.Header.Get("Allow"))
	}

	created := shellJSONRequest(t, ts.Client(), http.MethodPost, base, "secret", map[string]any{"cols": 80, "rows": 24})
	payload := decodeShellResponse[shellCreateResponse](t, created)
	if payload.CWD != cwd {
		t.Fatalf("shell cwd=%q want authoritative %q", payload.CWD, cwd)
	}
}

func TestServeShellCapabilityAdvertisesSupportedFirstPartyUI(t *testing.T) {
	srv, _, _ := newShellTestServer(t, false)
	recorder := httptest.NewRecorder()
	srv.handleCapabilities(recorder, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	shell, _ := payload["shell"].(map[string]any)
	if shell["enabled"] != true || shell["transport"] != "http_sse" {
		t.Fatalf("shell capability = %#v", shell)
	}

	srv.cfg.ui = false
	recorder = httptest.NewRecorder()
	srv.handleCapabilities(recorder, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	shell, _ = payload["shell"].(map[string]any)
	if shell["enabled"] != false {
		t.Fatalf("API-only shell capability = %#v", shell)
	}
}

func TestSameOriginShellRequestAcceptsHubBackendHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://reverse.local/v1/sessions/s1/shell", nil)
	req.Header.Set("Origin", "https://hub.example")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if !sameOriginShellRequest(req) {
		t.Fatal("same-origin Hub browser request was rejected because the transport replaced Host")
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if sameOriginShellRequest(req) {
		t.Fatal("cross-site browser request was accepted")
	}
}

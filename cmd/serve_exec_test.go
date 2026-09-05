package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type webExecTestTool struct {
	entered, release chan struct{}
	calls            atomic.Int64
}

func (t *webExecTestTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "write_file", Schema: map[string]any{"type": "object"}}
}
func (t *webExecTestTool) Preview(json.RawMessage) string { return "" }
func (t *webExecTestTool) Execute(ctx context.Context, _ json.RawMessage) (llm.ToolOutput, error) {
	if t.calls.Add(1) == 1 {
		close(t.entered)
		select {
		case <-t.release:
		case <-ctx.Done():
			return llm.ToolOutput{}, ctx.Err()
		}
	}
	return llm.ToolOutput{Content: "persisted effect"}, nil
}

func TestWebExecFailedExecReleasesToolCapableRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sid := session.NewID()
	if err := store.Create(ctx, &session.Session{ID: sid, Provider: "mock", Model: "mock", Origin: session.OriginWeb, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	tool := &webExecTestTool{entered: make(chan struct{}), release: make(chan struct{})}
	provider := llm.NewMockProvider("mock").AddToolCall("before", "write_file", map[string]any{}).AddToolCall("after", "write_file", map[string]any{}).AddTextResponse("finished")
	registry := llm.NewToolRegistry()
	registry.Register(tool)
	rt := &serveRuntime{provider: provider, providerKey: "mock", engine: llm.NewEngine(provider, registry), defaultModel: "mock", store: store}
	rt.Touch()
	s := &serveServer{store: store, shutdownCh: make(chan struct{}), responseRuns: newServeResponseRunManager()}
	defer s.responseRuns.Close()
	defer func() {
		if s.responseLifecycleCancel != nil {
			s.responseLifecycleCancel()
			s.responseLifecycleWG.Wait()
		}
	}()
	c := newWebExecCoordinator(ctx, s, "")
	s.webExec = c
	c.ready = true
	c.safeTool = func(tool llm.Tool) bool { _, ok := tool.(*webExecTestTool); return ok }
	attempted := make(chan struct{})
	var attemptedID string
	var execCalls atomic.Int64
	c.exec = func(path string, args, env []string) error {
		if execCalls.Add(1) > 1 {
			t.Error("duplicate signal queued another exec attempt")
			return errors.New("unexpected duplicate exec")
		}
		duplicateDone := make(chan struct{})
		go func() {
			c.request()
			close(duplicateDone)
		}()
		select {
		case <-duplicateDone:
		case <-time.After(time.Second):
			t.Error("duplicate signal blocked behind exec mutex")
		}
		if tool.calls.Load() != 1 {
			t.Errorf("tool executions at exec=%d", tool.calls.Load())
		}
		messages, err := store.GetMessages(ctx, sid, 0, 0)
		if err != nil {
			t.Error(err)
		}
		results := 0
		users := 0
		for _, message := range messages {
			if message.Role == llm.RoleUser {
				users++
			}
			for _, part := range message.Parts {
				if part.ToolResult != nil {
					results++
				}
			}
		}
		if results != 1 || users != 1 {
			t.Errorf("durable results/users=%d/%d", results, users)
		}
		if os.Getenv("TERM_LLM_RESTART_ID") != "" {
			t.Error("restart hint leaked into process environment")
		}
		for _, entry := range env {
			if strings.HasPrefix(entry, "TERM_LLM_RESTART_ID=") {
				attemptedID = strings.TrimPrefix(entry, "TERM_LLM_RESTART_ID=")
			}
		}
		close(attempted)
		return errors.New("fixture exec failure")
	}
	run, err := s.startResponseRun(rt, true, false, []llm.Message{llm.UserText("work")}, llm.Request{SessionID: sid, Tools: []llm.ToolSpec{tool.Spec()}, MaxTurns: 5}, sid, startResponseRunOptions{uiSession: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-tool.entered:
	case <-time.After(3 * time.Second):
		t.Fatalf("tool not entered: %#v", run.snapshot())
	}
	c.request()
	c.request() // duplicate is the same drain generation
	close(tool.release)
	select {
	case <-attempted:
	case <-time.After(3 * time.Second):
		t.Fatalf("exec not attempted: %#v, rejected=%s", run.snapshot(), c.unsupported)
	}
	select {
	case <-run.settled:
	case <-time.After(3 * time.Second):
		t.Fatal("original invocation did not recover")
	}
	if run.snapshot()["status"] != "completed" || tool.calls.Load() != 2 {
		t.Fatalf("recovery status=%v calls=%d", run.snapshot(), tool.calls.Load())
	}
	entries, err := store.ReadExecHandoff(ctx, attemptedID, c.service)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed exec left intents: %v, %v", entries, err)
	}
	// The provider sees no fake user turn after failed exec.
	for _, request := range provider.RecordedRequests() {
		users := 0
		for _, m := range request.Messages {
			if m.Role == llm.RoleUser {
				users++
			}
		}
		if users != 1 {
			t.Fatalf("fake user injected: %d", users)
		}
	}
}

func TestWebExecDrainTimeoutDoesNotCancelTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &webExecCoordinator{ready: true, ctx: ctx, runs: map[string]*webExecRun{"busy": {ctx: ctx}}, timeout: time.Millisecond}
	c.request()
	c.mu.Lock()
	released := c.release
	c.mu.Unlock()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("drain did not abort")
	}
	if ctx.Err() != nil {
		t.Fatal("drain cancelled the tool owner")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.draining {
		t.Fatal("admission not reopened")
	}
}

func TestWebExecEnvironmentScrub(t *testing.T) {
	t.Setenv("TERM_LLM_RESTART_ID", "old")
	t.Setenv("TERM_LLM_RESTART_SERVICE_ID", "service")
	id, service := captureExecHints()
	if id != "old" || service != "service" || os.Getenv("TERM_LLM_RESTART_ID") != "" || os.Getenv("TERM_LLM_RESTART_SERVICE_ID") != "" {
		t.Fatal("hints not captured and scrubbed")
	}
	env := strings.Join(webExecEnviron("new", "next"), "\n")
	if strings.Count(env, "TERM_LLM_RESTART_ID=") != 1 || !strings.Contains(env, "TERM_LLM_RESTART_ID=new") {
		t.Fatal("replacement environment is not unique")
	}
}

func TestWebExecAdmissionAndCancellation(t *testing.T) {
	for _, scenario := range []string{"mutation", "stop", "quarantined stop", "denied", "unsafe", "read"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &serveServer{cfg: serveServerConfig{basePath: "/ui"}}
			c := &webExecCoordinator{ctx: ctx, server: s, draining: true, release: make(chan struct{})}
			called := false
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
			method, path := "POST", "/ui/v1/responses"
			switch scenario {
			case "stop", "quarantined stop":
				path = "/ui/v1/responses/source/cancel"
				c.quarantined = scenario == "quarantined stop"
			case "denied":
				s.cfg.requireAuth = true
				s.cfg.token = "fixture"
				path = "/ui/v1/sessions/shell"
			case "unsafe":
				c.draining = false
				path = "/ui/v1/sessions/id/shell"
			case "read":
				method = "GET"
				path = "/ui/v1/events"
			}
			recorder := httptest.NewRecorder()
			c.handler(next).ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
			switch scenario {
			case "mutation":
				if called || recorder.Code != 503 {
					t.Fatalf("mutation admitted: %d", recorder.Code)
				}
			case "stop":
				if !called || c.draining {
					t.Fatal("stop did not preempt drain")
				}
			case "quarantined stop":
				if !called || !c.draining {
					t.Fatal("quarantined stop must remain available without reopening admission")
				}
			case "denied":
				if called || recorder.Code != 401 || c.unsupported != "" {
					t.Fatal("denied request affected restart")
				}
			case "unsafe":
				if !called || c.unsupported == "" {
					t.Fatal("unsafe owner was not tracked")
				}
			case "read":
				if !called || !c.draining {
					t.Fatal("read stream was not preserved")
				}
			}
		})
	}
}

func TestWebExecParentCancellationAndQuarantine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &webExecCoordinator{ready: true, ctx: ctx, runs: map[string]*webExecRun{"busy": {}}, timeout: time.Hour}
	c.request()
	c.mu.Lock()
	release := c.release
	c.mu.Unlock()
	cancel()
	select {
	case <-release:
	case <-time.After(time.Second):
		t.Fatal("TERM context did not release drain")
	}
	c.mu.Lock()
	c.draining = true
	c.quarantined = true
	c.release = make(chan struct{})
	c.abortLocked()
	select {
	case <-c.release:
		t.Fatal("quarantined work was resumed")
	default:
	}
	c.mu.Unlock()
}

func TestWebExecStartupReadiness(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &webExecCoordinator{ctx: ctx, runs: map[string]*webExecRun{"busy": {}}, timeout: time.Hour}
	c.request()
	if c.draining || c.requested.Load() {
		t.Fatal("startup signal started or retained a drain")
	}
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	c.request()
	c.mu.Lock()
	draining, release := c.draining, c.release
	c.mu.Unlock()
	if !draining {
		t.Fatal("ready server rejected drain")
	}
	cancel()
	select {
	case <-release:
	case <-time.After(time.Second):
		t.Fatal("drain did not stop")
	}
}

func TestWebExecDoesNotTrustToolNames(t *testing.T) {
	if webExecSafeTool(&webExecTestTool{}) {
		t.Fatal("custom tool claiming write_file was trusted")
	}
}

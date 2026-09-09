package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestSessionInputPreparedRuntimeReuseDuringActivity(t *testing.T) {
	prior := processSessionInputs
	processSessionInputs = newSessionInputCoordinator()
	t.Cleanup(func() { processSessionInputs = prior })
	ctx := context.Background()
	store := inputTestStore(t, filepath.Join(t.TempDir(), "sessions.db"))
	sess := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock-model"}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	srv := newTestServeServer()
	srv.store = store
	t.Cleanup(srv.sessionMgr.Close)
	rt, err := srv.prepareUIRuntime(ctx, serveRuntimeRequest{SessionID: sess.ID, RefreshInputs: true}, serveWorkspaceBinding{})
	if err != nil {
		t.Fatal(err)
	}
	selected := rt.inputs.Load()
	for _, activity := range []string{"main run", "side question", "compaction", "runtime ownership"} {
		t.Run(activity, func(t *testing.T) {
			switch activity {
			case "main run":
				rt.setActiveInterrupt(&runtimeInterruptState{})
				defer rt.setActiveInterrupt(nil)
			case "side question":
				rt.sideQuestion.mu.Lock()
				rt.sideQuestion.running = true
				rt.sideQuestion.mu.Unlock()
				defer func() { rt.sideQuestion.mu.Lock(); rt.sideQuestion.running = false; rt.sideQuestion.mu.Unlock() }()
			case "compaction":
				rt.compacting.Store(true)
				defer rt.compacting.Store(false)
			case "runtime ownership":
				rt.mu.Lock()
				defer rt.mu.Unlock()
			}
			got, err := srv.prepareUIRuntime(ctx, serveRuntimeRequest{SessionID: sess.ID, RefreshInputs: true}, serveWorkspaceBinding{})
			if err != nil || got != rt {
				t.Fatalf("prepared reuse during %s: runtime=%p err=%v", activity, got, err)
			}
			if rt.inputs.Load() != selected {
				t.Fatal("ready session reselected its inputs")
			}
			// Explicit fresh-history replacement still requires idle ownership.
			if _, err := srv.prepareUIRuntime(ctx, serveRuntimeRequest{SessionID: sess.ID, RefreshInputs: true, fresh: true}, serveWorkspaceBinding{}); !errors.Is(err, errServeSessionBusy) {
				t.Fatalf("fresh preparation during activity: %v", err)
			}
		})
	}
}

type sessionInputAdmissionProvider struct {
	llm.Provider
	inspect func() error
}

func (p *sessionInputAdmissionProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	if err := p.inspect(); err != nil {
		return nil, err
	}
	return p.Provider.Stream(ctx, req)
}

// Exercise real HTTP admission while the provider owns the runtime mutex.
// Metadata must remain writable, but neither refresh nor eviction may replace
// the runtime between admission and completion.
func TestSessionInputSynchronousCallsAllowMetadata(t *testing.T) {
	for _, surface := range []string{"chat", "chat stream", "responses", "compaction"} {
		t.Run(surface, func(t *testing.T) {
			ctx := context.Background()
			store := inputTestStore(t, filepath.Join(t.TempDir(), "sessions.db"))
			sess := &session.Session{ID: session.NewID(), Provider: "mock", ProviderKey: "mock", Model: "mock-model", Tools: "saved"}
			if err := store.Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			var last *session.Message
			for _, message := range []llm.Message{llm.UserText("earlier question"), llm.AssistantText("earlier answer")} {
				last = session.NewMessage(sess.ID, message, -1)
				if err := store.AddMessage(ctx, sess.ID, last); err != nil {
					t.Fatal(err)
				}
			}
			srv := newTestServeServer()
			srv.store = store
			t.Cleanup(srv.sessionMgr.Close)
			var inspected atomic.Bool
			provider := &sessionInputAdmissionProvider{Provider: llm.NewMockProvider("mock").AddTextResponse("summary or answer")}
			rt := &serveRuntime{provider: provider, engine: llm.NewEngine(provider, nil), providerKey: "mock", defaultModel: "mock-model", store: store, toolsSetting: sess.Tools}
			putTestSession(srv.sessionMgr, sess.ID, rt)
			provider.inspect = func() error {
				inspected.Store(true)
				patch := httptest.NewRequest(http.MethodPatch, "/v1/sessions/"+sess.ID, strings.NewReader(`{"name":"renamed while running"}`))
				patch.Header.Set("Content-Type", "application/json")
				out := httptest.NewRecorder()
				srv.handleSessionMetadataPatch(out, patch, sess.ID)
				if out.Code != http.StatusOK {
					return fmt.Errorf("metadata during %s: status=%d body=%s", surface, out.Code, out.Body.String())
				}
				_, err := srv.sessionMgr.ReplaceIdleWith(ctx, sess.ID, func(*serveRuntime) bool { return true }, func(context.Context) (*serveRuntime, error) { return nil, errors.New("must not construct replacement") })
				if !errors.Is(err, errServeSessionBusy) {
					return fmt.Errorf("replacement during %s: %v", surface, err)
				}
				return nil
			}
			body := `{"model":"mock-model","messages":[{"role":"user","content":"continue"}]}`
			if surface == "chat stream" {
				body = `{"model":"mock-model","stream":true,"messages":[{"role":"user","content":"continue"}]}`
			}
			if surface == "responses" {
				body = fmt.Sprintf(`{"model":"mock-model","previous_response_id":"resp_msg_%d","input":"continue"}`, last.ID)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(requestSessionIDHeader, sess.ID)
			out := httptest.NewRecorder()
			switch surface {
			case "chat", "chat stream":
				srv.handleChatCompletions(out, req)
			case "responses":
				srv.handleResponses(out, req)
			case "compaction":
				srv.handleSessionRuntimeCompact(out, req, sess.ID)
			}
			if !inspected.Load() {
				t.Fatalf("provider not reached: status=%d body=%s", out.Code, out.Body.String())
			}
			if out.Code != http.StatusOK || strings.Contains(out.Body.String(), `"error"`) {
				t.Fatalf("%s failed: status=%d body=%s", surface, out.Code, out.Body.String())
			}
			saved, err := store.Get(ctx, sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Name != "renamed while running" || saved.Tools != sess.Tools {
				t.Fatalf("metadata not preserved: %+v", saved)
			}
			if rt.hasActiveActivity() {
				t.Fatal("admission activity leaked after completion")
			}
		})
	}
}

func TestSessionInputSynchronousAdmissionProtectsSetupAndRollback(t *testing.T) {
	ctx := context.Background()
	srv := newTestServeServer()
	t.Cleanup(srv.sessionMgr.Close)
	const id = "admission-rollback"
	old, err := srv.sessionMgr.GetOrCreate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	candidate, previous, _, rollback, err := srv.sessionMgr.BeginSwap(ctx, id, srv.sessionMgr.factory)
	if err != nil {
		t.Fatal(err)
	}
	if previous != old {
		t.Fatal("swap lost source runtime")
	}
	release, err := srv.sessionMgr.admitSynchronousActivity(id, candidate, previous)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	assertProtected := func(rt *serveRuntime) {
		t.Helper()
		// This deliberately precedes runOnce: neither rt.mu nor activeInterrupt
		// protects the pointer yet. The admission lease must do so on its own.
		if _, _, err := srv.sessionMgr.lockIdleMetadataMutation(id); !errors.Is(err, errServeSessionBusy) {
			t.Fatalf("idle mutation during admission: %v", err)
		}
		_, err := srv.sessionMgr.ReplaceIdleWith(ctx, id, func(*serveRuntime) bool { return true }, srv.sessionMgr.factory)
		if !errors.Is(err, errServeSessionBusy) {
			t.Fatalf("replacement before runtime ownership: %v", err)
		}
		if _, _, _, _, err := srv.sessionMgr.BeginSwap(ctx, id, srv.sessionMgr.factory); !errors.Is(err, errServeSessionBusy) {
			t.Fatalf("swap during admission: %v", err)
		}
		rt.lastUsedUnixNano.Store(1)
		srv.sessionMgr.evictExpired()
		if current, ok := srv.sessionMgr.Get(id); !ok || current != rt {
			t.Fatal("admitted runtime evicted")
		}
		unlock, err := srv.lockSessionInputMetadata(id)
		if err != nil {
			t.Fatalf("admission still holds the metadata operation lock: %v", err)
		}
		unlock()
	}
	assertProtected(candidate)
	rollback()
	assertProtected(previous)
	if _, err := srv.sessionMgr.admitSynchronousActivity(id, candidate); !errors.Is(err, errServeSessionBusy) {
		t.Fatalf("retired candidate admitted: %v", err)
	}
	release()
	release() // Error/success cleanup must be idempotent.
	if old.hasActiveActivity() || candidate.hasActiveActivity() {
		t.Fatal("activity lease leaked after rollback")
	}
	replacement, err := srv.sessionMgr.ReplaceIdleWith(ctx, id, func(*serveRuntime) bool { return true }, srv.sessionMgr.factory)
	if err != nil || replacement == old {
		t.Fatalf("released session remains busy: %v", err)
	}
}

func TestSessionInputSynchronousAdmissionReleasedOnRunFailure(t *testing.T) {
	for _, failure := range []string{"provider error", "cancelled context", "busy runtime"} {
		t.Run(failure, func(t *testing.T) {
			srv := newTestServeServer()
			t.Cleanup(srv.sessionMgr.Close)
			const id = "admission-failure"
			rt, err := srv.sessionMgr.GetOrCreate(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			provider := &sessionInputAdmissionProvider{Provider: llm.NewMockProvider("mock"), inspect: func() error { return errors.New("synthetic provider failure") }}
			rt.provider = provider
			rt.engine = llm.NewEngine(provider, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if failure == "cancelled context" {
				cancel()
			}
			if failure == "busy runtime" {
				rt.mu.Lock()
			}
			runErr := func() error {
				release, err := srv.sessionMgr.admitSynchronousActivity(id, rt)
				if err != nil {
					return err
				}
				defer release()
				_, err = rt.Run(ctx, false, false, []llm.Message{llm.UserText("hello")}, llm.Request{SessionID: id})
				return err
			}()
			if failure == "busy runtime" {
				rt.mu.Unlock()
			}
			if runErr == nil {
				t.Fatal("expected run failure")
			}
			if rt.hasActiveActivity() {
				t.Fatal("run failure leaked admission activity")
			}
			unlock, err := srv.lockSessionInputMetadata(id)
			if err != nil {
				t.Fatal(err)
			}
			unlock()
		})
	}
}

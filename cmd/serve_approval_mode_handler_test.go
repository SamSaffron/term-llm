package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestHandleSessionApprovalModeReportsAndChangesRuntimePolicy(t *testing.T) {
	approval := tools.NewApprovalManager(tools.NewToolPermissions())
	approval.SetPolicyReviewFunc(func(context.Context, tools.PolicyReviewRequest) (tools.PolicyDecision, error) {
		return tools.PolicyDecision{Allowed: true}, nil
	}, nil)
	approval.SetApprovalMode(tools.ModeAuto)
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	runtime := &serveRuntime{
		approvalDefault: tools.ModeAuto,
		toolMgr:         &tools.ToolManager{ApprovalMgr: approval},
	}
	putTestSession(manager, "approval-session", runtime)
	server := &serveServer{sessionMgr: manager}

	get := httptest.NewRecorder()
	server.handleSessionByID(get, httptest.NewRequest(http.MethodGet, "/v1/sessions/approval-session/runtime/approvals", nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"default_mode":"auto"`) || !strings.Contains(get.Body.String(), `"effective_mode":"auto"`) {
		t.Fatalf("GET status/body = %d %s", get.Code, get.Body.String())
	}

	post := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions/approval-session/runtime/approvals", strings.NewReader(`{"mode":"yolo"}`))
	request.Header.Set("Content-Type", "application/json")
	server.handleSessionByID(post, request)
	if post.Code != http.StatusOK || approval.ApprovalMode() != tools.ModeYolo || !strings.Contains(post.Body.String(), `"requested_mode":"yolo"`) {
		t.Fatalf("POST status/body/mode = %d %s %s", post.Code, post.Body.String(), approval.ApprovalMode())
	}
}

func TestHandleSessionApprovalModeMaterializesColdRuntime(t *testing.T) {
	created := 0
	manager := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) {
		created++
		approval := tools.NewApprovalManager(tools.NewToolPermissions())
		approval.SetPolicyReviewFunc(func(context.Context, tools.PolicyReviewRequest) (tools.PolicyDecision, error) {
			return tools.PolicyDecision{Allowed: true}, nil
		}, nil)
		approval.SetApprovalMode(tools.ModeAuto)
		return &serveRuntime{
			approvalDefault: tools.ModeAuto,
			toolMgr:         &tools.ToolManager{ApprovalMgr: approval},
		}, nil
	})
	defer manager.Close()
	server := &serveServer{sessionMgr: manager}

	response := httptest.NewRecorder()
	server.handleSessionByID(response, httptest.NewRequest(http.MethodGet, "/v1/sessions/cold-session/runtime/approvals", nil))
	if response.Code != http.StatusOK || created != 1 || !strings.Contains(response.Body.String(), `"effective_mode":"auto"`) {
		t.Fatalf("status/created/body = %d %d %s", response.Code, created, response.Body.String())
	}
}

func TestHandleSessionApprovalModeColdRuntimeUsesBootModeNotPersistedSession(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const sessionID = "cold-boot-policy"
	if err := store.Create(context.Background(), &session.Session{
		ID: sessionID, Provider: "mock", Model: "mock", ApprovalMode: session.ApprovalModePrompt,
	}); err != nil {
		t.Fatal(err)
	}
	manager := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) {
		approval := tools.NewApprovalManager(tools.NewToolPermissions())
		approval.SetPolicyReviewFunc(func(context.Context, tools.PolicyReviewRequest) (tools.PolicyDecision, error) {
			return tools.PolicyDecision{Allowed: true}, nil
		}, nil)
		approval.SetApprovalMode(tools.ModeAuto)
		return &serveRuntime{
			approvalDefault: tools.ModeAuto,
			toolMgr:         &tools.ToolManager{ApprovalMgr: approval},
		}, nil
	})
	defer manager.Close()
	server := &serveServer{sessionMgr: manager, store: store, approvalDefault: tools.ModeAuto}

	response := httptest.NewRecorder()
	server.handleSessionByID(response, httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sessionID+"/runtime/approvals", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"default_mode":"auto"`) || !strings.Contains(response.Body.String(), `"effective_mode":"auto"`) {
		t.Fatalf("status/body = %d %s", response.Code, response.Body.String())
	}
}

func TestHandleSessionApprovalModeUnavailableForYoloLaunch(t *testing.T) {
	created := 0
	manager := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) {
		created++
		return &serveRuntime{}, nil
	})
	defer manager.Close()
	server := &serveServer{sessionMgr: manager, approvalDefault: tools.ModeYolo}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/v1/sessions/yolo-session/runtime/approvals", strings.NewReader(`{"mode":"prompt"}`))
		request.Header.Set("Content-Type", "application/json")
		server.handleSessionByID(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s status/body = %d %s", method, response.Code, response.Body.String())
		}
	}
	if created != 0 {
		t.Fatalf("Yolo approval controls materialized %d runtimes, want 0", created)
	}
}

func TestHandleSessionApprovalModeDoesNotWaitForActiveTurnLock(t *testing.T) {
	approval := tools.NewApprovalManager(tools.NewToolPermissions())
	approval.SetApprovalMode(tools.ModeYolo)
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	runtime := &serveRuntime{toolMgr: &tools.ToolManager{ApprovalMgr: approval}}
	putTestSession(manager, "active-session", runtime)
	server := &serveServer{sessionMgr: manager}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/sessions/active-session/runtime/approvals", strings.NewReader(`{"mode":"prompt"}`))
		request.Header.Set("Content-Type", "application/json")
		server.handleSessionByID(response, request)
		done <- response
	}()
	select {
	case response := <-done:
		if response.Code != http.StatusOK || approval.ApprovalMode() != tools.ModePrompt {
			t.Fatalf("status/body/mode = %d %s %s", response.Code, response.Body.String(), approval.ApprovalMode())
		}
	case <-time.After(time.Second):
		t.Fatal("approval mode change waited for the active turn lock")
	}
}

func TestHandleSessionApprovalModeDoesNotPersistRuntimeOverride(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess := &session.Session{ID: "runtime-approval", Provider: "mock", Model: "mock", ApprovalMode: session.ApprovalModePrompt}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	approval := tools.NewApprovalManager(tools.NewToolPermissions())
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	putTestSession(manager, sess.ID, &serveRuntime{toolMgr: &tools.ToolManager{ApprovalMgr: approval}})
	server := &serveServer{sessionMgr: manager, store: store}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sess.ID+"/runtime/approvals", strings.NewReader(`{"mode":"yolo"}`))
	request.Header.Set("Content-Type", "application/json")
	server.handleSessionByID(response, request)
	if response.Code != http.StatusOK || approval.ApprovalMode() != tools.ModeYolo {
		t.Fatalf("status/body/mode = %d %s %s", response.Code, response.Body.String(), approval.ApprovalMode())
	}
	persisted, err := store.Get(context.Background(), sess.ID)
	if err != nil || persisted.ApprovalMode != session.ApprovalModePrompt {
		t.Fatalf("persisted mode = %v, %v; want unchanged prompt", persisted, err)
	}
}

func TestHandleSessionStateDoesNotRestorePersistedApprovalPolicy(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess := &session.Session{ID: "cold-policy", Provider: "mock", Model: "mock", ApprovalMode: session.ApprovalModePrompt}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	server := &serveServer{store: store, approvalDefault: tools.ModeAuto}
	response := httptest.NewRecorder()
	server.handleSessionState(response, httptest.NewRequest(http.MethodGet, "/v1/sessions/cold-policy/state", nil), sess.ID)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"approval_policy"`) {
		t.Fatalf("status/body = %d %s", response.Code, response.Body.String())
	}
}

func TestHandleSessionStateOmitsApprovalPolicyForYoloLaunch(t *testing.T) {
	approval := tools.NewApprovalManager(tools.NewToolPermissions())
	approval.SetApprovalMode(tools.ModeYolo)
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	putTestSession(manager, "yolo-state", &serveRuntime{
		approvalDefault: tools.ModeYolo,
		toolMgr:         &tools.ToolManager{ApprovalMgr: approval},
	})
	server := &serveServer{sessionMgr: manager, approvalDefault: tools.ModeYolo}
	response := httptest.NewRecorder()
	server.handleSessionState(response, httptest.NewRequest(http.MethodGet, "/v1/sessions/yolo-state/state", nil), "yolo-state")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"approval_policy"`) {
		t.Fatalf("status/body = %d %s", response.Code, response.Body.String())
	}
}

func TestHandleSessionApprovalModeRejectsAutoWithoutGuardian(t *testing.T) {
	approval := tools.NewApprovalManager(tools.NewToolPermissions())
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	putTestSession(manager, "approval-session", &serveRuntime{
		toolMgr: &tools.ToolManager{ApprovalMgr: approval},
	})
	server := &serveServer{sessionMgr: manager}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions/approval-session/runtime/approvals", strings.NewReader(`{"mode":"auto"}`))
	request.Header.Set("Content-Type", "application/json")
	server.handleSessionByID(response, request)
	if response.Code != http.StatusConflict || approval.ApprovalMode() != tools.ModePrompt {
		t.Fatalf("status/body/mode = %d %s %s", response.Code, response.Body.String(), approval.ApprovalMode())
	}
}

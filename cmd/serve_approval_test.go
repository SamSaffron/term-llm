package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/tools"
)

func newTestRuntime() *serveRuntime {
	return &serveRuntime{}
}

// putTestSession injects a runtime into a session manager for testing.
func putTestSession(mgr *serveSessionManager, id string, rt *serveRuntime) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	rt.Touch()
	mgr.sessions[id] = rt
}

func TestAwaitApproval_NoTransport_FailsFast(t *testing.T) {
	rt := newTestRuntime()
	// No approvalEventFunc set — should fail fast instead of hanging.
	result, err := rt.awaitApproval("/some/path", false, false, "")
	if err != errServeApprovalNoTransport {
		t.Fatalf("expected errServeApprovalNoTransport, got %v", err)
	}
	if result.Choice != 0 {
		t.Errorf("expected zero-value result, got choice=%v", result.Choice)
	}
}

func TestAwaitApproval_WithTransport_EmitsEventAndBlocks(t *testing.T) {
	rt := newTestRuntime()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var capturedEvent string
	var capturedData map[string]any

	rt.approvalMu.Lock()
	rt.approvalEventFunc = func(event string, data map[string]any) error {
		capturedEvent = event
		capturedData = data
		return nil
	}
	rt.approvalCtx = ctx
	rt.approvalMu.Unlock()

	done := make(chan struct{})
	var result tools.ApprovalResult
	var awaitErr error

	go func() {
		result, awaitErr = rt.awaitApproval("/test/file.txt", false, false, "")
		close(done)
	}()

	// Wait for the pending approval to appear
	deadline := time.After(2 * time.Second)
	for {
		prompts := rt.pendingApprovalPrompts()
		if len(prompts) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for pending approval")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Verify SSE event was emitted
	if capturedEvent != "response.approval.prompt" {
		t.Errorf("expected event 'response.approval.prompt', got %q", capturedEvent)
	}
	if capturedData["path"] != "/test/file.txt" {
		t.Errorf("expected path '/test/file.txt', got %v", capturedData["path"])
	}

	// Submit approval
	prompts := rt.pendingApprovalPrompts()
	if len(prompts) == 0 {
		t.Fatal("no pending approvals")
	}

	// Find the "once" option index
	approvalID := prompts[0].ApprovalID
	onceIdx := -1
	rt.approvalMu.Lock()
	pending := rt.pendingApprovals[approvalID]
	rt.approvalMu.Unlock()
	for i, opt := range pending.Options {
		if opt.Choice == tools.ApprovalChoiceOnce {
			onceIdx = i
			break
		}
	}
	if onceIdx < 0 {
		t.Fatal("no 'once' option found")
	}

	if err := rt.submitApproval(approvalID, onceIdx, false); err != nil {
		t.Fatalf("submitApproval failed: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitApproval did not return after submit")
	}

	if awaitErr != nil {
		t.Errorf("unexpected error: %v", awaitErr)
	}
	if result.Choice != tools.ApprovalChoiceOnce {
		t.Errorf("expected ApprovalChoiceOnce, got %v", result.Choice)
	}
}

func TestAwaitApproval_Cancellation(t *testing.T) {
	rt := newTestRuntime()
	ctx, cancel := context.WithCancel(context.Background())

	rt.approvalMu.Lock()
	rt.approvalEventFunc = func(event string, data map[string]any) error { return nil }
	rt.approvalCtx = ctx
	rt.approvalMu.Unlock()

	done := make(chan struct{})
	var result tools.ApprovalResult

	go func() {
		result, _ = rt.awaitApproval("/test/file.txt", false, false, "")
		close(done)
	}()

	// Wait for pending approval to appear
	deadline := time.After(2 * time.Second)
	for {
		if len(rt.pendingApprovalPrompts()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for pending approval")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Cancel the context
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitApproval did not return after cancellation")
	}

	if !result.Cancelled {
		t.Error("expected result.Cancelled=true")
	}
	if result.Choice != tools.ApprovalChoiceCancelled {
		t.Errorf("expected ApprovalChoiceCancelled, got %v", result.Choice)
	}

	// Pending approval should be cleaned up
	if len(rt.pendingApprovalPrompts()) != 0 {
		t.Error("pending approvals should be empty after cancellation")
	}
}

func TestSubmitApproval_DenyFlow(t *testing.T) {
	rt := newTestRuntime()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt.approvalMu.Lock()
	rt.approvalEventFunc = func(event string, data map[string]any) error { return nil }
	rt.approvalCtx = ctx
	rt.approvalMu.Unlock()

	done := make(chan struct{})
	var result tools.ApprovalResult

	go func() {
		result, _ = rt.awaitApproval("/test/file.txt", false, false, "")
		close(done)
	}()

	// Wait for pending approval
	deadline := time.After(2 * time.Second)
	for {
		if len(rt.pendingApprovalPrompts()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for pending approval")
		case <-time.After(10 * time.Millisecond):
		}
	}

	prompts := rt.pendingApprovalPrompts()
	approvalID := prompts[0].ApprovalID

	// Submit as cancelled (deny)
	if err := rt.submitApproval(approvalID, 0, true); err != nil {
		t.Fatalf("submitApproval failed: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitApproval did not return after deny")
	}

	if !result.Cancelled {
		t.Error("expected result.Cancelled=true for deny")
	}
}

func TestSubmitApproval_NotPending(t *testing.T) {
	rt := newTestRuntime()
	err := rt.submitApproval("nonexistent", 0, false)
	if err != errServeApprovalNotPending {
		t.Errorf("expected errServeApprovalNotPending, got %v", err)
	}
}

func TestSubmitApproval_AlreadyAnswered(t *testing.T) {
	rt := newTestRuntime()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt.approvalMu.Lock()
	rt.approvalEventFunc = func(event string, data map[string]any) error { return nil }
	rt.approvalCtx = ctx
	rt.approvalMu.Unlock()

	done := make(chan struct{})
	go func() {
		rt.awaitApproval("/test/file.txt", false, false, "")
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		if len(rt.pendingApprovalPrompts()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for pending approval")
		case <-time.After(10 * time.Millisecond):
		}
	}

	prompts := rt.pendingApprovalPrompts()
	approvalID := prompts[0].ApprovalID

	// First submit should succeed
	if err := rt.submitApproval(approvalID, 0, true); err != nil {
		t.Fatalf("first submit failed: %v", err)
	}

	<-done

	// Second submit should fail
	err := rt.submitApproval(approvalID, 0, true)
	if err == nil {
		t.Error("expected error on double submit")
	}
}

func TestHandleSessionApproval_MissingChoice(t *testing.T) {
	// Verify that omitting "choice" when not cancelled returns 400
	body := `{"approval_id": "appr_test"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/test/approval", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mgr := newServeSessionManager(time.Hour, 10, nil)
	defer mgr.Close()
	rt := newTestRuntime()
	putTestSession(mgr, "test", rt)

	s := &serveServer{sessionMgr: mgr}
	s.handleSessionApproval(w, req, "test")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil || errObj["message"] != "choice is required when not cancelled" {
		t.Errorf("unexpected error response: %s", w.Body.String())
	}
}

func TestHandleSessionApproval_CancelledWithoutChoice(t *testing.T) {
	// Cancelled=true should work even without a choice field
	body := `{"approval_id": "appr_test", "cancelled": true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/test/approval", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	mgr := newServeSessionManager(time.Hour, 10, nil)
	defer mgr.Close()
	rt := newTestRuntime()
	putTestSession(mgr, "test", rt)

	s := &serveServer{sessionMgr: mgr}
	s.handleSessionApproval(w, req, "test")

	// Should get past validation — will fail with "no pending approval" which is expected
	if w.Code == http.StatusBadRequest {
		var resp map[string]any
		json.Unmarshal(w.Body.Bytes(), &resp)
		errObj, _ := resp["error"].(map[string]any)
		if errObj != nil && errObj["message"] == "choice is required when not cancelled" {
			t.Error("cancelled request should not require choice")
		}
	}
}

func TestSnapshotTitle_DerivedFromFlags(t *testing.T) {
	tests := []struct {
		name    string
		isWrite bool
		isShell bool
		want    string
	}{
		{"read", false, false, "Read Access Request"},
		{"write", true, false, "Write Access Request"},
		{"shell", false, true, "Shell Command Request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &servePendingApproval{
				ApprovalID: "test",
				IsWrite:    tt.isWrite,
				IsShell:    tt.isShell,
				Options:    []tools.ApprovalOption{},
				CreatedAt:  time.Now(),
			}
			snap := p.snapshot()
			if snap.Title != tt.want {
				t.Errorf("expected title %q, got %q", tt.want, snap.Title)
			}
		})
	}
}

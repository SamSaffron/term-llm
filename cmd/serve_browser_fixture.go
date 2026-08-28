//go:build browserfixture

package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// This route is compiled only into the isolated browser-test binary. The
// production binary has no fixture endpoint or trigger surface.
func init() {
	registerServeBrowserFixtureRoutes = func(mux *http.ServeMux, server *serveServer) {
		mux.HandleFunc("/__browser_fixture/ask-user", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var request struct {
				SessionID string `json:"session_id"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&request) != nil {
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
				return
			}
			request.SessionID = strings.TrimSpace(request.SessionID)
			runtime, ok := server.sessionMgr.Get(request.SessionID)
			if !ok || runtime == nil {
				http.Error(w, "fixture session is not live", http.StatusNotFound)
				return
			}
			callID := "browser-ask-" + time.Now().UTC().Format("150405.000000000")
			questions := []tools.AskUserQuestion{{
				Header:   "Direction",
				Question: "Which path should the browser fixture take?",
				Options: []tools.AskUserOption{
					{Label: "Safe", Description: "Continue through the tested path"},
					{Label: "Pause", Description: "Leave the request waiting"},
				},
			}}
			ready := make(chan struct{})
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				ctx = llm.ContextWithCallID(ctx, callID)
				runtime.prepareAskUser(callID, questions)
				close(ready)
				_, _ = runtime.awaitAskUser(ctx, questions)
			}()
			select {
			case <-ready:
				writeJSON(w, http.StatusCreated, map[string]string{"call_id": callID})
			case <-r.Context().Done():
			}
		})
		mux.HandleFunc("/__browser_fixture/approval", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var request struct {
				SessionID string `json:"session_id"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&request) != nil {
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
				return
			}
			runtime, ok := server.sessionMgr.Get(strings.TrimSpace(request.SessionID))
			if !ok || runtime == nil {
				http.Error(w, "fixture session is not live", http.StatusNotFound)
				return
			}
			pending, prompt := runtime.prepareApprovalRequest("review.txt", true, false, false, "")
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				defer runtime.removePendingApproval(pending.ApprovalID, pending)
				select {
				case <-pending.responseC:
				case <-ctx.Done():
				}
			}()
			writeJSON(w, http.StatusCreated, map[string]string{"approval_id": prompt.ApprovalID})
		})
		mux.HandleFunc("/__browser_fixture/children", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var request struct {
				SessionID string `json:"session_id"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&request) != nil {
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
				return
			}
			parent, err := server.store.Get(r.Context(), strings.TrimSpace(request.SessionID))
			if err != nil || parent == nil {
				http.Error(w, "fixture parent session is unavailable", http.StatusNotFound)
				return
			}
			now := time.Now().UTC()
			children := []*session.Session{
				{
					ID: "browser-child-complete-" + randomSuffix(), Name: "Completed browser child",
					Provider: parent.Provider, ProviderKey: parent.ProviderKey, Model: parent.Model,
					Mode: session.ModeChat, Origin: session.OriginWeb, Agent: "reviewer", ParentID: parent.ID,
					ProjectID: parent.ProjectID, CWD: parent.CWD, CreatedAt: now.Add(-3 * time.Second),
					UpdatedAt: now.Add(-time.Second), Status: session.StatusComplete,
				},
				{
					ID: "browser-child-failed-" + randomSuffix(), Name: "Failed browser child",
					Provider: parent.Provider, ProviderKey: parent.ProviderKey, Model: parent.Model,
					Mode: session.ModeChat, Origin: session.OriginWeb, Agent: "tester", ParentID: parent.ID,
					ProjectID: parent.ProjectID, CWD: parent.CWD, CreatedAt: now.Add(-2 * time.Second),
					UpdatedAt: now, Status: session.StatusError,
				},
			}
			for index, child := range children {
				if err := server.store.Create(r.Context(), child); err != nil {
					http.Error(w, "fixture child creation failed", http.StatusInternalServerError)
					return
				}
				task := []string{"Review the browser child lifecycle", "Verify the browser child failure"}[index]
				if err := server.store.AddMessage(r.Context(), child.ID, session.NewMessage(child.ID, llm.UserText(task), -1)); err != nil {
					http.Error(w, "fixture child message failed", http.StatusInternalServerError)
					return
				}
			}
			writeJSON(w, http.StatusCreated, map[string]any{"children": []string{children[0].ID, children[1].ID}})
		})
		mux.HandleFunc("/__browser_fixture/file-change", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var request struct {
				SessionID string `json:"session_id"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&request) != nil {
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
				return
			}
			if server.store == nil {
				http.Error(w, "fixture session store is unavailable", http.StatusNotFound)
				return
			}
			sess, err := server.store.Get(r.Context(), strings.TrimSpace(request.SessionID))
			if err != nil || sess == nil {
				http.Error(w, "fixture session workspace is unavailable", http.StatusNotFound)
				return
			}
			root := strings.TrimSpace(sess.CWD)
			if root == "" {
				root, err = server.currentGitRoot()
				if err == nil && strings.TrimSpace(root) != "" {
					sess.CWD = root
					err = server.store.Update(r.Context(), sess)
				}
			}
			if err != nil || strings.TrimSpace(root) == "" {
				http.Error(w, "fixture session workspace is unavailable", http.StatusNotFound)
				return
			}
			path := filepath.Join(root, "review.txt")
			if err := os.WriteFile(path, []byte("original line\nchanged by browser fixture\n"), 0o600); err != nil {
				http.Error(w, "fixture write failed", http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusCreated, map[string]string{"path": "review.txt"})
		})
	}
}

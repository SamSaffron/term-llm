package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
)

func TestCodeModeJobsAuthorityFreezesConfiguredClientAndModelPolicy(t *testing.T) {
	type admissionCall struct {
		model     string
		inherited modelpolicy.Policy
		parent    modelpolicy.ParentModel
	}
	var mu sync.Mutex
	var admissions []admissionCall
	var originHeader, sessionHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/jobs":
			var payload jobsV2AgentJobPayload
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode create payload: %v", err)
				return
			}
			mu.Lock()
			originHeader = r.Header.Get(QueueAgentNotifyOriginHeader)
			sessionHeader = r.Header.Get(QueueAgentNotifySessionIDHeader)
			mu.Unlock()
			writeJSON(t, w, jobsV2AgentJobResponse{ID: "job-bound"})
		case r.Method == http.MethodPost && r.URL.Path == "/v2/jobs/job-bound/trigger":
			writeJSON(t, w, jobsV2AgentRunResponse{ID: "run-bound", JobID: "job-bound", Status: "running"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/runs/run-bound"):
			writeJSON(t, w, jobsV2AgentRunResponse{ID: "run-bound", JobID: "job-bound", Status: "succeeded", Response: "done"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := DefaultToolConfig()
	cfg.Enabled = []string{QueueAgentToolName, WaitForJobsToolName}
	manager, err := NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := manager.Registry
	client := newJobsBackedAgentClient(server.URL, "")
	queue := NewQueueAgentToolWithClient(client, manager.ApprovalMgr, &cfg)
	frozenPolicy := modelpolicy.Policy{}.With("developer", []string{"frozen:*"})
	frozenParent := modelpolicy.ParentModel{Provider: "frozen", Model: "parent"}
	queue.SetModelAdmission(
		func() (modelpolicy.Policy, modelpolicy.ParentModel) { return frozenPolicy, frozenParent },
		func(_ context.Context, name, model string, inherited modelpolicy.Policy, parent modelpolicy.ParentModel) error {
			mu.Lock()
			admissions = append(admissions, admissionCall{model: model, inherited: inherited, parent: parent})
			mu.Unlock()
			return nil
		},
	)
	wait := NewWaitForJobsToolWithClient(client)
	registry.mu.Lock()
	registry.tools[QueueAgentToolName] = queue
	registry.tools[WaitForJobsToolName] = wait
	registry.mu.Unlock()

	authority := captureBindingAuthority(t, registry, "jobs-parent", []string{QueueAgentToolName, WaitForJobsToolName})
	defer authority.Close()
	boundQueue := bindingTool(t, authority, QueueAgentToolName).(*QueueAgentTool)
	boundWait := bindingTool(t, authority, WaitForJobsToolName).(*WaitForJobsTool)
	if boundQueue == queue || boundWait == wait {
		t.Fatal("authority reused mutable live jobs tools")
	}
	if boundQueue.client != client || boundWait.client != client {
		t.Fatal("bound jobs tools did not preserve the configured client")
	}

	// A later turn rewires the live jobs client and policy. The admitted cell
	// must keep its captured client, origin and model policy.
	registry.SetJobsServer("http://127.0.0.1:1", "")
	queue.SetModelAdmission(
		func() (modelpolicy.Policy, modelpolicy.ParentModel) {
			return modelpolicy.Policy{}, modelpolicy.ParentModel{}
		},
		func(context.Context, string, string, modelpolicy.Policy, modelpolicy.ParentModel) error {
			return errors.New("later policy must not apply")
		},
	)

	ctx := core.ContextWithSessionID(context.Background(), "jobs-parent")
	ctx = ContextWithQueueAgentOrigin(ctx, QueueAgentOriginContext{Origin: QueueAgentOriginWeb, SessionID: "jobs-parent"})
	out := lifecycleCall(t, boundQueue, ctx, `{"agent_name":"developer","prompt":"do it","model":"frozen:child","notify_when_done":true}`)
	if out.IsError {
		t.Fatalf("nested queue = %s", out.Content)
	}
	var result QueueAgentResult
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatal(err)
	}
	if result.JobID != "job-bound" || result.RunID != "run-bound" {
		t.Fatalf("nested queue result = %+v", result)
	}
	mu.Lock()
	gotOrigin, gotSession, gotAdmissions := originHeader, sessionHeader, append([]admissionCall(nil), admissions...)
	mu.Unlock()
	if gotOrigin != QueueAgentOriginWeb || gotSession != "jobs-parent" {
		t.Fatalf("nested origin headers = %q/%q", gotOrigin, gotSession)
	}
	if len(gotAdmissions) != 1 {
		t.Fatalf("admission calls = %d", len(gotAdmissions))
	}
	if gotAdmissions[0].model != "frozen:child" || !gotAdmissions[0].inherited.Restricted() || gotAdmissions[0].parent != frozenParent {
		t.Fatalf("nested admission policy was not frozen: %+v", gotAdmissions[0])
	}

	// wait_for_jobs must poll through the same captured client.
	waited := lifecycleCall(t, boundWait, ctx, `{"run_ids":["run-bound"]}`)
	if waited.IsError || !strings.Contains(waited.Content, `"status":"succeeded"`) {
		t.Fatalf("nested wait_for_jobs = %s", waited.Content)
	}
}

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/modelpolicy"
)

func TestQueueAgentFilesystemPathsUseCurrentYoloMode(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		name := "direct"
		if inherited {
			name = "inherited"
		}
		t.Run(name, func(t *testing.T) {
			var queuedNames []string
			triggers := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/jobs":
					var payload jobsV2AgentJobPayload
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("decode queued job: %v", err)
						http.Error(w, "invalid job", http.StatusBadRequest)
						return
					}
					queuedNames = append(queuedNames, payload.RunnerConfig["agent_name"].(string))
					writeJSON(t, w, jobsV2AgentJobResponse{ID: "job_yolo"})
				case "/v2/jobs/job_yolo/trigger":
					triggers++
					writeJSON(t, w, jobsV2AgentRunResponse{ID: "run_yolo", JobID: "job_yolo"})
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			t.Setenv("TERM_LLM_JOBS_SERVER", server.URL)

			approval := NewApprovalManager(NewToolPermissions())
			modeOwner := approval
			if inherited {
				modeOwner = NewApprovalManager(NewToolPermissions())
				if err := approval.SetParent(modeOwner); err != nil {
					t.Fatal(err)
				}
			}
			baseDir := t.TempDir()
			registry, err := NewLocalToolRegistry(&ToolConfig{Enabled: []string{QueueAgentToolName}, BaseDir: baseDir}, nil, approval)
			if err != nil {
				t.Fatal(err)
			}
			registered, ok := registry.Get(QueueAgentToolName)
			if !ok {
				t.Fatal("queue_agent was not registered")
			}
			tool := registered.(*QueueAgentTool)
			var admittedNames []string
			tool.SetModelAdmission(nil, func(_ context.Context, agentName, _ string, _ modelpolicy.Policy, _ modelpolicy.ParentModel) error {
				admittedNames = append(admittedNames, agentName)
				return nil
			})
			for _, mode := range []ApprovalMode{ModePrompt, ModeAuto, ModeYolo, ModeAuto, ModeYolo, ModePrompt} {
				modeOwner.SetApprovalMode(mode)
				for _, agentName := range []string{"reviewer", "./term-llm-agents/reviewer", filepath.Join(baseDir, "reviewer"), `C:\repo\reviewer`, "", "   "} {
					args, err := json.Marshal(QueueAgentArgs{AgentName: agentName, Prompt: "review"})
					if err != nil {
						t.Fatal(err)
					}
					beforeJobs, beforeAdmission, beforeTriggers := len(queuedNames), len(admittedNames), triggers
					out, err := tool.Execute(context.Background(), args)
					allowed := strings.TrimSpace(agentName) != "" && (mode == ModeYolo || agentName == "reviewer")
					if err != nil || out.IsError == allowed {
						t.Errorf("mode=%v agent=%q: output=%+v err=%v; allowed=%v", mode, agentName, out, err, allowed)
					}
					if allowed {
						if len(queuedNames) != beforeJobs+1 || len(admittedNames) != beforeAdmission+1 || triggers != beforeTriggers+1 {
							t.Errorf("yolo path did not reach admission, job creation and trigger: %q", agentName)
						} else if queuedNames[beforeJobs] != agentName || admittedNames[beforeAdmission] != agentName {
							t.Errorf("yolo path changed: admission=%q job=%q, want %q", admittedNames[beforeAdmission], queuedNames[beforeJobs], agentName)
						}
					} else if len(queuedNames) != beforeJobs || len(admittedNames) != beforeAdmission || triggers != beforeTriggers {
						t.Errorf("path reached admission or jobs outside yolo (or empty name): %q", agentName)
					}
				}
			}
		})
	}
}

func TestQueueAgentYoloPathStillChecksModelAdmission(t *testing.T) {
	approval := NewApprovalManager(NewToolPermissions())
	approval.SetApprovalMode(ModeYolo)
	transport := &rejectingAgentJobTransport{}
	client := &jobsBackedAgentClient{baseURL: "http://jobs.invalid", httpClient: &http.Client{Transport: transport}}
	tool := NewQueueAgentToolWithClient(client, approval)
	admitted := false
	tool.SetModelAdmission(nil, func(_ context.Context, name, _ string, _ modelpolicy.Policy, _ modelpolicy.ParentModel) error {
		admitted = true
		if name != "./term-llm-agents/reviewer" {
			t.Errorf("admission name = %q", name)
		}
		return errors.New("model policy denied")
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"agent_name":"./term-llm-agents/reviewer","prompt":"review"}`))
	if err != nil || !out.IsError || !strings.Contains(out.Content, "model policy denied") || !admitted || transport.calls != 0 {
		t.Fatalf("admission result: out=%+v err=%v admitted=%v requests=%d", out, err, admitted, transport.calls)
	}
}

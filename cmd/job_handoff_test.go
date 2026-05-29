package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestServeJobHandoffCreatesManualLLMRun(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	srv := &serveServer{
		jobsV2: mgr,
		cfg:    serveServerConfig{agentName: "jarvis"},
	}

	res, err := srv.jobHandoffFunc("sess_parent", "jarvis")(context.Background(), tools.JobHandoffRequest{
		Instructions:   "finish the long task",
		Name:           "long task",
		TimeoutSeconds: 30, // should be protected upward to 60s
	})
	if err != nil {
		t.Fatalf("handoff error: %v", err)
	}
	if res.Status != "started" || res.JobID == "" || res.RunID == "" || res.SessionID == "" {
		t.Fatalf("unexpected result: %+v", res)
	}

	job, err := mgr.GetJob(res.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.RunnerType != jobsV2RunnerLLM || job.TriggerType != jobsV2TriggerManual {
		t.Fatalf("unexpected job type: %+v", job)
	}
	if job.TimeoutSeconds != 60 {
		t.Fatalf("TimeoutSeconds = %d, want 60", job.TimeoutSeconds)
	}
	var cfg jobsV2LLMConfig
	if err := json.Unmarshal(job.RunnerConfig, &cfg); err != nil {
		t.Fatalf("runner config: %v", err)
	}
	if cfg.AgentName != "jarvis" || cfg.Instructions != "finish the long task" || cfg.ReplyToSessionID != "sess_parent" || cfg.SessionID != res.SessionID {
		t.Fatalf("unexpected runner config: %+v", cfg)
	}

	run, err := mgr.GetRun(res.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != jobsV2RunQueued {
		t.Fatalf("run status = %s, want queued", run.Status)
	}
}

func TestJobReplyHandlerAppendsCompletionToParentSession(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: t.TempDir() + "/sessions.db"})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	parent := &session.Session{ID: "sess_parent", Provider: "test", Model: "test", Mode: session.ModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := store.Create(ctx, parent); err != nil {
		t.Fatalf("Create: %v", err)
	}

	cfg, _ := json.Marshal(jobsV2LLMConfig{ReplyToSessionID: parent.ID})
	handler := makeJobReplyHandler(store)
	handler(jobsV2Run{ID: "run_123", Status: jobsV2RunSucceeded, SessionID: "sess_child", Response: "done from job"}, jobsV2Job{Name: "long task", RunnerType: jobsV2RunnerLLM, RunnerConfig: cfg})

	msgs, err := store.GetMessages(ctx, parent.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(msgs))
	}
	if msgs[0].Role != llm.RoleAssistant || !strings.Contains(msgs[0].TextContent, "done from job") || !strings.Contains(msgs[0].TextContent, "run_123") {
		t.Fatalf("unexpected message: role=%s text=%q", msgs[0].Role, msgs[0].TextContent)
	}
}

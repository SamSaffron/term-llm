package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestServeIsolatedSkillAtExhaustedParentBudget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		live      bool
		wantDepth int
	}{
		{"live nested parent", true, 4},
		{"offline subagent", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup, root := serveSkillTestSetup(t)
			store := newServeRuntimeTestStore()
			store.sessions["parent"] = &session.Session{ID: "parent", CWD: root, IsSubagent: true}
			child := &fakeServeSkillChildRunner{}
			srv := &serveServer{store: store, skillsSetup: setup, skillChildRunnerFactory: func(_ string, _ *serveRuntime) (runpkg.ChildRunner, error) { return child, nil }}
			if tc.live {
				mgr, err := tools.NewToolManager(&tools.ToolConfig{Enabled: []string{tools.SpawnAgentToolName}}, &config.Config{})
				if err != nil {
					t.Fatal(err)
				}
				spawn := mgr.GetSpawnAgentTool()
				spawn.SetDepth(3)
				spawn.SetRemainingDepth(0)
				rt := &serveRuntime{engine: llm.NewEngine(llm.NewMockProvider("mock"), nil), toolMgr: mgr, store: store}
				manager := newServeSessionManager(time.Minute, 4, func(context.Context) (*serveRuntime, error) { return rt, nil })
				t.Cleanup(manager.Close)
				if _, err := manager.GetOrCreate(context.Background(), "parent"); err != nil {
					t.Fatal(err)
				}
				srv.sessionMgr = manager
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/sessions/parent/skills/invoke", strings.NewReader(`{"name":"forked"}`))
			req.Header.Set("session_id", "parent")
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.handleSessionByID(rr, req)
			if rr.Code != http.StatusAccepted {
				t.Fatalf("skill child not started: status=%d body=%s", rr.Code, rr.Body.String())
			}
			deadline := time.Now().Add(time.Second)
			for child.requestSnapshot().RunID == "" && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			request := child.requestSnapshot()
			if request.Kind != runpkg.ChildRunIsolatedSkill || request.RemainingDepth == nil || *request.RemainingDepth != 0 || request.Depth != tc.wantDepth {
				t.Fatalf("child request depth/budget = %d/%v, want %d/0", request.Depth, request.RemainingDepth, tc.wantDepth)
			}
			// Exercise the child tool at the inherited cap, not only the request field.
			spawn := tools.NewSpawnAgentTool(tools.SpawnConfig{MaxDepth: 2}, request.Depth)
			spawn.SetRunner(&capturingSpawnRunner{})
			spawn.SetRemainingDepth(*request.RemainingDepth)
			out, err := spawn.Execute(context.Background(), json.RawMessage(`{"agent_name":"developer","prompt":"try spawning"}`))
			if err != nil || !strings.Contains(out.Content, "spawn depth budget exhausted") {
				t.Fatalf("skill child spawned at zero budget: %q, %v", out.Content, err)
			}
		})
	}
}

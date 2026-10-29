package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/modelpolicy"
)

type rejectingAgentJobTransport struct{ calls int }

func (r *rejectingAgentJobTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	return nil, fmt.Errorf("unexpected job request")
}

func TestQueueAgentRejectsFilesystemPathsBeforeAdmission(t *testing.T) {
	for _, name := range []string{"", "   ", "./term-llm-agents/reviewer", "/repo/term-llm-agents/reviewer", `C:\repo\reviewer`, `term-llm-agents\reviewer`, "../reviewer", ".", ".."} {
		t.Run(name, func(t *testing.T) {
			transport := &rejectingAgentJobTransport{}
			client := &jobsBackedAgentClient{baseURL: "http://jobs.invalid", httpClient: &http.Client{Transport: transport}}
			tool := NewQueueAgentToolWithClient(client, nil)
			admitted := false
			tool.SetModelAdmission(nil, func(context.Context, string, string, modelpolicy.Policy, modelpolicy.ParentModel) error {
				admitted = true
				return nil
			})
			args, err := json.Marshal(QueueAgentArgs{AgentName: name, Prompt: "review"})
			if err != nil {
				t.Fatal(err)
			}
			out, err := tool.Execute(context.Background(), args)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Error struct {
					Type ToolErrorType `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
				t.Fatal(err)
			}
			if !out.IsError || result.Error.Type != ErrInvalidParams || !strings.Contains(out.Content, "agent_name") {
				t.Errorf("path selection result = %+v, want invalid agent_name", out)
			}
			if admitted || transport.calls != 0 {
				t.Errorf("untrusted path reached admission=%v or job HTTP requests=%d", admitted, transport.calls)
			}
		})
	}
}

func TestInitiateHandoverRejectsFilesystemPathsBeforeUI(t *testing.T) {
	for _, name := range []string{"", "   ", "./term-llm-agents/reviewer", "@./term-llm-agents/reviewer", "/repo/reviewer", `C:\repo\reviewer`, `term-llm-agents\reviewer`, "../reviewer", ".", ".."} {
		t.Run(name, func(t *testing.T) {
			called := false
			ctx := ContextWithHandoverFunc(context.Background(), func(context.Context, string) (bool, error) {
				called = true
				return true, nil
			})
			args, err := json.Marshal(InitiateHandoverArgs{Agent: name})
			if err != nil {
				t.Fatal(err)
			}
			out, err := NewInitiateHandoverTool().Execute(ctx, args)
			if err != nil {
				t.Fatal(err)
			}
			var result InitiateHandoverResult
			if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
				t.Fatal(err)
			}
			if result.Status != "error" || !strings.Contains(result.Error, "agent") || called {
				t.Errorf("path selection result = %+v, UI called=%v", result, called)
			}
		})
	}
}

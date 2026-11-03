package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
)

func TestAuthorCanaryNeverReachesProvider(t *testing.T) {
	const canary = "AUTHOR_SECRET_CANARY_id_name_email@example.invalid"
	for _, compaction := range []bool{false, true} {
		t.Run(map[bool]string{false: "send_and_steering", true: "compaction"}[compaction], func(t *testing.T) {
			p := mockprovider.NewMockProvider("mock")
			if compaction {
				p.AddTextResponse("summary")
			}
			e := NewEngine(p, nil)
			messages := []core.Message{core.UserText("question"), core.AssistantText("answer"), core.UserText("follow up")}
			for i := range messages {
				messages[i].AuthorID = canary
			}
			if compaction {
				result, err := (CompactionEnv{}).Compact(context.Background(), p, "mock", "", messages, DefaultCompactionConfig())
				if err != nil {
					t.Fatal(err)
				}
				messages = result.ActiveMessages()
			}
			guidance := core.UserText("additional guidance")
			guidance.AuthorID = canary
			p.AddToolCall("call", "inject", map[string]any{}).AddTextResponse("steered answer")
			registry := NewToolRegistry()
			registry.Register(&steeringTool{name: "inject", fn: func() { e.QueueSteering(QueuedSteering{ID: "steer", Message: guidance}) }})
			e.tools = registry
			stream, err := e.Stream(context.Background(), core.Request{Messages: messages, Tools: []core.ToolSpec{{Name: "inject"}}, MaxTurns: 3})
			if err != nil {
				t.Fatal(err)
			}
			drainStream(t, stream)
			_ = stream.Close()
			requests := p.RecordedRequests()
			if len(requests) == 0 {
				t.Fatal("no provider requests")
			}
			sawSteering := false
			for _, req := range requests {
				for _, m := range req.Messages {
					if strings.Contains(core.MessageText(m), "additional guidance") {
						sawSteering = true
					}
				}
			}
			if !sawSteering {
				t.Fatal("test never delivered steering to the provider")
			}
			for _, req := range requests {
				b, err := json.Marshal(req)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(b), canary) {
					t.Fatalf("identity leaked: %s", b)
				}
			}
			if !compaction && messages[0].AuthorID != canary {
				t.Fatal("history metadata mutated")
			}
		})
	}
}

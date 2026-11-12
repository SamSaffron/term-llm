package engine

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
)

func TestProviderConversationExcludesTransientRosterAcrossTurns(t *testing.T) {
	e := newCodeEngine()
	t.Cleanup(e.CloseCodeMode)
	ctx := asyncCodeContext("stable-roster", "spawn")
	cell, err := e.ExecCodeMode(ctx, `return 'receipt'`, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	awaitCodeModeTerminal(t, e, ctx, cell)
	firstHistory := []core.Message{core.SystemText("policy"), core.UserText("first review")}
	first := e.prepareProviderRequest(core.Request{SessionID: "stable-roster", Messages: firstHistory})
	if first.ProviderConversation == nil || len(first.ProviderConversation.TurnContext) != 1 {
		t.Fatalf("missing typed roster context: %+v", first.ProviderConversation)
	}
	if !reflect.DeepEqual(first.ProviderConversation.Messages, firstHistory) {
		t.Fatalf("stable first transcript = %+v", first.ProviderConversation.Messages)
	}
	if !strings.Contains(core.MessageText(first.Messages[1]), "<live_code_cells>") {
		t.Fatalf("normal provider request lost the roster: %+v", first.Messages)
	}

	continuedHistory := append(append([]core.Message(nil), firstHistory...), core.AssistantText("old findings"), core.UserText("re-review"))
	continued := e.prepareProviderRequest(core.Request{SessionID: "stable-roster", Messages: continuedHistory})
	if !reflect.DeepEqual(continued.ProviderConversation.Messages[:len(firstHistory)], first.ProviderConversation.Messages) {
		t.Fatal("moving roster changed stable historical identity")
	}
	if !strings.Contains(core.MessageText(continued.Messages[3]), "<live_code_cells>") {
		t.Fatal("current roster was not moved to the follow-up policy position")
	}
	// A rebuilt child restores native provider state, not the old engine's cells.
	rebuilt := NewEngine(mockprovider.NewMockProvider("mock"), nil)
	afterReload := rebuilt.prepareProviderRequest(core.Request{SessionID: "stable-roster", Messages: continuedHistory})
	if len(afterReload.ProviderConversation.TurnContext) != 0 {
		t.Fatal("rebuilt engine unexpectedly retained runtime receipts")
	}
	if !reflect.DeepEqual(afterReload.ProviderConversation.Messages, continued.ProviderConversation.Messages) {
		t.Fatal("receipt disappearance changed durable conversation identity")
	}
	if len(firstHistory) != 2 || core.MessageText(firstHistory[1]) != "first review" {
		t.Fatal("preparing requests mutated historical input")
	}
}

func TestProviderConversationUsesProviderSafeTransforms(t *testing.T) {
	for _, indirect := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "indirect"}[indirect], func(t *testing.T) {
			e := NewEngine(mockprovider.NewMockProvider("mock"), nil)
			e.SetIndirectVision(indirect)
			original := []core.Message{{Role: core.RoleUser, AuthorID: "private-author", Parts: []core.Part{
				{Type: core.PartDisplayText, Text: "private display text"},
				{Type: core.PartText, Text: "visible question"},
				{Type: core.PartAgentMention, Text: "delegate to the named agent"},
				{Type: core.PartImage, ImagePath: "missing-probe-image.png"},
			}}}
			prepared := e.prepareProviderRequest(core.Request{Messages: original})
			if prepared.ProviderConversation == nil {
				t.Fatal("missing stable request metadata")
			}
			if !reflect.DeepEqual(prepared.ProviderConversation.Messages, prepared.Messages) {
				t.Fatal("stable history did not receive the same safety/vision transformations")
			}
			for _, message := range prepared.ProviderConversation.Messages {
				if message.AuthorID != "" || strings.Contains(core.MessageText(message), "private display text") {
					t.Fatal("stable provider metadata contains display-only data")
				}
				for _, part := range message.Parts {
					if indirect && part.Type == core.PartImage {
						t.Fatal("stable history bypassed indirect vision rewriting")
					}
				}
			}
			if original[0].AuthorID != "private-author" || original[0].Parts[0].Type != core.PartDisplayText {
				t.Fatal("provider projection mutated source history")
			}
			wire, err := json.Marshal(prepared)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(wire), "ProviderConversation") {
				t.Fatal("internal continuation metadata escaped into request JSON")
			}
		})
	}
}

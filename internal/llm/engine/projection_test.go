package engine

import (
	"github.com/samsaffron/term-llm/internal/llm/core"
	"testing"
)

func TestProviderProjectionStripsDisplayMetadata(t *testing.T) {
	message := core.UserText("provider context")
	message.Parts = append(message.Parts, core.Part{Type: core.PartDisplayText, Text: "visible request"})
	got := providerSafeRequestMessages([]core.Message{message})
	if len(got) != 1 || len(got[0].Parts) != 1 || core.MessageText(got[0]) != "provider context" {
		t.Fatalf("provider messages = %#v", got)
	}
	if len(message.Parts) != 2 {
		t.Fatal("source metadata mutated")
	}
}

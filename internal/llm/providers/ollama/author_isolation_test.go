package ollama

import (
	"bytes"
	"encoding/json"
	"github.com/term-llm/term-llm/internal/llm/core"
	"testing"
)

func TestAuthorRequestBuilderByteIdentical(t *testing.T) {
	messages := []core.Message{core.UserText("hello"), core.AssistantText("world"), core.UserText("next")}
	encode := func() []byte {
		wire := buildOllamaMessages(messages)
		b, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	plain := encode()
	for i := range messages {
		messages[i].AuthorID = "AUTHOR_CANARY"
	}
	attributed := encode()
	if !bytes.Equal(plain, attributed) {
		t.Fatalf("author changed wire request: %s vs %s", plain, attributed)
	}
}

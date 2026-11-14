package clibin

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
)

func TestAuthorCLIRequestBuildersAndAgyHashUnchanged(t *testing.T) {
	messages := []core.Message{core.SystemText("system"), core.UserText("question"), core.AssistantText("answer"), core.UserText("next")}
	plainHash, err := agyTranscriptHash(messages)
	if err != nil {
		t.Fatal(err)
	}
	agy := buildAgyPrompt(messages, "/workspace")
	cursor := buildCursorPrompt(messages)
	grok, err := buildGrokACPPrompt(messages)
	if err != nil {
		t.Fatal(err)
	}
	for i := range messages {
		messages[i].AuthorID = "AUTHOR_CANARY"
	}
	attributedHash, err := agyTranscriptHash(messages)
	if err != nil {
		t.Fatal(err)
	}
	if attributedHash != plainHash || buildAgyPrompt(messages, "/workspace") != agy || buildCursorPrompt(messages) != cursor {
		t.Fatal("attribution changed CLI prompt or transcript hash")
	}
	attributedGrok, err := buildGrokACPPrompt(messages)
	if err != nil || !bytes.Equal(grok, attributedGrok) {
		t.Fatalf("attribution changed Grok request: %v", err)
	}
	if messages[0].AuthorID == "" {
		t.Fatal("hashing mutated original history")
	}
}

func TestAgyAuthorHashPreservesEmptyTranscriptRepresentation(t *testing.T) {
	for _, messages := range [][]core.Message{nil, {}} {
		encoded, err := json.Marshal(messages)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("%x", sha256.Sum256(encoded))
		got, err := agyTranscriptHash(messages)
		if err != nil || got != want {
			t.Fatalf("hash changed for empty transcript %s: %s %v", encoded, got, err)
		}
	}
}

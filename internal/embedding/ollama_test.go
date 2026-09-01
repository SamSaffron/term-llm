package embedding

import (
	"reflect"
	"testing"
)

func TestPrepareQwen3EmbeddingTextsAddsRetrievalInstruction(t *testing.T) {
	input := []string{"Where is the deployment note?"}
	got := prepareQwen3EmbeddingTexts("qwen3-embedding:4b", "RETRIEVAL_QUERY", input)
	want := []string{"Instruct: Given a user question, retrieve relevant passages that answer it\nQuery: Where is the deployment note?"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prepared texts = %#v, want %#v", got, want)
	}
	if input[0] != "Where is the deployment note?" {
		t.Fatalf("input mutated: %#v", input)
	}
}

func TestPrepareQwen3EmbeddingTextsLeavesDocumentsAndOtherModelsAlone(t *testing.T) {
	input := []string{"document"}
	tests := []struct {
		model    string
		taskType string
	}{
		{model: "qwen3-embedding:4b", taskType: "RETRIEVAL_DOCUMENT"},
		{model: "nomic-embed-text", taskType: "RETRIEVAL_QUERY"},
		{model: "qwen3-embedding:4b", taskType: ""},
	}
	for _, tt := range tests {
		if got := prepareQwen3EmbeddingTexts(tt.model, tt.taskType, input); !reflect.DeepEqual(got, input) {
			t.Fatalf("prepareQwen3EmbeddingTexts(%q, %q) = %#v", tt.model, tt.taskType, got)
		}
	}
}

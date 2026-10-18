package llm

import (
	"context"
	"strings"
	"testing"
)

// Engine-level behaviour is covered in internal/llm/engine; this verifies the
// root wrapper runs with the root-installed engine environment.
func TestGeneratePathNotesWrapperUsesRootEnvironment(t *testing.T) {
	provider := NewMockProvider("mock").AddTextResponse("- Finding.")
	result, err := GeneratePathNotes(context.Background(), provider, "mock-model", []Message{UserText("old context")}, PathNotesConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Notes, "Finding") || len(provider.Requests) != 1 {
		t.Fatalf("result=%#v requests=%d", result, len(provider.Requests))
	}
}

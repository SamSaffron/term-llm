package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
)

// Engine-level behaviour is covered in internal/llm/engine; this verifies the
// root wrapper runs with the root-installed engine environment.
func TestGeneratePathNotesWrapperUsesRootEnvironment(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock").AddTextResponse("- Finding.")
	result, err := engine.GeneratePathNotes(context.Background(), provider, "mock-model", []core.Message{core.UserText("old context")}, engine.PathNotesConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Notes, "Finding") || len(provider.Requests) != 1 {
		t.Fatalf("result=%#v requests=%d", result, len(provider.Requests))
	}
}

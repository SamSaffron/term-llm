package memory

import (
	"context"
	"encoding/json"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMemoryPolicyContentGuardFailClosed(t *testing.T) {
	requireMemoryPolicyShell(t)
	dir := t.TempDir()
	guard := filepath.Join(dir, "guard")
	if err := os.WriteFile(guard, []byte("#!/bin/sh\necho synthetic-secret >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_LLM_MEMORY_CONTENT_GUARD", guard)
	err := runMemoryContentGuard(context.Background(), "fragment", "fragments/notes/test.md", "safe")
	if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatalf("unexpected guard result %v", err)
	}
	args, _ := json.Marshal(map[string]string{"path": "fragments/notes/test.md", "content": "safe"})
	// Nil store proves create and update reject before attempting a mutation.
	if _, err := (&memoryCreateFragmentTool{}).Execute(context.Background(), args); err == nil {
		t.Fatal("create bypassed guard")
	}
	if _, err := (&memoryUpdateFragmentTool{}).Execute(context.Background(), args); err == nil {
		t.Fatal("update bypassed guard")
	}
	t.Setenv("TERM_LLM_MEMORY_CONTENT_GUARD", filepath.Join(dir, "missing"))
	if runMemoryContentGuard(context.Background(), "fragment", "x", "x") == nil {
		t.Fatal("missing guard must fail closed")
	}
}
func TestMemoryPolicyContentGuardOptional(t *testing.T) {
	t.Setenv("TERM_LLM_MEMORY_CONTENT_GUARD", "")
	if err := runMemoryContentGuard(context.Background(), "fragment", "x", "x"); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryPolicySanitizeInput(t *testing.T) {
	requireMemoryPolicyShell(t)
	path := filepath.Join(t.TempDir(), "guard")
	t.Setenv(memoryGuardEnv, path)
	for _, script := range []string{"#!/bin/sh\necho private-marker >&2\nexit 1\n", "#!/bin/sh\nprintf invalid-json\n", "#!/bin/sh\nprintf '{\"content\":null}'\n"} {
		if err := os.WriteFile(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		text, err := sanitizeMemoryInput(context.Background(), "private-marker")
		if err == nil || text != "" || strings.Contains(err.Error(), "private-marker") {
			t.Fatal("sanitizer did not fail closed")
		}
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '{\"content\":\"redacted\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	text, err := sanitizeMemoryInput(context.Background(), "private-marker")
	if err != nil || text != "redacted" {
		t.Fatal("sanitizer output not used")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sanitizeMemoryInput(ctx, "private-marker"); err == nil {
		t.Fatal("cancelled policy accepted")
	}
	// Exercise os/exec's io.Copy path, not just the writer directly.
	if err := os.WriteFile(path, []byte("#!/bin/sh\ndd if=/dev/zero bs=1048576 count=5 2>/dev/null\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := runMemoryContentGuard(context.Background(), "fragment", "x", "x"); err == nil {
		t.Fatal("oversize child stdout accepted")
	}
	var out memoryPolicyOutput
	if _, err := out.Write(make([]byte, (4<<20)+1)); err == nil {
		t.Fatal("unbounded output accepted")
	}
}

func TestMemoryPolicyToolRejectionSurvivesModelDone(t *testing.T) {
	requireMemoryPolicyShell(t)
	ctx := context.Background()
	guard := filepath.Join(t.TempDir(), "guard")
	script := `#!/bin/sh
read -r payload
case "$payload" in *'"mode":"input"'*) printf '{"content":"sanitized input"}';; *) exit 1;; esac
`
	if err := os.WriteFile(guard, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(memoryGuardEnv, guard)
	store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider := mockprovider.NewMockProvider("policy").AddToolCall("write", "memory_create_fragment", map[string]string{"path": "fragments/notes/test.md", "content": "rejected fact"}).AddTextResponse("done")
	eng := engine.NewEngine(provider, engine.NewToolRegistry())
	opts := MineOptions{ToolMaxTurns: 3}
	if _, err := opts.runExtractionRequest(ctx, eng, store, "assistant", "original prompt", &memoryExtractionStats{}); err == nil {
		t.Fatal("rejected tool batch marked successful")
	}
	frag, err := store.GetFragment(ctx, "assistant", "fragments/notes/test.md")
	if err != nil || frag != nil {
		t.Fatal("rejected fragment persisted")
	}
	if len(provider.RecordedRequests()) != 2 {
		t.Fatal("expected model to continue after rejected tool")
	}
}

func requireMemoryPolicyShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX policy fixture")
	}
}

package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	memorydb "github.com/samsaffron/term-llm/internal/memory"
)

func TestMemoryPolicyLookupSanitizesBeforeClipping(t *testing.T) {
	requireMemoryPolicyShell(t)
	ctx := context.Background()
	guard := filepath.Join(t.TempDir(), "guard")
	// A deliberately context-sensitive policy: a clipped prefix is not detected.
	script := `#!/bin/sh
read -r payload
case "$payload" in *'BEGIN_PRIVATE/SECRET_END'*) printf '{"content":"redacted safe source"}';; *) printf '%s' "$payload";; esac
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
	path := "fragments/notes/test.md"
	if err := store.CreateFragment(ctx, &memorydb.Fragment{Agent: "assistant", Path: path, Content: "BEGIN_PRIVATE/SECRET_END needle fact"}); err != nil {
		t.Fatal(err)
	}
	get := &memoryGetFragmentTool{store: store, agent: "assistant", readBytes: 10}
	search := &memorySearchFragmentsTool{store: store, agent: "assistant", readBytes: 10}
	for _, tc := range []struct {
		name string
		run  func() (string, error)
	}{
		{"get", func() (string, error) {
			out, err := get.Execute(ctx, json.RawMessage(`{"path":"fragments/notes/test.md"}`))
			return out.Content, err
		}},
		{"search", func() (string, error) {
			out, err := search.Execute(ctx, json.RawMessage(`{"query":"needle"}`))
			return out.Content, err
		}},
		{"taxonomy", func() (string, error) {
			return buildGuardedTaxonomyMap(ctx, []memorydb.Fragment{{Path: "fragments/BEGIN_PRIVATE/SECRET_END"}}, 500)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.run()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, "BEGIN") || strings.Contains(out, "PRIVATE") || !strings.Contains(out, "redacted") {
				t.Fatalf("complete source was not sanitized before clipping: %q", out)
			}
		})
	}
	if err := os.WriteFile(guard, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	collector := newMemoryExtractionCollector()
	wrapped := &memoryPolicyTool{Tool: get, collector: collector}
	if _, err := wrapped.Execute(ctx, json.RawMessage(`{"path":"fragments/notes/test.md"}`)); err == nil || !collector.policyFailed() {
		t.Fatal("failed lookup policy did not latch")
	}
}

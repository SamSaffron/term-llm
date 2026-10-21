package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This digest is captured from the pre-intent grep ToolSpec at fa7aed75.
// JSON's sorted map keys make this an exact schema AND description contract.
func TestGrepUnconfiguredSpecBaseline(t *testing.T) {
	data, err := json.Marshal(NewGrepTool(nil, DefaultOutputLimits()).Spec())
	if err != nil {
		t.Fatal(err)
	}
	const want = "a0c05a6290af929ff8f78241d475f396cbbdc98afdb825deec2ab592a8982d57"
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want {
		t.Fatalf("grep spec digest = %s; want %s\n%s", got, want, data)
	}
}

func TestGrepUnconfiguredIntentBaseline(t *testing.T) {
	// Force the deterministic Go path; ripgrep equivalence is tested separately.
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(path, []byte("before\nneedle\nafter\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tool := NewGrepTool(nil, DefaultOutputLimits())
	const body = "1 match in 1 file\n\nsample.txt (1 match):\n  1: before\n> 2: needle\n  3: after\n"
	for _, intent := range []string{"", `,"intent":"find writes"`, `,"intent":42`, `,"intent":{"x":true}`} {
		raw := json.RawMessage(fmt.Sprintf(`{"pattern":"needle","path":%q,"context_lines":1%s}`, path, intent))
		got, err := tool.Execute(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		want := body
		if intent != "" {
			want = "Unknown parameter 'intent' was ignored\n" + body
		}
		if text := strings.ReplaceAll(got.Content, path, "sample.txt"); text != want {
			t.Fatalf("intent %s: got %q; want %q", intent, text, want)
		}
	}
}

func TestGrepUnconfiguredIntentRipgrepBaseline(t *testing.T) {
	rg := installFakeRG(t, buildSyntheticRipgrepJSON(4))
	tool := NewGrepTool(nil, DefaultOutputLimits())
	args := map[string]any{"pattern": "needle", "path": t.TempDir(), "max_results": 7, "context_lines": 1}
	plain := execGrepRaw(t, context.Background(), tool, args)
	plainArgv := strings.Join(rg.argv(t), "\x00")
	for _, intent := range []any{"find writes", 42, map[string]any{"x": true}} {
		args["intent"] = intent
		got := execGrepRaw(t, context.Background(), tool, args)
		if want := "Unknown parameter 'intent' was ignored\n" + plain; got != want {
			t.Fatalf("intent %v: got %q, want %q", intent, got, want)
		}
		if gotArgv := strings.Join(rg.argv(t), "\x00"); gotArgv != plainArgv {
			t.Fatalf("intent %v changed rg args: %q != %q", intent, gotArgv, plainArgv)
		}
	}
}

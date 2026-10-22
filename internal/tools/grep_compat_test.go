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

// TestGrepUnconfiguredRipgrepGolden compares the current tool with goldens
// captured from the real GrepTool.Execute at fa7aed75 (rg argv and output
// bytes) for a fixed fake rg JSON stream. See testdata/grep/regenerate.sh for
// provenance; the only normalisation is the fixture root, {{ROOT}}.
func TestGrepUnconfiguredRipgrepGolden(t *testing.T) {
	const dir = "testdata/grep"
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var cases []struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(read("cases.json"), &cases); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stream := strings.ReplaceAll(string(read("rg_stream.jsonl.tmpl")), "{{ROOT}}", root)
	rg := installFakeRG(t, []byte(stream))
	tool := NewGrepTool(nil, DefaultOutputLimits())
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			in := strings.ReplaceAll(string(c.Args), "{{ROOT}}", root)
			out, err := tool.Execute(context.Background(), json.RawMessage(in))
			if err != nil {
				t.Fatal(err)
			}
			gotArgs := strings.ReplaceAll(strings.Join(rg.argv(t), "\n")+"\n", root, "{{ROOT}}")
			if want := string(read(c.Name + ".args")); gotArgs != want {
				t.Fatalf("rg args:\n%s\nwant:\n%s", gotArgs, want)
			}
			gotOut := strings.ReplaceAll(out.Content, root, "{{ROOT}}")
			if want := string(read(c.Name + ".out")); gotOut != want {
				t.Fatalf("output:\n%s\nwant:\n%s", gotOut, want)
			}
		})
	}
}

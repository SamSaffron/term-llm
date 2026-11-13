package shellcmd

import (
	"path/filepath"
	"testing"
)

func TestExtractLeadingCdPreservesConservativeOperandRules(t *testing.T) {
	base := filepath.Clean(t.TempDir())
	for _, tc := range []struct {
		command string
		want    string
		dir     string
	}{
		{`cd child && echo ready`, "echo ready", filepath.Join(base, "child")},
		{`cd 'two words' && echo ready`, "echo ready", filepath.Join(base, "two words")},
		{`cd "two words" && echo ready`, "echo ready", filepath.Join(base, "two words")},
		{`cd child; echo ready`, `cd child; echo ready`, base},
		{`cd "$TARGET" && echo ready`, `cd "$TARGET" && echo ready`, base},
		{`cd "~/child" && echo ready`, `cd "~/child" && echo ready`, base},
		{`cd - && echo ready`, `cd - && echo ready`, base},
		{`cd child &&`, `cd child &&`, base},
		{`cd 'unterminated && echo ready`, `cd 'unterminated && echo ready`, base},
	} {
		command, dir := ExtractLeadingCd(tc.command, base)
		if command != tc.want || dir != tc.dir {
			t.Errorf("rewrite %q = %q in %q, want %q in %q", tc.command, command, dir, tc.want, tc.dir)
		}
	}
}

func TestExtractLeadingCdExpandsOnlyUnquotedHome(t *testing.T) {
	home := filepath.Clean(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	command, dir := ExtractLeadingCd(`cd ~/child && echo ready`, "")
	if command != "echo ready" || dir != filepath.Join(home, "child") {
		t.Fatalf("home rewrite = %q in %q", command, dir)
	}
	if got, ok := ExpandTilde("plain"); !ok || got != "plain" {
		t.Fatalf("literal tilde expansion = %q, %v", got, ok)
	}
}

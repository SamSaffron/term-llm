package sessions

import (
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestGistPreviewURL(t *testing.T) {
	if got, want := session.GistPreviewURL("abcdef123456"), "https://gisthost.github.io/?abcdef123456/index.html"; got != want {
		t.Fatalf("GistPreviewURL = %q, want %q", got, want)
	}
	for _, invalid := range []string{"", "not/valid", "abc?x=1", "ABC"} {
		if got := session.GistPreviewURL(invalid); got != "" {
			t.Errorf("session.GistPreviewURL(%q) = %q, want empty", invalid, got)
		}
	}
}

func TestBuildSessionGistFiles(t *testing.T) {
	sess := &session.Session{ID: "abcdef", Name: "Export", Provider: "test", Model: "model", CreatedAt: time.Now()}
	messages := []session.Message{{Role: llm.RoleUser, TextContent: "hello", Parts: []llm.Part{{Type: llm.PartText, Text: "hello"}}}}
	files, err := session.GistFiles(sess, messages, session.ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
	if !strings.Contains(files["session.md"], "hello") {
		t.Error("session.md missing transcript")
	}
	if !strings.Contains(files["index.html"], "<!doctype html>") || !strings.Contains(files["index.html"], "hello") {
		t.Error("index.html missing self-contained transcript")
	}
}

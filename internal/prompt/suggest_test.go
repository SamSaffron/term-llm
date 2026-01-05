package prompt

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/input"
)

func TestSuggestUserPrompt(t *testing.T) {
	t.Run("basic prompt without files", func(t *testing.T) {
		result := SuggestUserPrompt("list files", nil, "")
		expected := "I want to: list files"
		if result != expected {
			t.Errorf("expected: %s\ngot: %s", expected, result)
		}
	})

	t.Run("prompt with single file", func(t *testing.T) {
		files := []input.FileContent{
			{Path: "test.go", Content: "package main"},
		}
		result := SuggestUserPrompt("explain this", files, "")
		if !strings.Contains(result, "<<<<< FILE: test.go >>>>>") {
			t.Error("missing file marker")
		}
		if !strings.Contains(result, "package main") {
			t.Error("missing file content")
		}
		if !strings.Contains(result, "I want to: explain this") {
			t.Error("missing user request")
		}
	})

	t.Run("prompt with multiple files", func(t *testing.T) {
		files := []input.FileContent{
			{Path: "a.go", Content: "aaa"},
			{Path: "b.go", Content: "bbb"},
		}
		result := SuggestUserPrompt("compare", files, "")
		if !strings.Contains(result, "<<<<< FILE: a.go >>>>>") {
			t.Error("missing first file marker")
		}
		if !strings.Contains(result, "<<<<< FILE: b.go >>>>>") {
			t.Error("missing second file marker")
		}
	})

	t.Run("prompt with stdin", func(t *testing.T) {
		result := SuggestUserPrompt("analyze", nil, "piped data")
		if !strings.Contains(result, "<<<<< STDIN >>>>>") {
			t.Error("missing stdin marker")
		}
		if !strings.Contains(result, "piped data") {
			t.Error("missing stdin content")
		}
		if !strings.Contains(result, "I want to: analyze") {
			t.Error("missing user request")
		}
	})

	t.Run("prompt with files and stdin", func(t *testing.T) {
		files := []input.FileContent{
			{Path: "code.go", Content: "code"},
		}
		result := SuggestUserPrompt("review", files, "context")
		if !strings.Contains(result, "<<<<< FILE: code.go >>>>>") {
			t.Error("missing file marker")
		}
		if !strings.Contains(result, "<<<<< STDIN >>>>>") {
			t.Error("missing stdin marker")
		}
		if !strings.Contains(result, "I want to: review") {
			t.Error("missing user request")
		}
	})
}

func TestSuggestSystemPrompt(t *testing.T) {
	t.Run("basic system prompt", func(t *testing.T) {
		result := SuggestSystemPrompt("bash", "", 3, false)
		if !strings.Contains(result, "CLI command expert") {
			t.Error("missing command expert reference")
		}
		if !strings.Contains(result, "Shell: bash") {
			t.Error("missing shell info")
		}
		if !strings.Contains(result, "Suggest exactly 3") {
			t.Error("missing suggestion count")
		}
	})

	t.Run("with instructions", func(t *testing.T) {
		result := SuggestSystemPrompt("zsh", "prefer modern tools", 5, false)
		if !strings.Contains(result, "prefer modern tools") {
			t.Error("missing instructions")
		}
	})

	t.Run("with search enabled", func(t *testing.T) {
		result := SuggestSystemPrompt("bash", "", 3, true)
		if !strings.Contains(result, "web search") {
			t.Error("missing web search reference")
		}
	})
}

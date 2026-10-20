package cmd

import (
	"io"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/app/ask"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/spf13/cobra"
)

func askInputFromCommand(cmd *cobra.Command) (io.Reader, bool) {
	r := cmd.InOrStdin()
	return r, ask.ReaderHasStdin(r)
}
func TestPrepareAskInputUsesCobraInputAndKeepsSmallTextInline(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader("small text"))
	reader, hasStdin := askInputFromCommand(cmd)
	if !hasStdin {
		t.Fatal("SetIn reader was not recognized as piped stdin")
	}

	got, err := ask.PrepareInput("question", nil, reader, hasStdin, config.AskConfig{StdinInlineMaxBytes: 10, StdinMaxBytes: 1024})
	if err != nil {
		t.Fatalf("PrepareInput: %v", err)
	}
	if len(got.StagedPaths) != 0 {
		t.Fatalf("staged paths = %v, want none", got.StagedPaths)
	}
	want := prompt.AskUserPrompt("question", nil, "small text")
	if len(got.Message.Parts) != 1 || got.Message.Parts[0].Type != core.PartText || got.Message.Parts[0].Text != want {
		t.Fatalf("message = %#v, want exact legacy prompt %q", got.Message, want)
	}
}

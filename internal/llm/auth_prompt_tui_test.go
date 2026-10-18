package llm

import (
	"strings"
	"testing"

	rp "github.com/samsaffron/term-llm/internal/llm/providers/responsesproviders"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

func TestAuthPromptsRefuseActiveTUI(t *testing.T) {
	closeLog, err := runtimeoutput.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	for _, tc := range []struct {
		name    string
		prompt  func() error
		command string
	}{
		{"ChatGPT", func() error { _, err := rp.PromptForChatGPTAuth(); return err }, "auth login chatgpt"},
		{"Grok", func() error { _, err := rp.PromptForGrokAuth(); return err }, "auth login grok"},
		{"Copilot", func() error { _, err := rp.PromptForCopilotAuth(); return err }, "auth login copilot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.prompt()
			if err == nil || !strings.Contains(err.Error(), tc.command) {
				t.Fatalf("prompt error = %v; want actionable auth command", err)
			}
		})
	}
}

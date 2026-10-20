package runtime

import "testing"

func TestExtractModelFromProviderName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Claude CLI (sonnet)", "sonnet"},
		{"Claude CLI (sonnet, effort=high)", "sonnet"},
		{"Grok CLI (grok-4.5, effort=xhigh)", "grok-4.5"},
		{"OpenAI (gpt-5)", "gpt-5"},
		{"OpenAI (gpt-5, effort=high)", "gpt-5"},
		{"Anthropic (claude-sonnet-4, thinking=8k)", "claude-sonnet-4"},
		{"Bedrock (claude-sonnet-4, adaptive, us-west-2)", "claude-sonnet-4"},
		{"Gemini (gemini-2.5-pro, thinking=high)", "gemini-2.5-pro"},
		{"xAI (grok-4-1-fast)", "grok-4-1-fast"},
		{"agy CLI", ""},
		{"agy CLI (gemini-3.6-flash-high)", "gemini-3.6-flash-high"},
		{"debug", "debug"},
		{"debug:slow", "debug:slow"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := ExtractModelFromProviderName(tc.in); got != tc.want {
				t.Fatalf("extractModelFromProviderName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

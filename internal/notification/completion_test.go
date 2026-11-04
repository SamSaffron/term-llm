package notification

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCompletionText(t *testing.T) {
	for _, tc := range []struct {
		name             string
		label, chatTitle string
		failed           bool
		wantTitle        string
		wantBody         string
	}{
		{"names the chat", "", "Fix flaky CI", false, "Fix flaky CI", "Response ready"},
		{"labels the node", "Dev", "Fix flaky CI", false, "Dev: Fix flaky CI", "Response ready"},
		{"failure", "Dev", "Fix flaky CI", true, "Dev: Fix flaky CI", "Response failed"},
		{"no chat title keeps the outcome in the title", "Dev", "  ", false, "Dev: Response ready", ""},
		{"no chat title or label", "", "", true, "Response failed", ""},
		{"collapses whitespace and trims the label", " Dev ", "Fix\n flaky   CI ", false, "Dev: Fix flaky CI", "Response ready"},
		{"keeps a title at the limit", "", strings.Repeat("x", 60), false, strings.Repeat("x", 60), "Response ready"},
		{"shortens a first prompt at a word", "",
			"please look at why the integration tests keep timing out on the main branch and fix them", false,
			"please look at why the integration tests keep timing out on…", "Response ready"},
		{"shortens unspaced text by rune", "", strings.Repeat("节点", 40), false, strings.Repeat("节点", 30) + "…", "Response ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			title, body := CompletionText(tc.label, tc.chatTitle, tc.failed)
			if title != tc.wantTitle || body != tc.wantBody {
				t.Errorf("CompletionText = %q / %q, want %q / %q", title, body, tc.wantTitle, tc.wantBody)
			}
			if !utf8.ValidString(title) {
				t.Errorf("title is not valid UTF-8: %q", title)
			}
		})
	}
}

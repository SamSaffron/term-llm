package reasoning

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseReasoningSummaryExtractsTitleAndBody(t *testing.T) {
	got := ParseReasoningSummary("  **Inspecting repository structure**\n\nI am checking files.  ")
	if got.Title != "Inspecting repository structure" {
		t.Fatalf("Title = %q", got.Title)
	}
	if got.Body != "I am checking files." {
		t.Fatalf("Body = %q", got.Body)
	}
}

func TestParseReasoningSummaryExtractsStreamingTitle(t *testing.T) {
	got := ParseReasoningSummary("**Planning implementation**")
	if got.Title != "Planning implementation" || got.Body != "" {
		t.Fatalf("Parse = %+v", got)
	}
}

func TestParseReasoningSummaryPreservesIndentedBody(t *testing.T) {
	got := ParseReasoningSummary("**Checking code**\n\n    fmt.Println(\"hi\")\n  still indented")
	want := "    fmt.Println(\"hi\")\n  still indented"
	if got.Body != want {
		t.Fatalf("Body = %q, want %q", got.Body, want)
	}
}

func TestParseReasoningSummaryDoesNotConsumeInlineBoldPrefix(t *testing.T) {
	input := "**Important:** keep this"
	got := ParseReasoningSummary(input)
	if got.Title != "" {
		t.Fatalf("Title = %q, want empty", got.Title)
	}
	if got.Body != input {
		t.Fatalf("Body = %q, want %q", got.Body, input)
	}
}

func TestParseReasoningSummaryHandlesCRLF(t *testing.T) {
	got := ParseReasoningSummary("**Checking tests**\r\n\r\nBody\r\n")
	if got.Title != "Checking tests" || got.Body != "Body" {
		t.Fatalf("Parse = %+v", got)
	}
}

func TestParseReasoningSummaryNoTitle(t *testing.T) {
	input := "I checked the code without a title."
	got := ParseReasoningSummary(input)
	if got.Title != "" || got.Body != input {
		t.Fatalf("Parse = %+v", got)
	}
}

func TestParseReasoningSummaryOverlongTitle(t *testing.T) {
	long := strings.Repeat("a", MaxReasoningTitleRunes+20)
	got := ParseReasoningSummary("**" + long + "**\n\nBody")
	if utf8.RuneCountInString(got.Title) != MaxReasoningTitleRunes {
		t.Fatalf("title rune count = %d, want %d", utf8.RuneCountInString(got.Title), MaxReasoningTitleRunes)
	}
	if !strings.HasSuffix(got.Title, "…") {
		t.Fatalf("expected ellipsis in %q", got.Title)
	}
}

package sessiontitle

import (
	"context"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestParseCandidate(t *testing.T) {
	raw := "```json\n{\"short_title\":\"Fix cross-session bleed\",\"long_title\":\"Repairing web UI session switching and resume correctness\",\"confidence\":0.82}\n```"
	cand, err := ParseCandidate(raw)
	if err != nil {
		t.Fatalf("ParseCandidate: %v", err)
	}
	if cand.ShortTitle != "Fix cross-session bleed" {
		t.Fatalf("ShortTitle = %q", cand.ShortTitle)
	}
	if cand.LongTitle != "Repairing web UI session switching and resume correctness" {
		t.Fatalf("LongTitle = %q", cand.LongTitle)
	}
	if !Acceptable(cand) {
		t.Fatal("candidate should be acceptable")
	}
}

func TestParseCandidateNullTitles(t *testing.T) {
	raw := `{"short_title":null,"long_title":null,"confidence":0}`
	cand, err := ParseCandidate(raw)
	if err != nil {
		t.Fatalf("ParseCandidate: %v", err)
	}
	if cand.ShortTitle != "" {
		t.Fatalf("ShortTitle should be empty, got %q", cand.ShortTitle)
	}
	if cand.LongTitle != "" {
		t.Fatalf("LongTitle should be empty, got %q", cand.LongTitle)
	}
	if Acceptable(cand) {
		t.Fatal("null-title candidate should not be acceptable")
	}
}

func TestParseCandidateMalformedJSON(t *testing.T) {
	raw := `{"short_title": "broken`
	_, err := ParseCandidate(raw)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestAcceptableBoundary(t *testing.T) {
	tests := []struct {
		name string
		cand Candidate
		want bool
	}{
		{
			name: "empty short title",
			cand: Candidate{ShortTitle: "", LongTitle: "five words in this title", Confidence: 0.8},
			want: false,
		},
		{
			name: "empty long title",
			cand: Candidate{ShortTitle: "two words", LongTitle: "", Confidence: 0.8},
			want: false,
		},
		{
			name: "short title 1 word (below min)",
			cand: Candidate{ShortTitle: "word", LongTitle: "five words are in this title", Confidence: 0.8},
			want: false,
		},
		{
			name: "short title 2 words (at min)",
			cand: Candidate{ShortTitle: "two words", LongTitle: "five words are in this title", Confidence: 0.8},
			want: true,
		},
		{
			name: "short title 8 words (at max)",
			cand: Candidate{ShortTitle: "one two three four five six seven eight", LongTitle: "five words are in this title", Confidence: 0.8},
			want: true,
		},
		{
			name: "short title 9 words (above max)",
			cand: Candidate{ShortTitle: "one two three four five six seven eight nine", LongTitle: "five words are in this title", Confidence: 0.8},
			want: false,
		},
		{
			name: "long title 4 words (below min)",
			cand: Candidate{ShortTitle: "two words", LongTitle: "only four words here", Confidence: 0.8},
			want: false,
		},
		{
			name: "long title 5 words (at min)",
			cand: Candidate{ShortTitle: "two words", LongTitle: "five words are in this", Confidence: 0.8},
			want: true,
		},
		{
			name: "long title 18 words (at max)",
			cand: Candidate{ShortTitle: "two words", LongTitle: "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen", Confidence: 0.8},
			want: true,
		},
		{
			name: "long title 19 words (above max)",
			cand: Candidate{ShortTitle: "two words", LongTitle: "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen", Confidence: 0.8},
			want: false,
		},
		{
			name: "confidence 0 (omitted) is acceptable",
			cand: Candidate{ShortTitle: "two words", LongTitle: "five words are in this title", Confidence: 0},
			want: true,
		},
		{
			name: "confidence 0.44 is too low",
			cand: Candidate{ShortTitle: "two words", LongTitle: "five words are in this title", Confidence: 0.44},
			want: false,
		},
		{
			name: "confidence 0.45 is acceptable",
			cand: Candidate{ShortTitle: "two words", LongTitle: "five words are in this title", Confidence: 0.45},
			want: true,
		},
		{
			name: "generic short title rejected",
			cand: Candidate{ShortTitle: "general discussion", LongTitle: "five words are in this title", Confidence: 0.8},
			want: false,
		},
		{
			name: "generic long title rejected",
			cand: Candidate{ShortTitle: "two words", LongTitle: "help with something that is five words long", Confidence: 0.8},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Acceptable(tt.cand)
			if got != tt.want {
				t.Errorf("Acceptable(%+v) = %v, want %v", tt.cand, got, tt.want)
			}
		})
	}
}

func TestBuildConversationSlice(t *testing.T) {
	messages := []session.Message{
		{Role: llm.RoleUser, TextContent: "<<<<< FILE: /tmp/task.txt >>>>> Work in the term-llm worktree and redesign the resume browser to use full width and show more context."},
		{Role: llm.RoleAssistant, TextContent: "I'll inspect the existing implementation and propose a cleaner browsing experience."},
		{Role: llm.RoleUser, TextContent: "It should feel delightful and not like a cramped dialog."},
	}
	slice := BuildConversationSlice(messages)
	if slice == "" {
		t.Fatal("BuildConversationSlice returned empty string")
	}
	if want := "User: Work in the term-llm worktree"; slice[:len(want)] != want {
		t.Fatalf("unexpected slice prefix: %q", slice)
	}
}

func TestBuildConversationSliceEmpty(t *testing.T) {
	slice := BuildConversationSlice(nil)
	if slice != "" {
		t.Fatalf("expected empty string for nil messages, got %q", slice)
	}
	slice = BuildConversationSlice([]session.Message{})
	if slice != "" {
		t.Fatalf("expected empty string for empty messages, got %q", slice)
	}
}

func TestBuildConversationSliceSystemOnly(t *testing.T) {
	messages := []session.Message{
		{Role: "system", TextContent: "You are a helpful assistant."},
		{Role: "tool", TextContent: "tool result here"},
	}
	slice := BuildConversationSlice(messages)
	if slice != "" {
		t.Fatalf("expected empty string for non-user/assistant roles, got %q", slice)
	}
}

func TestNormalizeTitleEdgeCases(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "double quotes stripped", input: `"Quoted Title"`, want: "Quoted Title"},
		{name: "single quotes stripped", input: `'Quoted Title'`, want: "Quoted Title"},
		{name: "trailing period removed", input: "Title with period.", want: "Title with period"},
		{name: "trailing exclamation removed", input: "Exciting title!", want: "Exciting title"},
		{name: "ampersand preserved", input: "Build & deploy", want: "Build & deploy"},
		{name: "slash preserved", input: "CI/CD pipeline", want: "CI/CD pipeline"},
		{name: "whitespace collapsed", input: "  too   many   spaces  ", want: "too many spaces"},
		{name: "empty string", input: "", want: ""},
		{name: "only quotes", input: `""`, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeTitle(tt.input)
			if got != tt.want {
				t.Errorf("normalizeTitle(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGenerateUsesProvider(t *testing.T) {
	provider := llm.NewMockProvider("fast").AddTextResponse(`{"short_title":"Delightful resume browser redesign","long_title":"Redesigning chat TUI resume flow into a real session browser","confidence":0.91}`)
	sess := &session.Session{Summary: "resume command in chat tui is ugly"}
	messages := []session.Message{{Role: llm.RoleUser, TextContent: "resume command in chat tui is ugly and cramped"}}
	cand, err := Generate(context.Background(), provider, sess, messages)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if cand.ShortTitle != "Delightful resume browser redesign" {
		t.Fatalf("ShortTitle = %q", cand.ShortTitle)
	}
}

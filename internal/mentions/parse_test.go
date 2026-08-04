package mentions

import "testing"

func TestActiveTokenAt(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		cursor int
		want   string
		start  int
		ok     bool
	}{
		{"start", "@llmtyp", -1, "llmtyp", 0, true},
		{"middle", "review @internal/llm/typ please", 24, "internal/llm/typ", 7, true},
		{"email", "me@example.com", -1, "", 0, false},
		{"punctuation boundary", "see(@file", -1, "", 0, false},
		{"ideographic full stop boundary", "see。@file", -1, "file", 6, true},
		{"ideographic comma boundary", "see、@file", -1, "file", 6, true},
		{"fullwidth question boundary", "see？@file", -1, "file", 6, true},
		{"fullwidth exclamation boundary", "see！@file", -1, "file", 6, true},
		{"quoted CJK boundary", `see。@"docs/design notes`, -1, "docs/design notes", 6, true},
		{"quoted", `see @"docs/design notes`, -1, "docs/design notes", 4, true},
		{"closed", `see @"docs/design notes.md"`, -1, "", 0, false},
		{"unicode before", `é @資料`, -1, "資料", 3, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cursor := tt.cursor
			if cursor < 0 {
				cursor = len(tt.text)
			}
			got, ok := ActiveTokenAt(tt.text, cursor)
			if ok != tt.ok || ok && (got.Query != tt.want || got.Start != tt.start) {
				t.Fatalf("ActiveTokenAt(%q) = %#v, %v", tt.text, got, ok)
			}
		})
	}
}

func TestInsertText(t *testing.T) {
	if got := InsertText("internal/llm/types.go", false); got != "@internal/llm/types.go" {
		t.Fatalf("plain insertion = %q", got)
	}
	if got := InsertText(`docs/design "notes".md`, false); got != `@"docs/design \"notes\".md"` {
		t.Fatalf("quoted insertion = %q", got)
	}
	if got := InsertText("internal/llm", true); got != "@internal/llm/" {
		t.Fatalf("directory insertion = %q", got)
	}
}

func TestParseSubmitted(t *testing.T) {
	input := "@first.go mail@example.com x@nope.go\n" +
		`review @"docs/design notes.md#L10-20" and @pkg/file.go#L7 and @first.go`
	got := ParseSubmitted(input)
	want := []SubmittedMention{
		{Path: "docs/design notes.md", LineStart: 10, LineEnd: 20},
		{Path: "first.go"},
		{Path: "pkg/file.go", LineStart: 7, LineEnd: 7},
	}
	if len(got) != len(want) {
		t.Fatalf("ParseSubmitted() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mention %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestParseSubmittedCJKStartBoundariesAreNotTerminators(t *testing.T) {
	for _, delimiter := range []string{"。", "、", "？", "！"} {
		t.Run(delimiter, func(t *testing.T) {
			got := ParseSubmitted("prefix" + delimiter + `@plain.go prefix` + delimiter + `@"quoted name.md"`)
			want := []SubmittedMention{{Path: "quoted name.md"}, {Path: "plain.go"}}
			if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
				t.Fatalf("ParseSubmitted() = %#v, want %#v", got, want)
			}
		})
	}

	got := ParseSubmitted("。@first.go、@second.go")
	if len(got) != 1 || got[0].Path != "first.go、@second.go" {
		t.Fatalf("CJK punctuation became an unquoted terminator: %#v", got)
	}
}

func TestParseSubmittedQuotedFirstAndRawDeduplication(t *testing.T) {
	got := ParseSubmitted(`@same.go @./same.go @"quoted name.md" @same.go#L2 @same.go`)
	want := []SubmittedMention{
		{Path: "quoted name.md"},
		{Path: "same.go"},
		{Path: "./same.go"},
		{Path: "same.go", LineStart: 2, LineEnd: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("ParseSubmitted() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mention %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestParseSubmittedQuotedEscapesAndInvalidRanges(t *testing.T) {
	got := ParseSubmitted(`see @"docs/a \"quote\".md" and @ok.go#L9-L12 @back.go#L9-2`)
	if len(got) != 3 {
		t.Fatalf("mentions = %#v", got)
	}
	if got[0].Path != `docs/a "quote".md` {
		t.Fatalf("quoted path = %q", got[0].Path)
	}
	if got[1].LineStart != 9 || got[1].LineEnd != 12 {
		t.Fatalf("range = %#v", got[1])
	}
	if got[2].Path != "back.go#L9-2" || got[2].LineStart != 0 {
		t.Fatalf("invalid backwards range should remain literal: %#v", got[2])
	}

	outside := ParseSubmitted(`@"quoted path.md"#L3-4`)
	if len(outside) != 1 || outside[0] != (SubmittedMention{Path: "quoted path.md"}) {
		t.Fatalf("range outside closing quote was associated: %#v", outside)
	}

	fragments := ParseSubmitted(`@guide.md#heading @guide.md#L4-5#heading`)
	wantFragments := []SubmittedMention{{Path: "guide.md"}, {Path: "guide.md", LineStart: 4, LineEnd: 5}}
	if len(fragments) != len(wantFragments) || fragments[0] != wantFragments[0] || fragments[1] != wantFragments[1] {
		t.Fatalf("fragment parsing = %#v, want %#v", fragments, wantFragments)
	}
}

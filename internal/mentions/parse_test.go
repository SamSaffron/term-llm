package mentions

import (
	"strings"
	"testing"
)

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

func TestActiveTokenDistinguishesQuotedAgentNamespaceFromQuotedFile(t *testing.T) {
	agent, ok := ActiveTokenAt(`ask @agent:"code`, len(`ask @agent:"code`))
	if !ok || !agent.Agent || !agent.Quoted || agent.Query != "code" {
		t.Fatalf("quoted agent token = %#v, %v", agent, ok)
	}
	file, ok := ActiveTokenAt(`open @"agent:code`, len(`open @"agent:code`))
	if !ok || file.Agent || !file.Quoted || file.Query != "agent:code" {
		t.Fatalf("quoted file token = %#v, %v", file, ok)
	}
	files := ParseSubmitted(`@"agent:codebase"`)
	agents, err := ParseSubmittedAgents(`@"agent:codebase"`)
	if err != nil || len(files) != 1 || files[0].Path != "agent:codebase" || len(agents) != 0 {
		t.Fatalf("quoted file submit route: files=%#v agents=%#v err=%v", files, agents, err)
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

func TestParseSubmittedAgentsQuotingBoundariesAndDedupe(t *testing.T) {
	input := `@agent:codebase x@agent:nope see。@agent:"name with spaces" ` +
		`@agent:codebase @agent:unknown-valid @./agent:file`
	got, err := ParseSubmittedAgents(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codebase", "name with spaces", "codebase", "unknown-valid"}
	if len(got) != len(want) {
		t.Fatalf("ParseSubmittedAgents() = %#v, want names %#v", got, want)
	}
	for i, name := range want {
		if got[i].Name != name || input[got[i].Start:got[i].End] == "" {
			t.Fatalf("mention %d = %#v, want name %q", i, got[i], name)
		}
	}
	unique := UniqueAgentMentionNames(got)
	wantUnique := []string{"codebase", "name with spaces", "unknown-valid"}
	if len(unique) != len(wantUnique) {
		t.Fatalf("UniqueAgentMentionNames() = %#v", unique)
	}
	for i := range wantUnique {
		if unique[i] != wantUnique[i] {
			t.Fatalf("unique %d = %q, want %q", i, unique[i], wantUnique[i])
		}
	}
}

func TestParseSubmittedAgentsBoundedGrammarPunctuationAndFileNamespaceCollision(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`ask @agent:codebase, please`, "codebase"},
		{`ask @agent:codebase.`, "codebase"},
		{`ask @agent:team.alpha!`, "team.alpha"},
		{`ask @agent:unknown-valid?`, "unknown-valid"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseSubmittedAgents(tt.input)
			if err != nil || len(got) != 1 || got[0].Name != tt.want {
				t.Fatalf("ParseSubmittedAgents(%q) = %#v, err=%v", tt.input, got, err)
			}
			if token := tt.input[got[0].Start:got[0].End]; strings.ContainsAny(token[len("@agent:"):], ",!?") {
				t.Fatalf("mention range consumed trailing punctuation: %q", token)
			}
		})
	}

	for _, input := range []string{
		`@agent:`,
		`@agent:"`,
		`@agent:""`,
		`@agent:bad"quote`,
		`@agent:"quoted"suffix`,
		`@agent:"quoted"#L3-4`,
		`@agent:reviewer#L3-4`,
		`@agent:path/name`,
		`@agent:bad..name`,
	} {
		t.Run("malformed_"+input, func(t *testing.T) {
			got, err := ParseSubmittedAgents(input)
			if err != nil || len(got) != 0 {
				t.Fatalf("malformed prose should be ordinary text: got=%#v err=%v", got, err)
			}
		})
	}

	files := ParseSubmitted(`@agent:codebase @agent:"name with spaces" @./agent:codebase`)
	if len(files) != 1 || files[0].Path != "./agent:codebase" {
		t.Fatalf("file mentions = %#v", files)
	}
	if got := InsertText("agent:codebase", false); got != "@./agent:codebase" {
		t.Fatalf("reserved file insertion = %q", got)
	}
}

func TestInsertAndActiveAgentMentionText(t *testing.T) {
	if got := InsertAgentText("codebase"); got != "@agent:codebase" {
		t.Fatalf("plain agent insertion = %q", got)
	}
	quoted := InsertAgentText(`name with spaces`)
	if quoted != `@agent:"name with spaces"` {
		t.Fatalf("quoted agent insertion = %q", quoted)
	}
	parsed, err := ParseSubmittedAgents(quoted)
	if err != nil || len(parsed) != 1 || parsed[0].Name != `name with spaces` {
		t.Fatalf("quoted round trip = %#v, err=%v", parsed, err)
	}
	legacy := InsertAgentText("code+review")
	if legacy != `@agent:"code+review"` {
		t.Fatalf("legacy agent insertion = %q", legacy)
	}
	parsed, err = ParseSubmittedAgents(legacy)
	if err != nil || len(parsed) != 1 || parsed[0].Name != "code+review" {
		t.Fatalf("legacy quoted round trip = %#v, err=%v", parsed, err)
	}

	active, ok := ActiveTokenAt(`prefix。@agent:"name with`, len(`prefix。@agent:"name with`))
	if !ok || active.Query != "name with" || !active.Quoted || !active.Agent {
		t.Fatalf("active quoted agent token = %#v, %v", active, ok)
	}
}

func BenchmarkParseSubmittedAgents(b *testing.B) {
	input := strings.Repeat(`review @agent:codebase and @agent:"name with spaces" alongside @internal/mentions/parse.go `, 20)
	b.ReportAllocs()
	for b.Loop() {
		mentions, err := ParseSubmittedAgents(input)
		if err != nil || len(mentions) != 40 {
			b.Fatalf("mentions=%d err=%v", len(mentions), err)
		}
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

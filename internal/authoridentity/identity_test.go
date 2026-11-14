package authoridentity

import (
	"context"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/session"
)

func TestCLINameValidation(t *testing.T) {
	for _, tc := range []struct {
		name, value, want string
		explicit, invalid bool
	}{
		{name: "omitted"},
		{name: "spaces preserved inside", value: "  nick kroll  ", want: "nick kroll"},
		{name: "explicit empty", explicit: true, invalid: true},
		{name: "whitespace", value: " \t ", invalid: true},
		{name: "invalid UTF8", value: string([]byte{0xff}), invalid: true},
		{name: "newline", value: "nick\nkroll", invalid: true},
		{name: "tab", value: "nick\tkroll", invalid: true},
		{name: "escape", value: "nick\x1bkroll", invalid: true},
		{name: "DEL", value: "nick\x7fkroll", invalid: true},
		{name: "max runes", value: strings.Repeat("n", 128), want: strings.Repeat("n", 128)},
		{name: "too many runes", value: strings.Repeat("n", 129), invalid: true},
		{name: "max bytes", value: strings.Repeat("é", 128), want: strings.Repeat("é", 128)},
		{name: "too many bytes", value: strings.Repeat("🙂", 65), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFlag(tc.value, tc.explicit)
			if (err != nil) != tc.invalid || got != tc.want {
				t.Fatalf("got=%q err=%v want=%q invalid=%t", got, err, tc.want, tc.invalid)
			}
		})
	}
}

func TestCLIStampInsertsWithoutReplacingProxyMetadata(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	proxy := session.Author{ID: "sam", Name: "Sam Saffron", Email: "sam@example.com"}
	if err := store.UpsertAuthor(ctx, proxy, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sam", "nick kroll", ""} {
		message := core.UserText("hello")
		if err := Stamp(ctx, store, &message, name); err != nil {
			t.Fatal(err)
		}
		if message.AuthorID != name {
			t.Fatal(message)
		}
	}
	authors, err := store.GetAuthors(ctx, []string{"sam", "nick kroll", ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(authors) != 2 || authors["sam"] != proxy || authors["nick kroll"].Name != "nick kroll" {
		t.Fatalf("directory=%+v", authors)
	}
	message := core.AssistantText("answer")
	if err := Stamp(ctx, store, &message, "sam"); err != nil || message.AuthorID != "" {
		t.Fatalf("assistant stamped: %+v %v", message, err)
	}
}

func TestSelectionValidationAndClearing(t *testing.T) {
	var absent *Selection
	if absent.Name() != "" {
		t.Fatal("nil selection is not anonymous")
	}
	var selected Selection
	if err := selected.Set("  nick kroll "); err != nil || selected.Name() != "nick kroll" {
		t.Fatal(err)
	}
	if err := selected.Set("bad\nname"); err == nil || selected.Name() != "nick kroll" {
		t.Fatalf("invalid selection changed user: %q %v", selected.Name(), err)
	}
	if err := selected.Set(""); err != nil || selected.Name() != "" {
		t.Fatal(err)
	}
}

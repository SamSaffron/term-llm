//go:build !windows

package cmd

import (
	"reflect"
	"testing"
)

func TestChatReloadArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"bare long", []string{"chat", "--resume", "--text"}, []string{"chat", "--text", "--resume=current"}},
		{"bare short", []string{"chat", "-r", "--provider", "debug:fast"}, []string{"chat", "--provider", "debug:fast", "--resume=current"}},
		{"long value", []string{"chat", "--resume=old", "--text"}, []string{"chat", "--text", "--resume=current"}},
		{"short value", []string{"chat", "-r=old", "--text"}, []string{"chat", "--text", "--resume=current"}},
		{"positional after bare", []string{"chat", "--resume", "hello"}, []string{"chat", "hello", "--resume=current"}},
		{"literal delimiter", []string{"chat", "--resume=old", "--", "--resume", "literal"}, []string{"chat", "--resume=current", "--", "--resume", "literal"}},
		{"fresh chat", []string{"chat", "hello"}, []string{"chat", "hello", "--resume=current"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"term-llm"}, tc.args...)
			want := append([]string{"term-llm"}, tc.want...)
			got := chatReloadArgs(args, "current")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %q want %q", got, want)
			}
			if again := chatReloadArgs(got, "current"); !reflect.DeepEqual(again, want) {
				t.Fatalf("repeated reload changed argv: %q", again)
			}
		})
	}
	if got := chatReloadArgs([]string{"term-llm", "chat", "--resume", "--text"}, ""); !reflect.DeepEqual(got, []string{"term-llm", "chat", "--text"}) {
		t.Fatal("no-session reload changed flags", got)
	}
}

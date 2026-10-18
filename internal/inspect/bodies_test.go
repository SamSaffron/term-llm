package inspect

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestInspectBodiesBudgets(t *testing.T) {
	for _, tt := range []struct{ name, text string }{
		{"ascii", strings.Repeat("a", MaxBodyBytes+99)},
		{"utf8", strings.Repeat("世", MaxBodyBytes/3+99)},
		{"escaped", strings.Repeat("\x00", MaxBodyBytes+99)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var messages []session.Message
			var ids []string
			for i := 1; i <= 12; i++ {
				messages = append(messages, inspectMessage(int64(i), core.RoleTool, core.Part{Type: core.PartToolResult, ToolResult: &core.ToolResult{Name: "read_file", Content: tt.text}}))
				ids = append(ids, fmt.Sprintf("m%d.p0.tool_result", i))
			}
			d := Project(messages, Options{TranscriptRev: 42})
			got := d.ResolveBodies(append(ids, "m999.p0.tool_result"))
			if got.TranscriptRev != 42 || len(got.Bodies) == 0 || len(got.Deferred) == 0 || len(got.Missing) != 1 || got.Missing[0] != "m999.p0.tool_result" {
				t.Fatalf("body counts = %d/%d/%v", len(got.Bodies), len(got.Deferred), got.Missing)
			}
			for _, body := range got.Bodies {
				if !body.Truncated || body.TotalBytes != len(tt.text) || len(body.Text) > MaxBodyBytes || !utf8.ValidString(body.Text) || !strings.HasPrefix(tt.text, body.Text) {
					t.Fatalf("invalid truncation: bytes=%d total=%d", len(body.Text), body.TotalBytes)
				}
			}
			encoded, err := json.Marshal(got)
			if err != nil || len(encoded) > MaxBodiesResponseBytes {
				t.Fatalf("response bytes=%d err=%v", len(encoded), err)
			}
			retry := d.ResolveBodies(got.Deferred[:1])
			if len(retry.Bodies) != 1 {
				t.Fatal("deferred body cannot be fetched separately")
			}
		})
	}
}

func TestInspectBodiesMissingAndSessionIsolation(t *testing.T) {
	first := Project([]session.Message{inspectMessage(10, core.RoleUser, core.Part{Type: core.PartText, Text: "first session"})}, Options{SessionID: "first"})
	second := Project([]session.Message{inspectMessage(11, core.RoleUser, core.Part{Type: core.PartText, Text: "second session"})}, Options{SessionID: "second"})
	if len(first.ResolveBodies([]string{"m10.p0.user"}).Bodies) != 1 {
		t.Fatal("fixture")
	}
	got := second.ResolveBodies([]string{"m10.p0.user", "m11.p0.user", "m11.p0.user"})
	if len(got.Missing) != 1 || got.Missing[0] != "m10.p0.user" || len(got.Bodies) != 1 || got.Bodies[0].Text != "second session" {
		t.Fatalf("response = %+v", got)
	}
	empty, _ := json.Marshal(second.ResolveBodies(nil))
	for _, want := range []string{`"bodies":[]`, `"missing":[]`, `"deferred":[]`} {
		if !strings.Contains(string(empty), want) {
			t.Fatal(string(empty))
		}
	}
}

func TestInspectPreviewAndTruncate(t *testing.T) {
	for _, tt := range []struct {
		name, input, want string
		limit             int
	}{
		{"empty", "", "", 2}, {"zero", "abc", "", 0}, {"negative", "abc", "", -1},
		{"ascii", "abcd", "abc", 3}, {"utf8 split", "a世界", "a", 3}, {"utf8 boundary", "a世界", "a世", 4}, {"uncapped", "世", "世", 5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := TruncateUTF8(tt.input, tt.limit); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
	for _, text := range []string{" \t hello\n \r\nworld ", strings.Repeat("世", 300), strings.Repeat(" ", 1000) + strings.Repeat("a ", 300)} {
		got := preview(text)
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) > 200 || strings.ContainsAny(got, "\n\t\r") {
			t.Fatalf("preview = %q", got)
		}
	}
	if got := preview(" \t hello\n \r\nworld "); got != "hello world" {
		t.Fatal(got)
	}
}

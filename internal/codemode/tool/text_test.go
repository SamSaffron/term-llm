//go:build !js

package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

type legacyTextCell struct{ core.CodeModeCell }

func (legacyTextCell) Names() []string                      { return nil }
func (legacyTextCell) Close()                               {}
func (legacyTextCell) Outcomes() []core.CodeModeCallOutcome { return nil }

func TestLegacyTextEmissionsSeparated(t *testing.T) {
	for _, tc := range []struct {
		name, code, want string
	}{
		{"adjacent", `text('one'); text('two')`, "one\ntwo"},
		{"trailing newline", `text('one\n'); text('two')`, "one\ntwo"},
		{"explicit blank line", `text('one\n\n'); text('two\n')`, "one\n\ntwo\n"},
		{"leading newline", `text('one'); text('\ntwo')`, "one\ntwo"},
		{"empty", `text(''); text('one'); text(''); text('two'); text('')`, "one\ntwo"},
		{"JSON and return", `text({a:1}); return [2,3]`, "{\"a\":1}\n[2,3]"},
		{"HTML characters", `text('left<right && right>left')`, "left<right && right>left"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := core.ContextWithCodeModeCell(context.Background(), legacyTextCell{})
			args, err := json.Marshal(map[string]string{"code": tc.code})
			if err != nil {
				t.Fatal(err)
			}
			output, err := New().Execute(ctx, args)
			if err != nil {
				t.Fatal(err)
			}
			if output.IsError || len(output.ContentParts) == 0 {
				t.Fatalf("legacy cell failed: %+v", output)
			}
			emissions := output.ContentParts[:len(output.ContentParts)-1]
			if got := core.ToolResultTextContent(&core.ToolResult{ContentParts: emissions}); got != tc.want {
				t.Fatalf("flattened text = %q, want %q", got, tc.want)
			}
			if strings.Contains(output.Content, `\u003c`) || strings.Contains(output.Content, `\u003e`) || strings.Contains(output.Content, `\u0026`) {
				t.Fatalf("legacy envelope HTML-escaped output: %s", output.Content)
			}
		})
	}
}

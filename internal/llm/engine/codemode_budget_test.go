package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestCodeModeGlobalBudgetRetainsUndeliveredText(t *testing.T) {
	for _, tc := range []struct {
		name       string
		global     int
		compaction int
		tokens     int
	}{
		{"default global", 20000, 0, 10000},
		{"large requested token budget", 20000, 0, 100000},
		{"smaller compaction cap", 20000, 12000, 100000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCodeEngine()
			defer e.CloseCodeMode()
			e.SetMaxToolOutputChars(tc.global)
			limit := tc.global
			if tc.compaction > 0 {
				cc := DefaultCompactionConfig()
				cc.MaxToolResultChars = tc.compaction
				e.SetCompaction(100000, cc)
				limit = min(limit, tc.compaction)
			}
			ctx := asyncCodeContext("budget-session", "budget-observer")
			want := strings.Repeat("é🐈", 5000)
			literal, _ := json.Marshal(want)
			result, err := e.ExecCodeMode(ctx, "text("+string(literal)+")", 10*time.Second, tc.tokens)
			if err != nil {
				t.Fatal(err)
			}
			if !result.HasMoreOutput {
				t.Fatalf("first observation must retain the undelivered tail: %+v", result)
			}
			var got strings.Builder
			for observations := 0; ; observations++ {
				if observations >= 20 {
					t.Fatal("script output did not drain")
				}
				raw, err := json.Marshal(core.CodeModeEnvelopeFor(result, "budget-observer"))
				if err != nil {
					t.Fatal(err)
				}
				parts := append([]core.ToolContentPart(nil), result.Parts...)
				parts = append(parts, core.ToolContentPart{Type: core.ToolContentPartText, Text: "\n" + string(raw)})
				output := e.applyToolOutputTruncation(core.ToolOutput{Content: string(raw), ContentParts: parts, PreserveJSON: true, IsError: result.IsError})
				if output.IsError || strings.Contains(output.Content, `"omitted_output"`) {
					t.Fatalf("fresh output was still cut by downstream projection: %+v", output)
				}
				if len(core.ToolResultTextContent(&core.ToolResult{ContentParts: output.ContentParts})) > limit {
					t.Fatal("effective downstream budget exceeded")
				}
				for _, part := range output.ContentParts[:len(output.ContentParts)-1] {
					got.WriteString(part.Text)
				}
				if result.Status == "completed" && !result.HasMoreOutput {
					break
				}
				result, err = e.WaitCodeMode(ctx, result.CellID, time.Second, tc.tokens)
				if err != nil {
					t.Fatal(err)
				}
			}
			if got.String() != want {
				t.Fatalf("lost/duplicated text: got %d bytes, want %d", got.Len(), len(want))
			}
		})
	}
}

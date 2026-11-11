package engine

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/codemode"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestCodeModeTextEmissionsSeparated(t *testing.T) {
	for _, tc := range []struct {
		name, code, want string
	}{
		{"adjacent", `text('one'); text('two')`, "one\ntwo"},
		{"trailing newline", `text('one\n'); text('two')`, "one\ntwo"},
		{"explicit blank line", `text('one\n\n'); text('two\n')`, "one\n\ntwo\n"},
		{"leading newline", `text('one'); text('\ntwo')`, "one\ntwo"},
		{"empty", `text(''); text('one'); text(''); text('two'); text('')`, "one\ntwo"},
		{"JSON and return", `text({a:1}); return [2,3]`, "{\"a\":1}\n[2,3]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCodeEngine()
			defer e.CloseCodeMode()
			ctx := asyncCodeContext("separation", "exec")
			result, err := e.ExecCodeMode(ctx, tc.code, 2*time.Second, 0)
			if err != nil {
				t.Fatal(err)
			}
			result = awaitCodeModeTerminal(t, e, ctx, result)
			if result.IsError {
				t.Fatalf("cell failed: %+v", result)
			}
			if got := core.ToolResultTextContent(&core.ToolResult{ContentParts: result.Parts}); got != tc.want {
				t.Fatalf("flattened text = %q, want %q", got, tc.want)
			}
		})
	}
}

func emitCodeModeText(t *testing.T, cell *codeModeAsyncCell, text string) {
	t.Helper()
	raw, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	if err := cell.emit("text", raw); err != nil {
		t.Fatal(err)
	}
}

func TestCodeModeTextSeparatorBudgetSplits(t *testing.T) {
	for _, budget := range []int{1, 3, 4, 5, 6, 7} {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			cell := newCodeModeAsyncCell(nil, "test", "session", nil)
			emitCodeModeText(t, cell, "abcd")
			emitCodeModeText(t, cell, "éfg")
			want := "abcd\néfg"
			if cell.pendingBytes != len(want) {
				t.Fatalf("pending bytes = %d, want %d including separator", cell.pendingBytes, len(want))
			}
			var got strings.Builder
			for len(cell.parts) > 0 {
				parts, _ := cell.drainPartsLocked(budget)
				chunk := partsText(core.CodeModeCallResult{Parts: parts})
				if len(chunk) > budget || !utf8.ValidString(chunk) {
					t.Fatalf("invalid budget chunk: %q", chunk)
				}
				if chunk == "" { // One byte cannot hold the next UTF-8 rune.
					parts, _ = cell.drainPartsLocked(2)
					chunk = partsText(core.CodeModeCallResult{Parts: parts})
					if chunk == "" {
						t.Fatal("output did not drain with a UTF-8-sized budget")
					}
				}
				got.WriteString(chunk)
				if cell.pendingBytes != len(want)-got.Len() {
					t.Fatalf("pending byte accounting after %q: %d", got.String(), cell.pendingBytes)
				}
			}
			if got.String() != want {
				t.Fatalf("split emissions = %q, want %q", got.String(), want)
			}
			emitCodeModeText(t, cell, "next")
			parts, _ := cell.drainPartsLocked(100)
			if got := partsText(core.CodeModeCallResult{Parts: parts}); got != "\nnext" {
				t.Fatalf("lost emission boundary after draining: %q", got)
			}
		})
	}
}

func TestCodeModeTextTailRetention(t *testing.T) {
	for _, next := range []string{"b", "é", "\nsummary", strings.Repeat("é", codemode.SessionLimits.RetainBytes)} {
		t.Run(strconv.Itoa(len(next)), func(t *testing.T) {
			cell := newCodeModeAsyncCell(nil, "test", "session", nil)
			first := strings.Repeat("a", codemode.SessionLimits.RetainBytes)
			emitCodeModeText(t, cell, first)
			emitCodeModeText(t, cell, next)
			want := first
			if !strings.HasPrefix(next, "\n") {
				want += "\n"
			}
			want += next
			cut := len(want) - codemode.SessionLimits.RetainBytes
			for cut < len(want) && !utf8.RuneStart(want[cut]) {
				cut++
			}
			want = want[cut:]
			if !cell.truncated || cell.omittedBytes != cut || cell.pendingBytes != len(want) {
				t.Fatalf("retention pending=%d omitted=%d want=%d/%d", cell.pendingBytes, cell.omittedBytes, len(want), cut)
			}
			var got strings.Builder
			for len(cell.parts) > 0 {
				parts, _ := cell.drainPartsLocked(7)
				chunk := partsText(core.CodeModeCallResult{Parts: parts})
				if !utf8.ValidString(chunk) || chunk == "" {
					t.Fatal("invalid pagination")
				}
				got.WriteString(chunk)
			}
			if got.String() != want || cell.pendingBytes != 0 {
				t.Fatal("tail lost or duplicated")
			}
		})
	}
}

func TestCodeModeTailPreservesMediaAndObservedOutput(t *testing.T) {
	cell := newCodeModeAsyncCell(nil, "test", "session", nil)
	emitCodeModeText(t, cell, "observed")
	parts, _ := cell.drainPartsLocked(100)
	if partsText(core.CodeModeCallResult{Parts: parts}) != "observed" {
		t.Fatal(parts)
	}
	emitCodeModeText(t, cell, strings.Repeat("x", codemode.SessionLimits.RetainBytes))
	cell.parts = append(cell.parts, core.ToolContentPart{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{Base64: "aW1n"}})
	emitCodeModeText(t, cell, "final summary")
	if cell.parts[len(cell.parts)-2].Type != core.ToolContentPartImageData || cell.parts[len(cell.parts)-1].Text != "\nfinal summary" {
		t.Fatal("text moved ahead of image")
	}
	before := cell.pendingBytes
	parts, limited := cell.drainPartsLocked(4)
	if !limited || len(parts) != 1 || len(parts[0].Text) != 4 || cell.pendingBytes != before-4 {
		t.Fatal("pagination reordered image")
	}
	parts, _ = cell.drainPartsLocked(codemode.SessionLimits.RetainBytes)
	if parts[len(parts)-2].Type != core.ToolContentPartImageData || parts[len(parts)-1].Text != "\nfinal summary" {
		t.Fatal("media order lost")
	}
}

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

func TestCodeModeTextSeparatorRetention(t *testing.T) {
	for _, tc := range []struct {
		name, suffix, next, retained string
		space, omitted               int
	}{
		{"partial", "", "bcd", "\nb", 2, 2},
		{"full", "", "b", "", 0, 2},
		{"UTF-8", "", "é", "\n", 1, 2},
		{"existing newline", "\n", "bc", "b", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cell := newCodeModeAsyncCell(nil, "test", "session", nil)
			first := strings.Repeat("a", codemode.SessionLimits.RetainBytes-tc.space-len(tc.suffix)) + tc.suffix
			emitCodeModeText(t, cell, first)
			emitCodeModeText(t, cell, tc.next)
			if !cell.truncated || cell.omittedBytes != tc.omitted {
				t.Fatalf("retention omitted %d bytes, want %d; truncated=%v", cell.omittedBytes, tc.omitted, cell.truncated)
			}
			parts, _ := cell.drainPartsLocked(codemode.SessionLimits.RetainBytes)
			if got := partsText(core.CodeModeCallResult{Parts: parts}); got != first+tc.retained {
				t.Fatalf("retained text suffix = %q, want %q", strings.TrimPrefix(got, first), tc.retained)
			}
			if cell.pendingBytes != 0 {
				t.Fatalf("pending bytes after drain = %d", cell.pendingBytes)
			}
		})
	}
}

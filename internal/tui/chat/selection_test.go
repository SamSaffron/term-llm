package chat

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/ui"
)

func TestSelectionNormalized(t *testing.T) {
	tests := []struct {
		name      string
		anchor    ContentPos
		cursor    ContentPos
		wantStart ContentPos
		wantEnd   ContentPos
	}{
		{
			name:      "anchor before cursor",
			anchor:    ContentPos{Line: 1, Col: 3},
			cursor:    ContentPos{Line: 5, Col: 10},
			wantStart: ContentPos{Line: 1, Col: 3},
			wantEnd:   ContentPos{Line: 5, Col: 10},
		},
		{
			name:      "cursor before anchor",
			anchor:    ContentPos{Line: 5, Col: 10},
			cursor:    ContentPos{Line: 1, Col: 3},
			wantStart: ContentPos{Line: 1, Col: 3},
			wantEnd:   ContentPos{Line: 5, Col: 10},
		},
		{
			name:      "same line anchor col before cursor col",
			anchor:    ContentPos{Line: 2, Col: 5},
			cursor:    ContentPos{Line: 2, Col: 15},
			wantStart: ContentPos{Line: 2, Col: 5},
			wantEnd:   ContentPos{Line: 2, Col: 15},
		},
		{
			name:      "same line cursor col before anchor col",
			anchor:    ContentPos{Line: 2, Col: 15},
			cursor:    ContentPos{Line: 2, Col: 5},
			wantStart: ContentPos{Line: 2, Col: 5},
			wantEnd:   ContentPos{Line: 2, Col: 15},
		},
		{
			name:      "same position",
			anchor:    ContentPos{Line: 3, Col: 7},
			cursor:    ContentPos{Line: 3, Col: 7},
			wantStart: ContentPos{Line: 3, Col: 7},
			wantEnd:   ContentPos{Line: 3, Col: 7},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Selection{
				Active: true,
				Anchor: tt.anchor,
				Cursor: tt.cursor,
			}
			start, end := s.Normalized()
			if start != tt.wantStart {
				t.Errorf("start = %+v, want %+v", start, tt.wantStart)
			}
			if end != tt.wantEnd {
				t.Errorf("end = %+v, want %+v", end, tt.wantEnd)
			}
		})
	}
}

func TestClampInt(t *testing.T) {
	tests := []struct {
		v, lo, hi, want int
	}{
		{5, 0, 10, 5},
		{-1, 0, 10, 0},
		{15, 0, 10, 10},
		{0, 0, 0, 0},
	}

	for _, tt := range tests {
		got := ui.ClampInt(tt.v, tt.lo, tt.hi)
		if got != tt.want {
			t.Errorf("ClampInt(%d, %d, %d) = %d, want %d", tt.v, tt.lo, tt.hi, got, tt.want)
		}
	}
}

func TestCutVisual(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		startCol int
		endCol   int
		want     string
	}{
		{
			name:     "ascii mid-word",
			input:    "hello world",
			startCol: 2,
			endCol:   7,
			want:     "llo w",
		},
		{
			name:     "full line",
			input:    "hello",
			startCol: 0,
			endCol:   5,
			want:     "hello",
		},
		{
			name:     "zero width",
			input:    "hello",
			startCol: 3,
			endCol:   3,
			want:     "",
		},
		{
			name:     "wide chars CJK",
			input:    "a你好b",
			startCol: 1,
			endCol:   5,
			want:     "你好",
		},
		{
			name:     "beyond end clamps",
			input:    "hi",
			startCol: 0,
			endCol:   100,
			want:     "hi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cutVisual(tt.input, tt.startCol, tt.endCol)
			if got != tt.want {
				t.Errorf("cutVisual(%q, %d, %d) = %q, want %q",
					tt.input, tt.startCol, tt.endCol, got, tt.want)
			}
		})
	}
}

func TestExtractSelectedText(t *testing.T) {
	tests := []struct {
		name         string
		contentLines []string
		start        ContentPos
		end          ContentPos
		want         string
	}{
		{
			name:         "single line partial word",
			contentLines: []string{"hello world foo bar"},
			start:        ContentPos{Line: 0, Col: 6},
			end:          ContentPos{Line: 0, Col: 11},
			want:         "world",
		},
		{
			name:         "multi-line with partial first and last",
			contentLines: []string{"first line", "second line", "third line"},
			start:        ContentPos{Line: 0, Col: 6},
			end:          ContentPos{Line: 2, Col: 5},
			want:         "line\nsecond line\nthird",
		},
		{
			name:         "full lines multi-line",
			contentLines: []string{"aaa", "bbb", "ccc"},
			start:        ContentPos{Line: 0, Col: 0},
			end:          ContentPos{Line: 2, Col: 3},
			want:         "aaa\nbbb\nccc",
		},
		{
			name:         "empty selection same position",
			contentLines: []string{"hello"},
			start:        ContentPos{Line: 0, Col: 3},
			end:          ContentPos{Line: 0, Col: 3},
			want:         "",
		},
		{
			name:         "selection beyond content lines",
			contentLines: []string{"only line"},
			start:        ContentPos{Line: 5, Col: 0},
			end:          ContentPos{Line: 6, Col: 5},
			want:         "",
		},
		{
			name:         "wide chars single line",
			contentLines: []string{"a你好b"},
			start:        ContentPos{Line: 0, Col: 1},
			end:          ContentPos{Line: 0, Col: 5},
			want:         "你好",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Model{
				contentLines: tt.contentLines,
				selection: Selection{
					Active: true,
					Anchor: tt.start,
					Cursor: tt.end,
				},
			}
			got := m.extractSelectedText()
			if got != tt.want {
				t.Errorf("extractSelectedText() = %q, want %q", got, tt.want)
			}
		})
	}
}

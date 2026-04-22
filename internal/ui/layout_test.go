package ui

import "testing"

func TestClampInt(t *testing.T) {
	tests := []struct {
		name  string
		value int
		lo    int
		hi    int
		want  int
	}{
		{name: "within range", value: 5, lo: 0, hi: 10, want: 5},
		{name: "below range", value: -1, lo: 0, hi: 10, want: 0},
		{name: "above range", value: 15, lo: 0, hi: 10, want: 10},
		{name: "single point", value: 4, lo: 2, hi: 2, want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClampInt(tt.value, tt.lo, tt.hi); got != tt.want {
				t.Fatalf("ClampInt(%d, %d, %d) = %d, want %d", tt.value, tt.lo, tt.hi, got, tt.want)
			}
		})
	}
}

func TestVisibleRange(t *testing.T) {
	tests := []struct {
		name      string
		total     int
		cursor    int
		visible   int
		wantStart int
		wantEnd   int
	}{
		{name: "empty", total: 0, cursor: 0, visible: 5, wantStart: 0, wantEnd: 0},
		{name: "fits all", total: 3, cursor: 1, visible: 10, wantStart: 0, wantEnd: 3},
		{name: "cursor at top", total: 10, cursor: 0, visible: 3, wantStart: 0, wantEnd: 3},
		{name: "cursor in middle", total: 10, cursor: 5, visible: 3, wantStart: 3, wantEnd: 6},
		{name: "cursor clamped at end", total: 3, cursor: 99, visible: 2, wantStart: 1, wantEnd: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStart, gotEnd := VisibleRange(tt.total, tt.cursor, tt.visible)
			if gotStart != tt.wantStart || gotEnd != tt.wantEnd {
				t.Fatalf("VisibleRange(%d, %d, %d) = (%d, %d), want (%d, %d)", tt.total, tt.cursor, tt.visible, gotStart, gotEnd, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestVisibleRangeByHeights(t *testing.T) {
	tests := []struct {
		name      string
		heights   []int
		cursor    int
		maxRows   int
		wantStart int
		wantEnd   int
	}{
		{name: "empty", heights: nil, cursor: 0, maxRows: 5, wantStart: 0, wantEnd: 0},
		{name: "fits all", heights: []int{1, 2, 1}, cursor: 1, maxRows: 10, wantStart: 0, wantEnd: 3},
		{name: "centered by height", heights: []int{2, 2, 2, 2}, cursor: 2, maxRows: 4, wantStart: 1, wantEnd: 3},
		{name: "selected item alone when full", heights: []int{2, 3, 1}, cursor: 1, maxRows: 3, wantStart: 1, wantEnd: 2},
		{name: "cursor clamped", heights: []int{1, 1, 1}, cursor: 99, maxRows: 2, wantStart: 1, wantEnd: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStart, gotEnd := VisibleRangeByHeights(tt.heights, tt.cursor, tt.maxRows)
			if gotStart != tt.wantStart || gotEnd != tt.wantEnd {
				t.Fatalf("VisibleRangeByHeights(%v, %d, %d) = (%d, %d), want (%d, %d)", tt.heights, tt.cursor, tt.maxRows, gotStart, gotEnd, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestRemainingLines(t *testing.T) {
	if got := RemainingLines(24, 5); got != 19 {
		t.Fatalf("RemainingLines(24, 5) = %d, want 19", got)
	}
	if got := RemainingLines(2, 10); got != 1 {
		t.Fatalf("RemainingLines(2, 10) = %d, want 1", got)
	}
}

func TestRemainingHeight(t *testing.T) {
	if got := RemainingHeight(24, "line one\nline two", "footer"); got != 21 {
		t.Fatalf("RemainingHeight(...) = %d, want 21", got)
	}
	if got := RemainingHeight(2, "a\nb\nc"); got != 1 {
		t.Fatalf("RemainingHeight clamp = %d, want 1", got)
	}
}

package ui

import (
	"charm.land/bubbles/v2/viewport"
	"charm.land/lipgloss/v2"
)

// ClampInt constrains v to the inclusive range [lo, hi].
func ClampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// RemainingLines subtracts reserved lines from total and always returns at least 1.
func RemainingLines(total, reserved int) int {
	return max(1, total-reserved)
}

// RemainingHeight subtracts the rendered heights of the provided sections from
// total and always returns at least 1.
func RemainingHeight(total int, sections ...string) int {
	reserved := 0
	for _, section := range sections {
		reserved += lipgloss.Height(section)
	}
	return RemainingLines(total, reserved)
}

// VisibleRange returns the [start, end) range of items that should be shown to
// keep cursor visible inside a window of visible items.
func VisibleRange(total, cursor, visible int) (start, end int) {
	if total <= 0 {
		return 0, 0
	}
	if visible < 1 {
		visible = 1
	}
	cursor = ClampInt(cursor, 0, total-1)
	if total <= visible {
		return 0, total
	}
	start = cursor - visible + 1
	if start < 0 {
		start = 0
	}
	end = start + visible
	if end > total {
		end = total
		start = max(0, end-visible)
	}
	return start, end
}

// VisibleRangeByHeights returns the [start, end) range of items that can fit
// within maxRows while keeping cursor visible. Item heights are measured in
// terminal rows.
func VisibleRangeByHeights(heights []int, cursor, maxRows int) (start, end int) {
	if len(heights) == 0 {
		return 0, 0
	}
	if maxRows < 1 {
		maxRows = 1
	}
	cursor = ClampInt(cursor, 0, len(heights)-1)

	total := 0
	for _, h := range heights {
		total += max(1, h)
	}
	if total <= maxRows {
		return 0, len(heights)
	}

	start, end = cursor, cursor+1
	used := max(1, heights[cursor])
	if used > maxRows {
		return start, end
	}

	prev := cursor - 1
	next := cursor + 1
	for {
		added := false
		if prev >= 0 {
			h := max(1, heights[prev])
			if used+h <= maxRows {
				start = prev
				used += h
				prev--
				added = true
			}
		}
		if next < len(heights) {
			h := max(1, heights[next])
			if used+h <= maxRows {
				end = next + 1
				used += h
				next++
				added = true
			}
		}
		if !added {
			break
		}
	}

	return start, end
}

// NewViewportWithFooter creates a viewport sized for a layout with a fixed
// number of footer lines.
func NewViewportWithFooter(width, height, footerLines int) viewport.Model {
	return viewport.New(
		viewport.WithWidth(width),
		viewport.WithHeight(RemainingLines(height, footerLines)),
	)
}

// ResizeViewportWithFooter updates a viewport for a layout with a fixed number
// of footer lines.
func ResizeViewportWithFooter(vp *viewport.Model, width, height, footerLines int) {
	vp.SetWidth(width)
	vp.SetHeight(RemainingLines(height, footerLines))
}

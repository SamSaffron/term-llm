package cmd

import (
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/terminaltext"
)

// activityBytesBetweenLocked returns retained, unclaimed output for [start,end).
// Caller must hold s.mu. Returned bytes retain gaps where protocol/command ranges
// were conservatively claimed.
func (s *serveShell) activityBytesBetweenLocked(start, end int64) ([]byte, bool) {
	if start >= end {
		return nil, false
	}
	truncated := false
	if start < s.activityFloor {
		start = s.activityFloor
		truncated = true
	}
	var raw []byte
	for _, segment := range s.activitySegments {
		segStart, segEnd := segment.start, segment.start+int64(len(segment.data))
		from, to := max64(start, segStart), min64(end, segEnd)
		if from >= to {
			continue
		}
		for _, claimed := range s.claimedRanges {
			if claimed.end <= from || claimed.start >= to {
				continue
			}
			if from < claimed.start {
				raw = append(raw, segment.data[from-segStart:claimed.start-segStart]...)
			}
			from = max64(from, claimed.end)
			if from >= to {
				break
			}
		}
		if from < to {
			raw = append(raw, segment.data[from-segStart:to-segStart]...)
		}
	}
	return raw, truncated
}

func sanitizeServeShellActivityText(raw []byte, limit int) (string, bool) {
	clean := terminaltext.SanitizeBytes(raw)
	truncated := false
	if limit > 0 && len(clean) > limit {
		clean = clean[len(clean)-limit:]
		for len(clean) > 0 && !utf8.Valid(clean) {
			clean = clean[1:]
		}
		truncated = true
	}
	return string(clean), truncated
}

func (s *serveShell) activityExcerptBetweenLocked(start, end int64, limit int) (string, bool) {
	raw, truncated := s.activityBytesBetweenLocked(start, end)
	text, textTruncated := sanitizeServeShellActivityText(raw, limit)
	return text, truncated || textTruncated
}

func (s *serveShell) activityExcerptBetween(start, end int64, limit int) (string, bool) {
	s.mu.Lock()
	text, truncated := s.activityExcerptBetweenLocked(start, end, limit)
	s.mu.Unlock()
	return text, truncated
}

// consumeShellResultActivity returns setup/lease-wait activity at most once in
// the shell-result channel, even when multiple calls began waiting at the same
// offset. Durable top-level activity has an independent cursor by design.
func (s *serveShell) consumeShellResultActivity(start, end int64, limit int) (string, bool) {
	s.mu.Lock()
	if start < s.shellResultActivityCursor {
		start = s.shellResultActivityCursor
	}
	raw, truncated := s.activityBytesBetweenLocked(start, end)
	if end > s.shellResultActivityCursor {
		s.shellResultActivityCursor = end
	}
	s.mu.Unlock()
	text, textTruncated := sanitizeServeShellText(raw, limit)
	return text, truncated || textTruncated
}

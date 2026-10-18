package inspect

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxBodyBytes           = 256 * 1024
	MaxBodiesResponseBytes = 2 * 1024 * 1024
)

// TruncateUTF8 caps bytes without splitting a code point.
func TruncateUTF8(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

func preview(text string) string {
	// Bound work and allocation even for a multi-megabyte body. Collapsed leading
	// whitespace does not consume the 200-character preview allowance.
	var b strings.Builder
	count, pendingSpace := 0, false
	for _, r := range text {
		if unicode.IsSpace(r) {
			pendingSpace = count > 0
			continue
		}
		if pendingSpace {
			b.WriteByte(' ')
			count++
			pendingSpace = false
		}
		if count >= 200 {
			break
		}
		b.WriteRune(r)
		count++
		if count >= 200 {
			break
		}
	}
	return b.String()
}

// ResolveBodies resolves only IDs belonging to this document. The budget counts
// encoded JSON bytes (not just text), including worst-case missing/deferred IDs.
func (d *Document) ResolveBodies(ids []string) BodiesResponse {
	out := BodiesResponse{TranscriptRev: d.TranscriptRev, Bodies: []Body{}, Missing: []string{}, Deferred: []string{}}
	overhead, _ := json.Marshal(BodiesResponse{TranscriptRev: d.TranscriptRev, Bodies: []Body{}, Missing: ids, Deferred: ids})
	remaining := MaxBodiesResponseBytes - len(overhead)
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		body, ok := d.bodies[id]
		if !ok {
			out.Missing = append(out.Missing, id)
			continue
		}
		body.Text = TruncateUTF8(body.Text, MaxBodyBytes)
		body.Truncated = len(body.Text) < body.TotalBytes
		encoded, _ := json.Marshal(body)
		size := len(encoded) + 1
		if size > remaining {
			out.Deferred = append(out.Deferred, id)
			continue
		}
		remaining -= size
		out.Bodies = append(out.Bodies, body)
	}
	return out
}

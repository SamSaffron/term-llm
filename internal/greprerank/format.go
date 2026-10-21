package greprerank

import (
	"strings"
	"unicode/utf8"
)

const rankedCapNote = "\n[output capped at 50KB — narrow your search]\n"

// FormatRanked renders matches in the given (ranked) order, repeating
// the file header per block. Ranked matches must not pass through
// the regrouping grep formatter: it regroups by file and caps blocks per file, which would
// destroy rank order. note may be called several times: with larger counts to
// reserve space, then with the number of blocks actually rendered. It must be
// pure and return a one-line note (including any trailing newline).
// The result never exceeds maxBytes. If the top-ranked block alone is too
// large it is truncated, keeping its file header and first matched line.
func FormatRanked(matches []Item, note func(displayed int) string, maxBytes int) string {
	// Conservative reservation: the note's length for the largest possible
	// count (digit widths only grow with the count).
	noteReserve := len(note(len(matches)))
	if n := len(note(0)); n > noteReserve {
		noteReserve = n
	}

	var body strings.Builder
	displayed := 0
	capped := false
	for i, it := range matches {
		reserve := noteReserve
		if i < len(matches)-1 {
			reserve += len(rankedCapNote)
		}
		limit := maxBytes - reserve
		blockLen := 1 + len(it.Path) + 2 + len(it.Block) + 1
		if body.Len()+blockLen <= limit {
			body.WriteString("\n")
			body.WriteString(it.Path)
			body.WriteString(":\n")
			body.WriteString(it.Block)
			body.WriteString("\n")
			displayed++
			continue
		}
		capped = true
		if displayed == 0 {
			avail := maxBytes - noteReserve - len(rankedCapNote)
			body.WriteString(truncatedRankedBlock(it, avail))
			displayed++
		}
		break
	}

	var out strings.Builder
	out.WriteString(note(displayed))
	out.WriteString(body.String())
	if capped {
		out.WriteString(rankedCapNote)
	}
	return out.String()
}

// truncatedRankedBlock renders one block in at most budget bytes, preserving the
// file header and the first matched ("> N: ...") line where possible.
func truncatedRankedBlock(it Item, budget int) string {
	path := it.Path
	if maxPath := budget / 2; len(path) > maxPath {
		path = cutUTF8(path, maxPath)
	}
	header := "\n" + path + ":\n"
	avail := budget - len(header) - 1 // trailing newline
	if avail <= 0 {
		return header
	}

	ctx := it.Block
	start := 0
	if !strings.HasPrefix(ctx, "> ") {
		if idx := strings.Index(ctx, "\n> "); idx >= 0 {
			start = idx + 1
		}
	}
	end := len(ctx)
	if nl := strings.IndexByte(ctx[start:], '\n'); nl >= 0 {
		end = start + nl
	}

	var text string
	if end <= avail {
		// Matched line fits: keep as much surrounding context as the budget allows.
		text = cutUTF8(ctx, avail)
	} else {
		text = cutUTF8(ctx[start:end], avail)
	}
	return header + text + "\n"
}

// cutUTF8 returns a prefix of s no longer than n bytes without splitting a rune.
func cutUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

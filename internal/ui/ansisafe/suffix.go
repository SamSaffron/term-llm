package ansisafe

// suffixStartIndex returns the starting index for a safe suffix that avoids
// splitting ANSI CSI escape sequences.
func suffixStartIndex[S ~string | ~[]byte](s S, pos int) int {
	if pos <= 0 {
		return 0
	}
	if pos >= len(s) {
		return len(s)
	}

	// Scan backwards to find if we're inside a CSI escape sequence.
	// Use 64 bytes as limit to handle long SGR sequences (multiple params, 256-color, etc.)
	for index := pos - 1; index >= 0 && index >= pos-64; index-- {
		if s[index] != 0x1B {
			continue
		}

		// Found ESC, check if this is a CSI sequence (ESC '[')
		if index+1 >= len(s) || s[index+1] != '[' {
			// Not a CSI sequence (could be OSC, cursor save, etc.)
			return pos
		}

		// This is a CSI sequence - paramStart is after the '['
		paramStart := index + 2

		// Check if sequence terminates before pos
		for term := paramStart; term < pos; term++ {
			if s[term] >= 0x40 && s[term] <= 0x7E { // CSI terminator
				return pos // sequence ended, safe to slice
			}
		}

		// No terminator found before pos - we're mid-sequence
		// Advance pos past the sequence terminator
		searchStart := pos
		if searchStart < paramStart {
			searchStart = paramStart
		}
		for term := searchStart; term < len(s); term++ {
			if s[term] >= 0x40 && s[term] <= 0x7E {
				return term + 1
			}
		}

		// No terminator found at all - malformed sequence, keep original split
		return pos
	}

	return pos
}

// SuffixString returns s[pos:] but adjusts pos forward if it would split an
// ANSI CSI sequence.
func SuffixString(s string, pos int) string {
	start := suffixStartIndex(s, pos)
	if start >= len(s) {
		return ""
	}
	return s[start:]
}

// SuffixBytes returns s[pos:] but adjusts pos forward if it would split an
// ANSI CSI sequence.
func SuffixBytes(s []byte, pos int) []byte {
	start := suffixStartIndex(s, pos)
	if start >= len(s) {
		return nil
	}
	return s[start:]
}

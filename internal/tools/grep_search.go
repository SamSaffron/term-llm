package tools

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// searchFile searches a single file for matches.
func searchFile(path string, re *regexp.Regexp, maxMatches, contextLines int) ([]GrepMatch, error) {
	return searchFileBlocks(path, re, maxMatches, contextLines, false)
}

// searchFileBlocks keeps structured line windows for the intent-only Go path.
// Plain grep retains its original unmerged matches and output.
func searchFileBlocks(path string, re *regexp.Regexp, maxMatches, contextLines int, merge bool) ([]GrepMatch, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Check for binary
	buf := make([]byte, 512)
	n, err := file.Read(buf)
	if err != nil && n == 0 {
		return nil, err
	}

	contentType := http.DetectContentType(buf[:n])
	if !strings.HasPrefix(contentType, "text/") &&
		!strings.Contains(contentType, "json") &&
		!strings.Contains(contentType, "xml") {
		return nil, fmt.Errorf("binary file")
	}

	// Reset to beginning
	if _, err := file.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("seek: %w", err)
	}

	// Read all lines for context support
	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Search for matches
	var matches []GrepMatch
	for lineNum, line := range lines {
		if re.MatchString(line) {
			match := GrepMatch{
				FilePath:   path,
				LineNumber: lineNum + 1, // 1-indexed
				Match:      line,
				Context:    buildContext(lines, lineNum, contextLines),
			}
			matches = append(matches, match)

			if len(matches) >= maxMatches {
				break
			}
		}
	}

	if merge {
		return mergeSearchMatches(path, lines, matches, contextLines), nil
	}
	return matches, nil
}

// Merge directly from the source lines, never by parsing formatted Context.
func mergeSearchMatches(path string, lines []string, matches []GrepMatch, contextLines int) []GrepMatch {
	pending := make([]pendingMatch, len(matches))
	for i, m := range matches {
		p := pendingMatch{filePath: path, lineNumber: m.LineNumber, matchLine: m.Match}
		start, end := max(0, m.LineNumber-1-contextLines), min(len(lines), m.LineNumber+contextLines)
		for j := start; j < m.LineNumber-1; j++ {
			p.before = append(p.before, contextEntry{j + 1, lines[j]})
		}
		for j := m.LineNumber; j < end; j++ {
			p.after = append(p.after, contextEntry{j + 1, lines[j]})
		}
		pending[i] = p
	}
	blocks := pendingsToBlocks(pending)
	out := make([]GrepMatch, len(blocks))
	for i, b := range blocks {
		out[i] = blockToGrepMatch(b)
	}
	return out
}

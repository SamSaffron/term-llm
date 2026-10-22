package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/samsaffron/term-llm/internal/grepsearch"
)

type grepFileScanner func(context.Context, string, *regexp.Regexp, int, func([]string, int) error) error

type grepSearchControl struct {
	ctx    context.Context
	scan   grepFileScanner
	match  func(GrepMatch)
	blocks func([]matchBlock)
}

func searchFile(path string, re *regexp.Regexp, maxMatches, contextLines int) ([]GrepMatch, error) {
	return searchFileWithControl(path, re, maxMatches, contextLines)
}

// The optional controls apply only to intent expansion. Plain grep retains its
// original read/scan/format ordering and unmerged matches.
func searchFileWithControl(path string, re *regexp.Regexp, maxMatches, contextLines int, controls ...grepSearchControl) ([]GrepMatch, error) {
	control := grepSearchControl{scan: grepsearch.Scan}
	if len(controls) > 0 {
		control = controls[0]
	}
	var matches []GrepMatch
	var source []string
	err := control.scan(control.ctx, path, re, maxMatches, func(lines []string, index int) error {
		source = lines
		match := GrepMatch{FilePath: path, LineNumber: index + 1, Match: lines[index], Context: buildContext(lines, index, contextLines)}
		matches = append(matches, match)
		if control.match != nil {
			control.match(match)
		}
		if control.ctx != nil {
			return control.ctx.Err()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if control.blocks != nil {
		control.blocks(searchMatchBlocks(path, source, matches, contextLines))
		return matches, nil
	}
	return matches, nil
}

// Capture source windows for later merging, never by parsing formatted Context.
func searchMatchBlocks(path string, lines []string, matches []GrepMatch, contextLines int) []matchBlock {
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
	blocks := make([]matchBlock, len(pending))
	for i := range pending {
		blocks[i] = pendingToBlock(&pending[i])
	}
	return blocks
}

// buildContext builds context lines around a match.
func buildContext(lines []string, matchIdx, contextLines int) string {
	start := matchIdx - contextLines
	if start < 0 {
		start = 0
	}
	end := matchIdx + contextLines + 1
	if end > len(lines) {
		end = len(lines)
	}

	var sb strings.Builder
	for i := start; i < end; i++ {
		prefix := "  "
		if i == matchIdx {
			prefix = "> "
		}
		sb.WriteString(fmt.Sprintf("%s%d: %s\n", prefix, i+1, truncateLine(lines[i])))
	}

	return strings.TrimSuffix(sb.String(), "\n")
}

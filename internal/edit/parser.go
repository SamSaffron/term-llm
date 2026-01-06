package edit

import (
	"fmt"
	"strings"
)

// Format represents the edit format being used within a file block.
type Format int

const (
	FormatUnknown       Format = iota
	FormatSearchReplace        // <<<<<<< SEARCH ... ======= ... >>>>>>> REPLACE
	FormatUnifiedDiff          // --- +++ @@ with space/minus/plus prefixes
)

func (f Format) String() string {
	switch f {
	case FormatSearchReplace:
		return "search-replace"
	case FormatUnifiedDiff:
		return "unified-diff"
	default:
		return "unknown"
	}
}

// ParserState represents the current state of the streaming parser.
type ParserState int

const (
	StateIdle            ParserState = iota // Looking for [FILE:] or [ABOUT]
	StateInFile                             // Inside [FILE:], detecting format
	StateInSearch                           // Accumulating search content
	StateInReplace                          // Accumulating replace content
	StateInDiff                             // Accumulating unified diff content
	StateInAbout                            // Inside [ABOUT] block
)

func (s ParserState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateInFile:
		return "in-file"
	case StateInSearch:
		return "in-search"
	case StateInReplace:
		return "in-replace"
	case StateInDiff:
		return "in-diff"
	case StateInAbout:
		return "in-about"
	default:
		return "unknown"
	}
}

// SearchReplaceEdit represents a single search/replace edit.
type SearchReplaceEdit struct {
	Search  string
	Replace string
}

// FileEdit represents all edits for a single file.
type FileEdit struct {
	Path             string
	Format           Format
	SearchReplaces   []SearchReplaceEdit // For search/replace format
	UnifiedDiffLines []string            // For unified diff format
}

// ParserCallbacks contains callbacks invoked during parsing.
type ParserCallbacks struct {
	// OnFileStart is called when a [FILE: path] block begins.
	OnFileStart func(path string)

	// OnSearchReady is called when a search block is complete (at =======).
	// Return an error to halt parsing (e.g., if search doesn't match).
	OnSearchReady func(path, search string) error

	// OnReplaceReady is called when a search/replace pair is complete.
	OnReplaceReady func(path, search, replace string)

	// OnDiffReady is called when unified diff content is complete for a file.
	// diffLines contains the raw diff lines (including --- +++ @@ prefixes).
	OnDiffReady func(path string, diffLines []string) error

	// OnFileComplete is called when [/FILE] is seen.
	OnFileComplete func(edit FileEdit)

	// OnAboutLine is called for each line in the [ABOUT] section.
	OnAboutLine func(line string)

	// OnAboutComplete is called when [/ABOUT] is seen.
	OnAboutComplete func(content string)

	// OnText is called for non-edit text (before first [FILE:] or after [/ABOUT]).
	OnText func(text string)
}

// StreamParser parses streaming edit output.
type StreamParser struct {
	state      ParserState
	callbacks  ParserCallbacks
	buffer     strings.Builder // Accumulates partial lines
	lineBuffer []string        // Complete lines not yet processed

	// Current file state
	currentFile    string
	currentFormat  Format
	currentSearch  strings.Builder
	currentReplace strings.Builder
	currentDiff    []string
	fileEdits      []SearchReplaceEdit

	// About section
	aboutContent strings.Builder

	// Error state
	halted    bool
	haltError error
}

// NewStreamParser creates a new parser with the given callbacks.
func NewStreamParser(callbacks ParserCallbacks) *StreamParser {
	return &StreamParser{
		state:     StateIdle,
		callbacks: callbacks,
	}
}

// Feed processes a chunk of streaming text.
// Returns an error if parsing should halt (e.g., validation failed).
func (p *StreamParser) Feed(chunk string) error {
	if p.halted {
		return p.haltError
	}

	p.buffer.WriteString(chunk)
	return p.processBuffer()
}

// Finish signals that all input has been received.
// Processes any remaining buffered content.
func (p *StreamParser) Finish() error {
	if p.halted {
		return p.haltError
	}

	// Process any remaining content
	remaining := p.buffer.String()
	if remaining != "" {
		p.lineBuffer = append(p.lineBuffer, remaining)
		p.buffer.Reset()
	}

	// Process remaining lines
	for len(p.lineBuffer) > 0 {
		if err := p.processLine(p.lineBuffer[0]); err != nil {
			return err
		}
		p.lineBuffer = p.lineBuffer[1:]
	}

	// Handle incomplete states - flush any pending content
	if p.state == StateInAbout {
		// LLM didn't output [/ABOUT] - flush what we have
		if p.callbacks.OnAboutComplete != nil && p.aboutContent.Len() > 0 {
			p.callbacks.OnAboutComplete(strings.TrimSpace(p.aboutContent.String()))
		}
	}

	return nil
}

// State returns the current parser state.
func (p *StreamParser) State() ParserState {
	return p.state
}

// CurrentFile returns the path of the file currently being parsed.
func (p *StreamParser) CurrentFile() string {
	return p.currentFile
}

// IsHalted returns true if parsing was halted due to an error.
func (p *StreamParser) IsHalted() bool {
	return p.halted
}

// HaltError returns the error that caused parsing to halt.
func (p *StreamParser) HaltError() error {
	return p.haltError
}

// processBuffer splits buffered content into lines and processes complete ones.
func (p *StreamParser) processBuffer() error {
	content := p.buffer.String()
	lines := strings.Split(content, "\n")

	// Keep last incomplete line in buffer
	if len(lines) > 0 && !strings.HasSuffix(content, "\n") {
		p.buffer.Reset()
		p.buffer.WriteString(lines[len(lines)-1])
		lines = lines[:len(lines)-1]
	} else {
		p.buffer.Reset()
	}

	// Add complete lines to queue
	p.lineBuffer = append(p.lineBuffer, lines...)

	// Process complete lines
	for len(p.lineBuffer) > 0 {
		if err := p.processLine(p.lineBuffer[0]); err != nil {
			return err
		}
		p.lineBuffer = p.lineBuffer[1:]
	}

	return nil
}

// processLine processes a single line based on current state.
func (p *StreamParser) processLine(line string) error {
	trimmed := strings.TrimSpace(line)

	switch p.state {
	case StateIdle:
		return p.processIdle(line, trimmed)
	case StateInFile:
		return p.processInFile(line, trimmed)
	case StateInSearch:
		return p.processInSearch(line, trimmed)
	case StateInReplace:
		return p.processInReplace(line, trimmed)
	case StateInDiff:
		return p.processInDiff(line, trimmed)
	case StateInAbout:
		return p.processInAbout(line, trimmed)
	}

	return nil
}

func (p *StreamParser) processIdle(line, trimmed string) error {
	if strings.HasPrefix(trimmed, "[FILE:") && strings.HasSuffix(trimmed, "]") {
		// Extract path
		path := strings.TrimPrefix(trimmed, "[FILE:")
		path = strings.TrimSuffix(path, "]")
		path = strings.TrimSpace(path)

		p.currentFile = path
		p.currentFormat = FormatUnknown
		p.currentSearch.Reset()
		p.currentReplace.Reset()
		p.currentDiff = nil
		p.fileEdits = nil
		p.state = StateInFile

		if p.callbacks.OnFileStart != nil {
			p.callbacks.OnFileStart(path)
		}
		return nil
	}

	// Direct unified diff format (--- path/to/file)
	if strings.HasPrefix(trimmed, "---") && !strings.HasPrefix(trimmed, "----") {
		// Extract path from "--- path" or "--- a/path"
		path := strings.TrimPrefix(trimmed, "---")
		path = strings.TrimSpace(path)
		path = strings.TrimPrefix(path, "a/") // git-style prefix

		if path != "" {
			p.currentFile = path
			p.currentFormat = FormatUnifiedDiff
			p.currentSearch.Reset()
			p.currentReplace.Reset()
			p.currentDiff = []string{line}
			p.fileEdits = nil
			p.state = StateInDiff

			if p.callbacks.OnFileStart != nil {
				p.callbacks.OnFileStart(path)
			}
		}
		return nil
	}

	if trimmed == "[ABOUT]" {
		p.aboutContent.Reset()
		p.state = StateInAbout
		return nil
	}

	// Pass through other text
	if p.callbacks.OnText != nil && trimmed != "" {
		p.callbacks.OnText(line)
	}

	return nil
}

func (p *StreamParser) processInFile(line, trimmed string) error {
	// Detect format from first significant line
	if trimmed == "<<<<<<< SEARCH" {
		p.currentFormat = FormatSearchReplace
		p.currentSearch.Reset()
		p.state = StateInSearch
		return nil
	}

	if strings.HasPrefix(trimmed, "---") {
		p.currentFormat = FormatUnifiedDiff
		p.currentDiff = []string{line}
		p.state = StateInDiff
		return nil
	}

	if trimmed == "[/FILE]" {
		// Empty file block
		if p.callbacks.OnFileComplete != nil {
			p.callbacks.OnFileComplete(FileEdit{
				Path:   p.currentFile,
				Format: p.currentFormat,
			})
		}
		p.state = StateIdle
		return nil
	}

	// Handle [ABOUT] appearing without [/FILE] (LLM forgot to close file block)
	if trimmed == "[ABOUT]" {
		// Complete the file block first
		if p.callbacks.OnFileComplete != nil {
			p.callbacks.OnFileComplete(FileEdit{
				Path:   p.currentFile,
				Format: p.currentFormat,
			})
		}
		// Then enter about state
		p.aboutContent.Reset()
		p.state = StateInAbout
		return nil
	}

	return nil
}

func (p *StreamParser) processInSearch(line, trimmed string) error {
	if trimmed == "=======" {
		// Search block complete - validate now
		search := p.currentSearch.String()
		// Remove leading/trailing newlines
		search = strings.Trim(search, "\n")

		if p.callbacks.OnSearchReady != nil {
			if err := p.callbacks.OnSearchReady(p.currentFile, search); err != nil {
				p.halted = true
				p.haltError = err
				return err
			}
		}

		p.currentReplace.Reset()
		p.state = StateInReplace
		return nil
	}

	// Skip leading empty lines
	if p.currentSearch.Len() == 0 && trimmed == "" {
		return nil
	}

	p.currentSearch.WriteString(line)
	p.currentSearch.WriteString("\n")
	return nil
}

func (p *StreamParser) processInReplace(line, trimmed string) error {
	if trimmed == ">>>>>>> REPLACE" {
		// Replace block complete
		search := strings.Trim(p.currentSearch.String(), "\n")
		replace := strings.Trim(p.currentReplace.String(), "\n")

		if p.callbacks.OnReplaceReady != nil {
			p.callbacks.OnReplaceReady(p.currentFile, search, replace)
		}

		p.fileEdits = append(p.fileEdits, SearchReplaceEdit{
			Search:  search,
			Replace: replace,
		})

		// Go back to in-file state to look for more edits
		p.state = StateInFile
		return nil
	}

	// Skip leading empty lines
	if p.currentReplace.Len() == 0 && trimmed == "" {
		return nil
	}

	p.currentReplace.WriteString(line)
	p.currentReplace.WriteString("\n")
	return nil
}

func (p *StreamParser) processInDiff(line, trimmed string) error {
	// End of diff: [/FILE] for backward compat, or [ABOUT] section
	if trimmed == "[/FILE]" || trimmed == "[ABOUT]" {
		// Diff complete - validate
		if p.callbacks.OnDiffReady != nil {
			if err := p.callbacks.OnDiffReady(p.currentFile, p.currentDiff); err != nil {
				p.halted = true
				p.haltError = err
				return err
			}
		}

		if p.callbacks.OnFileComplete != nil {
			p.callbacks.OnFileComplete(FileEdit{
				Path:             p.currentFile,
				Format:           FormatUnifiedDiff,
				UnifiedDiffLines: p.currentDiff,
			})
		}

		p.state = StateIdle

		// If [ABOUT], process it
		if trimmed == "[ABOUT]" {
			p.aboutContent.Reset()
			p.state = StateInAbout
		}
		return nil
	}

	// New file diff starts (--- path) - finish current and start new
	if strings.HasPrefix(trimmed, "---") && !strings.HasPrefix(trimmed, "----") {
		// Check if this looks like a diff header (has a path after ---)
		potentialPath := strings.TrimSpace(strings.TrimPrefix(trimmed, "---"))
		potentialPath = strings.TrimPrefix(potentialPath, "a/")

		if potentialPath != "" && !strings.HasPrefix(potentialPath, "-") {
			// This is a new file diff - finish current one
			if p.callbacks.OnDiffReady != nil {
				if err := p.callbacks.OnDiffReady(p.currentFile, p.currentDiff); err != nil {
					p.halted = true
					p.haltError = err
					return err
				}
			}

			if p.callbacks.OnFileComplete != nil {
				p.callbacks.OnFileComplete(FileEdit{
					Path:             p.currentFile,
					Format:           FormatUnifiedDiff,
					UnifiedDiffLines: p.currentDiff,
				})
			}

			// Start new file diff
			p.currentFile = potentialPath
			p.currentDiff = []string{line}

			if p.callbacks.OnFileStart != nil {
				p.callbacks.OnFileStart(potentialPath)
			}
			return nil
		}
	}

	// Another search/replace block inside the file (mixed format not supported, but handle gracefully)
	if trimmed == "<<<<<<< SEARCH" {
		// First finish current diff
		if len(p.currentDiff) > 0 && p.callbacks.OnDiffReady != nil {
			if err := p.callbacks.OnDiffReady(p.currentFile, p.currentDiff); err != nil {
				p.halted = true
				p.haltError = err
				return err
			}
		}
		p.currentFormat = FormatSearchReplace
		p.currentSearch.Reset()
		p.state = StateInSearch
		return nil
	}

	p.currentDiff = append(p.currentDiff, line)
	return nil
}

func (p *StreamParser) processInAbout(line, trimmed string) error {
	if trimmed == "[/ABOUT]" {
		if p.callbacks.OnAboutComplete != nil {
			p.callbacks.OnAboutComplete(strings.TrimSpace(p.aboutContent.String()))
		}
		p.state = StateIdle
		return nil
	}

	p.aboutContent.WriteString(line)
	p.aboutContent.WriteString("\n")

	if p.callbacks.OnAboutLine != nil {
		p.callbacks.OnAboutLine(line)
	}

	return nil
}

// halt stops parsing with the given error.
func (p *StreamParser) halt(err error) {
	p.halted = true
	p.haltError = err
}

// Reset resets the parser to initial state.
func (p *StreamParser) Reset() {
	p.state = StateIdle
	p.buffer.Reset()
	p.lineBuffer = nil
	p.currentFile = ""
	p.currentFormat = FormatUnknown
	p.currentSearch.Reset()
	p.currentReplace.Reset()
	p.currentDiff = nil
	p.fileEdits = nil
	p.aboutContent.Reset()
	p.halted = false
	p.haltError = nil
}

// PartialSearch returns the search content accumulated so far (for error reporting).
func (p *StreamParser) PartialSearch() string {
	return p.currentSearch.String()
}

// PartialReplace returns the replace content accumulated so far.
func (p *StreamParser) PartialReplace() string {
	return p.currentReplace.String()
}

// PartialDiff returns the diff lines accumulated so far.
func (p *StreamParser) PartialDiff() []string {
	return p.currentDiff
}

// ParseError represents an error during parsing with context.
type ParseError struct {
	FilePath    string
	Format      Format
	Search      string // For search/replace format
	DiffContext string // For unified diff format
	Reason      string
}

func (e *ParseError) Error() string {
	if e.Format == FormatSearchReplace {
		return fmt.Sprintf("edit failed for %s: %s\nsearch: %s",
			e.FilePath, e.Reason, truncateForError(e.Search, 100))
	}
	return fmt.Sprintf("edit failed for %s: %s", e.FilePath, e.Reason)
}

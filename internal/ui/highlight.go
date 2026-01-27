package ui

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/mattn/go-runewidth"
)

// highlighterCache caches highlighters by file path to avoid expensive lexer matching
var (
	highlighterCache   = make(map[string]*Highlighter)
	highlighterCacheMu sync.RWMutex
)

// Highlighter handles syntax highlighting for diff display
type Highlighter struct {
	lexer chroma.Lexer
	style *chroma.Style
}

// NewHighlighter creates a highlighter for the given file path.
// Returns nil if the language is not recognized.
// Results are cached since lexers.Match is expensive (iterates all ~500 lexers).
func NewHighlighter(filePath string) *Highlighter {
	// Check cache first (fast path with read lock)
	highlighterCacheMu.RLock()
	if h, ok := highlighterCache[filePath]; ok {
		highlighterCacheMu.RUnlock()
		return h
	}
	highlighterCacheMu.RUnlock()

	// Cache miss - do expensive lexer matching
	lexer := lexers.Match(filePath)
	if lexer == nil {
		// Cache nil result too to avoid repeated lookups
		highlighterCacheMu.Lock()
		highlighterCache[filePath] = nil
		highlighterCacheMu.Unlock()
		return nil
	}
	lexer = chroma.Coalesce(lexer)

	// Use monokai theme - good contrast on dark backgrounds
	style := styles.Get("monokai")
	if style == nil {
		style = styles.Fallback
	}

	h := &Highlighter{
		lexer: lexer,
		style: style,
	}

	// Store in cache
	highlighterCacheMu.Lock()
	highlighterCache[filePath] = h
	highlighterCacheMu.Unlock()

	return h
}

// HighlightLine applies syntax highlighting to a line without a background color.
func (h *Highlighter) HighlightLine(line string) string {
	if h == nil {
		return line
	}

	iterator, err := h.lexer.Tokenise(nil, line)
	if err != nil {
		return line
	}

	var buf strings.Builder
	formatter := &noBgFormatter{style: h.style}
	err = formatter.Format(&buf, iterator)
	if err != nil {
		return line
	}

	return buf.String()
}

// HighlightLineWithBg applies syntax highlighting to a line with a specific background color.
// bg is an RGB array [r, g, b] for true color background.
func (h *Highlighter) HighlightLineWithBg(line string, bg [3]int) string {
	if h == nil {
		return line
	}

	iterator, err := h.lexer.Tokenise(nil, line)
	if err != nil {
		return line
	}

	// Format with our custom formatter that includes background
	var buf strings.Builder
	formatter := &bgFormatter{bg: bg, style: h.style}
	err = formatter.Format(&buf, iterator)
	if err != nil {
		return line
	}

	return buf.String()
}

// noBgFormatter is a Chroma formatter that applies only foreground colors
type noBgFormatter struct {
	style *chroma.Style
}

func (f *noBgFormatter) Format(w io.Writer, iterator chroma.Iterator) error {
	for token := iterator(); token != chroma.EOF; token = iterator() {
		value := strings.TrimRight(token.Value, "\n")
		if value == "" {
			continue
		}

		entry := f.style.Get(token.Type)

		var codes []string

		if entry.Colour.IsSet() {
			codes = append(codes, fmt.Sprintf("38;2;%d;%d;%d", entry.Colour.Red(), entry.Colour.Green(), entry.Colour.Blue()))
		}
		if entry.Bold == chroma.Yes {
			codes = append(codes, "1")
		}
		if entry.Italic == chroma.Yes {
			codes = append(codes, "3")
		}
		if entry.Underline == chroma.Yes {
			codes = append(codes, "4")
		}

		if len(codes) > 0 {
			fmt.Fprintf(w, "\x1b[%sm%s\x1b[0m", strings.Join(codes, ";"), value)
		} else {
			fmt.Fprint(w, value)
		}
	}
	return nil
}

// bgFormatter is a custom Chroma formatter that applies a consistent background color
type bgFormatter struct {
	bg    [3]int // RGB background color
	style *chroma.Style
}

func (f *bgFormatter) Format(w io.Writer, iterator chroma.Iterator) error {
	for token := iterator(); token != chroma.EOF; token = iterator() {
		// Skip newlines - lexers may produce trailing newline tokens
		// which would create phantom lines when combined with fmt.Println
		value := strings.TrimRight(token.Value, "\n")
		if value == "" {
			continue
		}

		entry := f.style.Get(token.Type)

		// Build ANSI sequence for this token
		var codes []string

		// Always set background (true color)
		codes = append(codes, fmt.Sprintf("48;2;%d;%d;%d", f.bg[0], f.bg[1], f.bg[2]))

		// Set foreground color if defined (true color)
		if entry.Colour.IsSet() {
			codes = append(codes, fmt.Sprintf("38;2;%d;%d;%d", entry.Colour.Red(), entry.Colour.Green(), entry.Colour.Blue()))
		}

		// Bold
		if entry.Bold == chroma.Yes {
			codes = append(codes, "1")
		}

		// Italic
		if entry.Italic == chroma.Yes {
			codes = append(codes, "3")
		}

		// Underline
		if entry.Underline == chroma.Yes {
			codes = append(codes, "4")
		}

		// Write the styled token
		fmt.Fprintf(w, "\x1b[%sm%s", strings.Join(codes, ";"), value)
	}

	// Reset at end
	fmt.Fprint(w, "\x1b[0m")
	return nil
}

const tabWidth = 8

func advanceColumn(col int, r rune) int {
	switch r {
	case '\t':
		if tabWidth <= 0 {
			return col
		}
		return col + (tabWidth - (col % tabWidth))
	case '\n':
		return 0
	}

	width := runewidth.RuneWidth(r)
	if width < 0 {
		width = 0
	}
	return col + width
}

func ansiDisplayWidth(s string, startCol int) int {
	col := startCol
	inEscape := false

	for i := 0; i < len(s); {
		b := s[i]
		if b == '\x1b' {
			inEscape = true
			i++
			continue
		}
		if inEscape {
			if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') {
				inEscape = false
			}
			i++
			continue
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			col++
			i++
			continue
		}

		col = advanceColumn(col, r)
		i += size
	}

	if col < startCol {
		return 0
	}
	return col - startCol
}

// ANSI escape code pattern for stripping/measuring
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// StripANSI removes all ANSI escape codes from a string
func StripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}

// ANSILen returns the display width of a string, ignoring ANSI codes
func ANSILen(s string) int {
	return ansiDisplayWidth(s, 0)
}

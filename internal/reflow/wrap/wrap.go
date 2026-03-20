package wrap

import (
	"bytes"
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"
	"github.com/muesli/reflow/ansi"
)

var (
	defaultNewline  = []rune{'\n'}
	defaultTabWidth = 4
)

type Wrap struct {
	Limit         int
	Newline       []rune
	KeepNewlines  bool
	PreserveSpace bool
	TabWidth      int

	buf             *bytes.Buffer
	lineLen         int
	parser          ansi.Parser
	forcefulNewline bool
}

// NewWriter returns a new instance of a wrapping writer, initialized with
// default settings.
func NewWriter(limit int) *Wrap {
	return &Wrap{
		Limit:         limit,
		Newline:       defaultNewline,
		KeepNewlines:  true,
		PreserveSpace: false,
		TabWidth:      defaultTabWidth,

		buf: &bytes.Buffer{},
	}
}

// Bytes is shorthand for declaring a new default Wrap instance,
// used to immediately wrap a byte slice.
func Bytes(b []byte, limit int) []byte {
	f := NewWriter(limit)
	_, _ = f.Write(b)

	return f.buf.Bytes()
}

func (w *Wrap) addNewLine() {
	_, _ = w.buf.WriteRune('\n')
	w.lineLen = 0
}

// String is shorthand for declaring a new default Wrap instance,
// used to immediately wrap a string.
func String(s string, limit int) string {
	return string(Bytes([]byte(s), limit))
}

func (w *Wrap) Write(b []byte) (int, error) {
	s := strings.Replace(string(b), "\t", strings.Repeat(" ", w.TabWidth), -1)
	if !w.KeepNewlines {
		s = strings.Replace(s, "\n", "", -1)
	}

	if w.Limit <= 0 {
		_, _ = w.buf.WriteString(s)
		return len(b), nil
	}

	width := ansi.PrintableRuneWidth(s)
	if !w.parser.InSequence() && w.lineLen+width <= w.Limit && !strings.ContainsRune(s, '\n') {
		w.lineLen += width
		_, _ = w.buf.WriteString(s)
		return len(b), nil
	}

	for _, c := range s {
		inSeq := w.parser.Feed(c)

		if inSeq {
			// Part of an escape sequence — write without width accounting.
		} else if inGroup(w.Newline, c) {
			w.addNewLine()
			w.forcefulNewline = false
			continue
		} else {
			width := runewidth.RuneWidth(c)

			if w.lineLen+width > w.Limit {
				w.addNewLine()
				w.forcefulNewline = true
			}

			if w.lineLen == 0 {
				if w.forcefulNewline && !w.PreserveSpace && unicode.IsSpace(c) {
					continue
				}
			} else {
				w.forcefulNewline = false
			}

			w.lineLen += width
		}

		_, _ = w.buf.WriteRune(c)
	}

	return len(b), nil
}

// Bytes returns the wrapped result as a byte slice.
func (w *Wrap) Bytes() []byte {
	return w.buf.Bytes()
}

// String returns the wrapped result as a string.
func (w *Wrap) String() string {
	return w.buf.String()
}

func inGroup(a []rune, c rune) bool {
	for _, v := range a {
		if v == c {
			return true
		}
	}
	return false
}

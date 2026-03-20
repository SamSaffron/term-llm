package truncate

import (
	"bytes"
	"io"

	"github.com/mattn/go-runewidth"

	"github.com/muesli/reflow/ansi"
)

type Writer struct {
	width     uint
	tail      string
	tailWidth uint

	ansiWriter *ansi.Writer
	buf        bytes.Buffer
	parser     ansi.Parser
	curWidth   uint
	truncated  bool
	safePos    int          // buf position where content fits within (width - tailWidth)
	ansiSeq    bytes.Buffer // current ANSI sequence being accumulated
	inOSC8     bool         // true while inside an OSC8 hyperlink
}

func NewWriter(width uint, tail string) *Writer {
	w := &Writer{
		width:     width,
		tail:      tail,
		tailWidth: uint(ansi.PrintableRuneWidth(tail)),
	}
	w.ansiWriter = &ansi.Writer{
		Forward: &w.buf,
	}
	return w
}

func NewWriterPipe(forward io.Writer, width uint, tail string) *Writer {
	return &Writer{
		width:     width,
		tail:      tail,
		tailWidth: uint(ansi.PrintableRuneWidth(tail)),
		ansiWriter: &ansi.Writer{
			Forward: forward,
		},
	}
}

// Bytes is shorthand for declaring a new default truncate-writer instance,
// used to immediately truncate a byte slice.
func Bytes(b []byte, width uint) []byte {
	return BytesWithTail(b, width, []byte(""))
}

// BytesWithTail is shorthand for declaring a new default truncate-writer instance,
// used to immediately truncate a byte slice. A tail is then added to the
// end of the byte slice.
func BytesWithTail(b []byte, width uint, tail []byte) []byte {
	f := NewWriter(width, string(tail))
	_, _ = f.Write(b)

	return f.Bytes()
}

// String is shorthand for declaring a new default truncate-writer instance,
// used to immediately truncate a string.
func String(s string, width uint) string {
	return StringWithTail(s, width, "")
}

// StringWithTail is shorthand for declaring a new default truncate-writer instance,
// used to immediately truncate a string. A tail is then added to the end of the
// string.
func StringWithTail(s string, width uint, tail string) string {
	return string(BytesWithTail([]byte(s), width, []byte(tail)))
}

// Write truncates content at the given printable cell width, leaving any
// ansi sequences intact.
func (w *Writer) Write(b []byte) (int, error) {
	if w.truncated {
		return len(b), nil
	}

	if w.width < w.tailWidth {
		w.truncated = true
		_, _ = w.buf.WriteString(w.tail)
		return len(b), nil
	}

	for _, c := range string(b) {
		wasNormal := !w.parser.InSequence()
		inSeq := w.parser.Feed(c)

		if inSeq {
			if wasNormal {
				w.ansiSeq.Reset()
			}
			_, _ = w.ansiSeq.WriteRune(c)

			if !w.parser.InSequence() {
				// Sequence just terminated — check for OSC8 open/close.
				seq := w.ansiSeq.Bytes()
				if len(seq) >= 5 && seq[0] == 0x1b && seq[1] == ']' && seq[2] == '8' && seq[3] == ';' {
					// OSC8: close if URI is empty (ESC]8;;BEL or ESC]8;;ESC\)
					if bytes.Equal(seq, []byte("\x1b]8;;\x07")) || bytes.Equal(seq, []byte("\x1b]8;;\x1b\\")) {
						w.inOSC8 = false
					} else {
						w.inOSC8 = true
					}
				}
			}
		} else {
			w.curWidth += uint(runewidth.RuneWidth(c))

			if w.curWidth > w.width {
				w.truncated = true
				w.buf.Truncate(w.safePos)
				_, _ = w.buf.WriteString(w.tail)
				if w.inOSC8 {
					_, _ = w.buf.WriteString("\x1b]8;;\x07")
				}
				if w.ansiWriter.LastSequence() != "" {
					w.ansiWriter.ResetAnsi()
				}
				return len(b), nil
			}
		}

		_, err := w.ansiWriter.Write([]byte(string(c)))
		if err != nil {
			return 0, err
		}

		if !inSeq && w.curWidth <= w.width-w.tailWidth {
			w.safePos = w.buf.Len()
		}
	}

	return len(b), nil
}

// Bytes returns the truncated result as a byte slice.
func (w *Writer) Bytes() []byte {
	return w.buf.Bytes()
}

// String returns the truncated result as a string.
func (w *Writer) String() string {
	return w.buf.String()
}

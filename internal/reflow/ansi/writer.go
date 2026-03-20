package ansi

import (
	"bytes"
	"io"
	"unicode/utf8"
)

type Writer struct {
	Forward io.Writer

	parser     Parser
	ansiseq    bytes.Buffer
	lastseq    bytes.Buffer
	seqchanged bool
	runeBuf    []byte
}

// Write is used to write content to the ANSI buffer.
func (w *Writer) Write(b []byte) (int, error) {
	for _, c := range string(b) {
		wasNormal := !w.parser.InSequence()
		inSeq := w.parser.Feed(c)

		if inSeq {
			if wasNormal {
				// Starting a new sequence.
				w.ansiseq.Reset()
				w.seqchanged = true
			}
			_, _ = w.ansiseq.WriteRune(c)

			if !w.parser.InSequence() {
				// Sequence just terminated — flush to Forward.
				if bytes.HasSuffix(w.ansiseq.Bytes(), []byte("[0m")) {
					w.lastseq.Reset()
					w.seqchanged = false
				} else if c == 'm' {
					_, _ = w.lastseq.Write(w.ansiseq.Bytes())
				}
				if _, err := w.ansiseq.WriteTo(w.Forward); err != nil {
					return 0, err
				}
			}
		} else {
			if _, err := w.writeRune(c); err != nil {
				return 0, err
			}
		}
	}

	return len(b), nil
}

func (w *Writer) writeRune(r rune) (int, error) {
	if w.runeBuf == nil {
		w.runeBuf = make([]byte, utf8.UTFMax)
	}
	n := utf8.EncodeRune(w.runeBuf, r)
	return w.Forward.Write(w.runeBuf[:n])
}

func (w *Writer) LastSequence() string {
	return w.lastseq.String()
}

// ResetAnsi writes an ANSI reset sequence to Forward if any style sequence
// has been written since the last reset. Errors are intentionally ignored
// because callers use this for best-effort cleanup at line boundaries.
func (w *Writer) ResetAnsi() {
	if !w.seqchanged {
		return
	}
	_, _ = w.Forward.Write([]byte("\x1b[0m"))
}

// RestoreAnsi replays the last tracked style sequence to Forward.
// Errors are intentionally ignored (best-effort restore at line boundaries).
func (w *Writer) RestoreAnsi() {
	_, _ = w.Forward.Write(w.lastseq.Bytes())
}

package margin

import (
	"bytes"
	"io"

	"github.com/muesli/reflow/indent"
	"github.com/muesli/reflow/padding"
)

type Writer struct {
	buf bytes.Buffer
	pw  *padding.Writer
	iw  *indent.Writer
}

func NewWriter(width uint, margin uint, marginFunc func(io.Writer)) *Writer {
	w := &Writer{}
	w.pw = padding.NewWriterPipe(&w.buf, width, marginFunc)
	w.iw = indent.NewWriterPipe(w.pw, margin, marginFunc)
	return w
}

// Bytes is shorthand for declaring a new default margin-writer instance,
// used to immediately apply a margin to a byte slice.
func Bytes(b []byte, width uint, margin uint) []byte {
	f := NewWriter(width, margin, nil)
	_, _ = f.Write(b)
	f.Close()

	return f.Bytes()
}

// String is shorthand for declaring a new default margin-writer instance,
// used to immediately apply a margin to a string.
func String(s string, width uint, margin uint) string {
	return string(Bytes([]byte(s), width, margin))
}

func (w *Writer) Write(b []byte) (int, error) {
	return w.iw.Write(b)
}

// Close will finish the margin operation. Always call it before trying to
// retrieve the final result.
func (w *Writer) Close() error {
	return w.pw.Close()
}

// Bytes returns the result as a byte slice.
func (w *Writer) Bytes() []byte {
	return w.buf.Bytes()
}

// String returns the result as a string.
func (w *Writer) String() string {
	return w.buf.String()
}

package stream

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// DecoderOptions configures a Decoder.
// RequireDone makes EOF without the terminal marker a StreamIncompleteError;
// Transport and Terminal customise that error message.
type DecoderOptions struct {
	RequireDone bool
	Transport   string
	Terminal    string
}

// Decoder reads server-sent events from a stream.
type Decoder struct {
	reader      *bufio.Reader
	requireDone bool
	transport   string
	terminal    string
	sawDone     bool
	closed      bool
	pendingErr  error
}

// NewDecoder returns a Decoder reading from r.
func NewDecoder(r io.Reader, opts DecoderOptions) *Decoder {
	reader, ok := r.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(r)
	}
	transport := opts.Transport
	if transport == "" {
		transport = "SSE"
	}
	terminal := opts.Terminal
	if terminal == "" {
		terminal = string(DoneData)
	}
	return &Decoder{
		reader:      reader,
		requireDone: opts.RequireDone,
		transport:   transport,
		terminal:    terminal,
	}
}

// DoneSeen reports whether the [DONE] marker was received.
func (d *Decoder) DoneSeen() bool {
	return d != nil && d.sawDone
}

// Next returns the next event, io.EOF at a clean end of stream, or a
// StreamIncompleteError when a required terminal marker is missing.
func (d *Decoder) Next() (eventType string, data []byte, err error) {
	if d.pendingErr != nil {
		err := d.pendingErr
		d.pendingErr = nil
		return "", nil, err
	}
	if d.closed {
		return "", nil, io.EOF
	}

	for {
		eventType, data, eof, err := ReadEventBytes(d.reader)
		if err != nil {
			return "", nil, err
		}
		if eof && eventType == "" && len(data) == 0 {
			d.closed = true
			if d.requireDone && !d.sawDone {
				return "", nil, d.missingDoneError()
			}
			return "", nil, io.EOF
		}
		if bytes.Equal(data, DoneData) {
			d.sawDone = true
			d.closed = true
			return "", nil, io.EOF
		}
		if len(bytes.TrimSpace(data)) == 0 {
			if eof {
				d.closed = true
				if d.requireDone && !d.sawDone {
					return "", nil, d.missingDoneError()
				}
				return "", nil, io.EOF
			}
			continue
		}
		if eof {
			d.closed = true
			if d.requireDone && !d.sawDone {
				d.pendingErr = d.missingDoneError()
			} else {
				d.pendingErr = io.EOF
			}
		}
		return eventType, data, nil
	}
}

func (d *Decoder) missingDoneError() error {
	return &StreamIncompleteError{Transport: d.transport, Terminal: d.terminal}
}

// ReadLine reads one SSE line, trimming the line terminator. eof reports that
// the input ended while reading it.
func ReadLine(reader *bufio.Reader) (line string, eof bool, err error) {
	lineBytes, eof, err := ReadLineBytes(reader)
	if err != nil {
		return "", false, err
	}
	return string(lineBytes), eof, nil
}

// ReadLineBytes is like ReadLine but returns bytes that may alias the reader's
// buffer and are valid only until the next read.
func ReadLineBytes(reader *bufio.Reader) (line []byte, eof bool, err error) {
	var owned []byte
	for {
		chunk, readErr := reader.ReadSlice('\n')
		if len(chunk) > 0 {
			if owned != nil {
				owned = append(owned, chunk...)
			} else if errors.Is(readErr, bufio.ErrBufferFull) {
				owned = append(owned, chunk...)
			} else {
				line = chunk
			}
		}

		switch {
		case readErr == nil:
			if owned != nil {
				line = owned
			}
			return bytes.TrimRight(line, "\r\n"), false, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF):
			if owned != nil {
				line = owned
			} else if len(chunk) > 0 {
				line = chunk
			}
			return bytes.TrimRight(line, "\r\n"), true, nil
		default:
			return nil, false, readErr
		}
	}
}

// Field names and the terminal data marker used in SSE streams.
var (
	EventField = []byte("event")
	DataField  = []byte("data")
	DoneData   = []byte("[DONE]")
)

// ReadEvent reads one complete SSE event (up to a blank line) and returns its
// event type and newline-joined data.
func ReadEvent(reader *bufio.Reader) (eventType, data string, eof bool, err error) {
	var dataBuilder strings.Builder
	dataLines := 0

	appendData := func(value []byte) {
		if dataLines == 0 {
			data = string(value)
			dataLines = 1
			return
		}
		if dataLines == 1 {
			dataBuilder.WriteString(data)
			data = ""
		}
		dataBuilder.WriteByte('\n')
		dataBuilder.Write(value)
		dataLines++
	}

	for {
		line, lineEOF, err := ReadLineBytes(reader)
		if err != nil {
			return "", "", false, err
		}
		if len(line) == 0 {
			if dataLines > 1 {
				data = dataBuilder.String()
			}
			return eventType, data, lineEOF, nil
		}
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value := line[:i], line[i+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			switch {
			case bytes.Equal(field, EventField):
				eventType = string(value)
			case bytes.Equal(field, DataField):
				appendData(value)
			}
		}
		if lineEOF {
			if dataLines > 1 {
				data = dataBuilder.String()
			}
			return eventType, data, true, nil
		}
	}
}

// ReadEventBytes is like ReadEvent but returns data as a byte slice.
func ReadEventBytes(reader *bufio.Reader) (eventType string, data []byte, eof bool, err error) {
	for {
		line, lineEOF, err := ReadLineBytes(reader)
		if err != nil {
			return "", nil, false, err
		}
		if len(line) == 0 {
			return eventType, data, lineEOF, nil
		}
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value := line[:i], line[i+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			switch {
			case bytes.Equal(field, EventField):
				eventType = string(value)
			case bytes.Equal(field, DataField):
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, value...)
			}
		}
		if lineEOF {
			return eventType, data, true, nil
		}
	}
}

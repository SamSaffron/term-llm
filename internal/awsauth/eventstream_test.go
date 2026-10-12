package awsauth

// Fixtures in testdata/ were generated with aws-sdk-go-v2 as an oracle (core
// v1.47.1, config v1.33.6, eventstream v1.7.20) before the dependency was removed.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type eventFixture struct {
	File     string
	Messages []struct {
		Headers []struct {
			Name  string
			Type  uint8
			Value json.RawMessage
		}
		Payload []byte
	}
	Error string
}

func TestEventStreamOracle(t *testing.T) {
	b, err := os.ReadFile("testdata/eventstream.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []eventFixture
	if err = json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.File, func(t *testing.T) {
			frame, err := os.ReadFile(filepath.Join("testdata", c.File))
			if err != nil {
				t.Fatal(err)
			}
			d := NewEventStreamDecoder(bytes.NewReader(frame))
			for _, want := range c.Messages {
				got, err := d.Next()
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got.Payload, want.Payload) || len(got.Headers) != len(want.Headers) {
					t.Fatalf("message = %#v", got)
				}
				for i, h := range got.Headers {
					w := want.Headers[i]
					if h.Name != w.Name || h.Type != w.Type {
						t.Errorf("header = %#v want %#v", h, w)
					}
					value := h.Value
					if uuid, ok := value.([16]byte); ok {
						value = uuid[:]
					}
					v, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(v, w.Value) {
						t.Errorf("%s value = %s want %s", h.Name, v, w.Value)
					}
				}
				if len(got.Headers) > 0 {
					if v, ok := got.Header(":event-type"); !ok || v != "chunk" {
						t.Errorf("event type = %q, %v", v, ok)
					}
				}
			}
			_, err = d.Next()
			if c.Error == "" {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("boundary error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.Error) {
				t.Fatalf("error = %v want %s", err, c.Error)
			}
			if c.Error == "unexpected EOF" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("not unexpected EOF: %v", err)
			}
		})
	}
}

func eventTestFrame(headers, payload []byte) []byte {
	frame := make([]byte, 16+len(headers)+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(frame)))
	binary.BigEndian.PutUint32(frame[4:], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:], crc32.ChecksumIEEE(frame[:8]))
	copy(frame[12:], headers)
	copy(frame[12+len(headers):], payload)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], crc32.ChecksumIEEE(frame[:len(frame)-4]))
	return frame
}

func TestEventStreamMalformedHeaders(t *testing.T) {
	for _, headers := range [][]byte{{2, 'x'}, {1, 'x'}, {1, 'x', 10}, {1, 'x', 3, 0}, {1, 'x', 7, 0, 2, 'a'}, {1, 'x', 9, 0}} {
		_, err := NewEventStreamDecoder(bytes.NewReader(eventTestFrame(headers, nil))).Next()
		if err == nil {
			t.Errorf("accepted %v", headers)
		}
	}
}

func TestEventStreamLengthBounds(t *testing.T) {
	cases := []struct{ total, headers uint32 }{{0, 0}, {15, 0}, {maxEventMessage + 1, 0}, {16, 1}, {maxEventHeaders + 17, maxEventHeaders + 1}, {maxEventPayload + 17, 0}}
	for _, c := range cases {
		frame := eventTestFrame(nil, nil)
		binary.BigEndian.PutUint32(frame, c.total)
		binary.BigEndian.PutUint32(frame[4:], c.headers)
		binary.BigEndian.PutUint32(frame[8:], crc32.ChecksumIEEE(frame[:8]))
		if _, err := NewEventStreamDecoder(bytes.NewReader(frame)).Next(); err == nil {
			t.Errorf("accepted %v", c)
		}
	}
}

func TestEventStreamHeader(t *testing.T) {
	m := EventStreamMessage{Headers: []EventStreamHeader{{Name: "text", Value: "hello"}, {Name: "bytes", Value: []byte("world")}, {Name: "int", Value: int32(4)}}}
	for _, c := range []struct {
		name, want string
		found      bool
	}{{"text", "hello", true}, {"bytes", "world", true}, {"int", "", false}, {"absent", "", false}} {
		got, ok := m.Header(c.name)
		if got != c.want || ok != c.found {
			t.Errorf("Header(%q) = %q,%v", c.name, got, ok)
		}
	}
}

func TestEventStreamNegativeTimestamp(t *testing.T) {
	headers := []byte{1, 't', 8, 255, 255, 255, 255, 255, 255, 255, 255}
	m, err := NewEventStreamDecoder(bytes.NewReader(eventTestFrame(headers, nil))).Next()
	if err != nil {
		t.Fatal(err)
	}
	if !m.Headers[0].Value.(time.Time).Equal(time.UnixMilli(-1)) {
		t.Error(m.Headers[0].Value)
	}
}

func FuzzEventStreamDecoder(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.bin")
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		d := NewEventStreamDecoder(bytes.NewReader(b))
		// A frame is at least 16 bytes, so this also detects accidental no-progress loops.
		for i := 0; i <= len(b)/16+1; i++ {
			if _, err := d.Next(); err != nil {
				return
			}
		}
		t.Fatal("decoder failed to make progress")
	})
}

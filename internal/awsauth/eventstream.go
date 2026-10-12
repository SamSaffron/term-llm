package awsauth

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"
)

// EventStreamHeader is a typed AWS event-stream header. Type is the wire type
// (0..9); Value is bool, int8, int16, int32, int64, []byte, string, time.Time,
// or [16]byte, respectively (types 0 and 1 both contain a bool).
type EventStreamHeader struct {
	Name  string
	Type  uint8
	Value any
}

// EventStreamMessage is one verified AWS event-stream frame.
type EventStreamMessage struct {
	Headers []EventStreamHeader
	Payload []byte
}

// Header returns the first matching string or byte-array header as a string.
func (m EventStreamMessage) Header(name string) (string, bool) {
	for _, h := range m.Headers {
		if h.Name != name {
			continue
		}
		switch v := h.Value.(type) {
		case string:
			return v, true
		case []byte:
			return string(v), true
		default:
			return "", false
		}
	}
	return "", false
}

// EventStreamDecoder reads and verifies frames without retaining prior payloads.
type EventStreamDecoder struct{ r io.Reader }

func NewEventStreamDecoder(r io.Reader) *EventStreamDecoder { return &EventStreamDecoder{r: r} }

// Bounds from the original SDK eventstream framing implementation: 16MiB
// payload and 128KiB headers. Enforce these before allocating untrusted lengths.
const (
	maxEventPayload = 16 * 1024 * 1024
	maxEventHeaders = 128 * 1024
	maxEventMessage = 16 + maxEventPayload + maxEventHeaders
)

// Next returns io.EOF only at a clean frame boundary. Incomplete frames wrap
// io.ErrUnexpectedEOF. CRC and length failures never return an unverified message.
func (d *EventStreamDecoder) Next() (EventStreamMessage, error) {
	var empty EventStreamMessage
	var prelude [12]byte
	if _, err := io.ReadFull(d.r, prelude[:]); err != nil {
		return empty, err
	}
	if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:]) {
		return empty, errors.New("event stream: prelude CRC mismatch")
	}
	total, headers := binary.BigEndian.Uint32(prelude[:4]), binary.BigEndian.Uint32(prelude[4:8])
	if err := validateEventLengths(total, headers); err != nil {
		return empty, err
	}
	frame := make([]byte, int(total))
	copy(frame, prelude[:])
	if _, err := io.ReadFull(d.r, frame[12:]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return empty, fmt.Errorf("event stream: incomplete frame: %w", err)
	}
	if crc32.ChecksumIEEE(frame[:len(frame)-4]) != binary.BigEndian.Uint32(frame[len(frame)-4:]) {
		return empty, errors.New("event stream: message CRC mismatch")
	}
	decoded, err := decodeEventHeaders(frame[12 : 12+int(headers)])
	if err != nil {
		return empty, fmt.Errorf("event stream: headers: %w", err)
	}
	return EventStreamMessage{Headers: decoded, Payload: frame[12+int(headers) : len(frame)-4]}, nil
}

func validateEventLengths(total, headers uint32) error {
	if total < 16 || total > maxEventMessage {
		return fmt.Errorf("event stream: invalid message length %d (allowed 16..%d)", total, maxEventMessage)
	}
	if headers > maxEventHeaders || headers > total-16 {
		return fmt.Errorf("event stream: invalid headers length %d", headers)
	}
	if total-headers-16 > maxEventPayload {
		return errors.New("event stream: payload exceeds 16 MiB")
	}
	return nil
}

func decodeEventHeaders(data []byte) ([]EventStreamHeader, error) {
	r := bytes.NewReader(data)
	var result []EventStreamHeader
	for r.Len() > 0 {
		n, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		name := make([]byte, int(n))
		if err = readEventBytes(r, name); err != nil {
			return nil, err
		}
		typ, err := r.ReadByte()
		if err != nil {
			return nil, io.ErrUnexpectedEOF
		}
		value, err := decodeEventValue(r, typ)
		if err != nil {
			return nil, err
		}
		result = append(result, EventStreamHeader{Name: string(name), Type: typ, Value: value})
	}
	return result, nil
}

func readEventBytes(r io.Reader, b []byte) error {
	_, err := io.ReadFull(r, b)
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func decodeEventValue(r *bytes.Reader, typ uint8) (any, error) {
	if typ <= 1 {
		return typ == 0, nil
	}
	if typ == 6 || typ == 7 {
		return decodeEventVariable(r, typ)
	}
	sizes := map[uint8]int{2: 1, 3: 2, 4: 4, 5: 8, 8: 8, 9: 16}
	size, ok := sizes[typ]
	if !ok {
		return nil, fmt.Errorf("unknown header value type %d", typ)
	}
	var b [16]byte
	if err := readEventBytes(r, b[:size]); err != nil {
		return nil, err
	}
	switch typ {
	case 2:
		return int8(b[0]), nil
	case 3:
		return int16(binary.BigEndian.Uint16(b[:2])), nil
	case 4:
		return int32(binary.BigEndian.Uint32(b[:4])), nil
	case 5:
		return int64(binary.BigEndian.Uint64(b[:8])), nil
	case 8:
		return time.UnixMilli(int64(binary.BigEndian.Uint64(b[:8]))).UTC(), nil
	default:
		return b, nil // UUID (type 9)
	}
}

func decodeEventVariable(r *bytes.Reader, typ uint8) (any, error) {
	var size [2]byte
	if err := readEventBytes(r, size[:]); err != nil {
		return nil, err
	}
	b := make([]byte, int(binary.BigEndian.Uint16(size[:])))
	if err := readEventBytes(r, b); err != nil {
		return nil, err
	}
	if typ == 7 {
		return string(b), nil
	}
	return b, nil
}

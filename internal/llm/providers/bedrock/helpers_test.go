package bedrock

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func drainGoldenStream(t *testing.T, stream core.Stream) []core.Event {
	t.Helper()
	defer stream.Close()
	var events []core.Event
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			events = append(events, core.Event{Type: core.EventError, Err: err})
			return events
		}
		events = append(events, ev)
		if ev.Type == core.EventDone {
			return events
		}
	}
}

type goldenEventStreamHeader struct{ Name, Value string }

func encodeGoldenEventStreamFrame(headers []goldenEventStreamHeader, payload []byte) []byte {
	var hb bytes.Buffer
	for _, h := range headers {
		hb.WriteByte(byte(len(h.Name)))
		hb.WriteString(h.Name)
		hb.WriteByte(7) // string
		_ = binary.Write(&hb, binary.BigEndian, uint16(len(h.Value)))
		hb.WriteString(h.Value)
	}
	total := 12 + hb.Len() + len(payload) + 4
	var msg bytes.Buffer
	_ = binary.Write(&msg, binary.BigEndian, uint32(total))
	_ = binary.Write(&msg, binary.BigEndian, uint32(hb.Len()))
	_ = binary.Write(&msg, binary.BigEndian, crc32.ChecksumIEEE(msg.Bytes()[:8]))
	msg.Write(hb.Bytes())
	msg.Write(payload)
	_ = binary.Write(&msg, binary.BigEndian, crc32.ChecksumIEEE(msg.Bytes()))
	return msg.Bytes()
}

func goldenChunkFrame(data []byte) []byte {
	chunk, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString(data), "p": "abcdefgh"})
	return encodeGoldenEventStreamFrame([]goldenEventStreamHeader{
		{Name: ":event-type", Value: "chunk"},
		{Name: ":content-type", Value: "application/json"},
		{Name: ":message-type", Value: "event"},
	}, chunk)
}

// goldenRedirectTransport sends every request to the fixture server while
// preserving the original path, raw path, query, and headers.
type goldenRedirectTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (rt goldenRedirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = rt.target.Scheme
	clone.URL.Host = rt.target.Host
	clone.Host = rt.target.Host
	return rt.base.RoundTrip(clone)
}

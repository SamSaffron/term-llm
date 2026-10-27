package peer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestSendAdmissionIsContextCancellable(t *testing.T) {
	connection := &conn{
		limits:   (Limits{}).withDefaults(),
		sendGate: make(chan struct{}, 1),
		low:      make(chan struct{}, 1),
		closed:   make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := connection.send(ctx, &Frame{Type: TypeResponseBody, ID: "blocked"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("send waiting behind another writer = %v, want cancellation", err)
	}
}

func TestFrameWireFormat(t *testing.T) {
	f := &Frame{Type: TypeRequest, ID: "7", Method: "POST", Path: "/x", Header: http.Header{"Accept": {"a", "b"}}, Body: []byte("hi")}
	b, err := encodeFrame(f, DefaultMaxFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"version": float64(1), "type": "request", "id": "7", "method": "POST", "path": "/x",
		"header": map[string]any{"Accept": []any{"a", "b"}}, "body": "aGk=",
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatalf("wire = %s, want %s", gb, wb)
	}

	if _, err := decodeFrame([]byte(`{"version":2,"type":"request","id":"1"}`), DefaultMaxFrameBytes); err == nil {
		t.Fatal("accepted unsupported version")
	}
	if _, err := decodeFrame([]byte(`{"version":1,"type":"cancel","id":""}`), DefaultMaxFrameBytes); err == nil {
		t.Fatal("accepted empty id")
	}
	if _, err := encodeFrame(&Frame{Type: TypeResponseBody, ID: "1", Body: make([]byte, DefaultMaxFrameBytes)}, DefaultMaxFrameBytes); err == nil {
		t.Fatal("encoded oversized frame")
	}
}

func TestLimitsKeepBodiesInsideFrames(t *testing.T) {
	l := Limits{MaxFrameBytes: 8 << 10, MaxRequestBodyBytes: 1 << 20, BodyChunkBytes: 1 << 20}.withDefaults()
	for _, n := range []int{l.MaxRequestBodyBytes, l.BodyChunkBytes} {
		if _, err := encodeFrame(&Frame{Type: TypeResponseBody, ID: "123", Body: make([]byte, n)}, l.MaxFrameBytes); err != nil {
			t.Fatalf("%d-byte body does not fit: %v", n, err)
		}
	}
}

func TestCleanRequestPath(t *testing.T) {
	for _, ok := range []string{"/", "/api/v1/x?y=1&z=%20", "/v1/sessions/search?q=src%2Fmain.go", "/chat/sessions/abc?url=https%3A%2F%2Fexample.com%2Fa"} {
		if _, err := cleanRequestPath(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "x", "//h/x", "/a/./b", "/a/..", "/%2e%2e/", "/a%5Cb", "/a%00", "/a b", "/a#f", "/\x01"} {
		if _, err := cleanRequestPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// contractVectors are the frozen peer pilot v1 HTTP framing vectors.
var contractVectors = []string{
	`{"version":1,"type":"request","id":"v1-1","method":"POST","path":"/node/agent/chat/v1/responses","header":{"Content-Type":["application/json"],"Idempotency-Key":["vector-1"]},"body":"e30="}`,
	`{"version":1,"type":"response_start","id":"v1-1","status":200,"header":{"Content-Type":["text/event-stream"]}}`,
	`{"version":1,"type":"response_body","id":"v1-1","body":"ZGF0YToge30KCg=="}`,
	`{"version":1,"type":"response_end","id":"v1-1"}`,
	`{"version":1,"type":"cancel","id":"v1-1"}`,
}

func TestContractVectorsRoundTrip(t *testing.T) {
	types := []string{TypeRequest, TypeResponseStart, TypeResponseBody, TypeResponseEnd, TypeCancel}
	for i, v := range contractVectors {
		f, err := decodeFrame([]byte(v), DefaultMaxFrameBytes)
		if err != nil || f.Type != types[i] || f.ID != "v1-1" {
			t.Fatalf("vector %d: %+v, %v", i, f, err)
		}
		b, err := encodeFrame(f, DefaultMaxFrameBytes)
		if err != nil {
			t.Fatal(err)
		}
		var want, got any
		_ = json.Unmarshal([]byte(v), &want)
		_ = json.Unmarshal(b, &got)
		wb, _ := json.Marshal(want)
		gb, _ := json.Marshal(got)
		if string(wb) != string(gb) {
			t.Fatalf("vector %d re-encodes as %s, want %s", i, gb, wb)
		}
	}
	req, _ := decodeFrame([]byte(contractVectors[0]), DefaultMaxFrameBytes)
	body, _ := decodeFrame([]byte(contractVectors[2]), DefaultMaxFrameBytes)
	if string(req.Body) != "{}" || string(body.Body) != "data: {}\n\n" {
		t.Fatalf("decoded bodies %q %q", req.Body, body.Body)
	}
	if l := (Limits{}).withDefaults(); l.MaxFrameBytes != 65536 || l.BodyChunkBytes != 16384 ||
		l.MaxConcurrentStreams != 32 || l.MaxBufferedBytes != 1048576 {
		t.Fatalf("defaults drifted from contract: %+v", l)
	}
	if _, err := cleanRequestPath("/node/agent/chat/v1/responses"); err != nil {
		t.Fatal(err)
	}
}

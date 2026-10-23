package peerbroker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// rawWS registers with a plain gorilla client so tests can send arbitrary frames.
func rawWS(t *testing.T, srvURL, id, tok string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srvURL, "http") + "/v1/sessions/" + id + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(u, http.Header{"Authorization": {"Bearer " + tok}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var f Frame
	if err := ws.ReadJSON(&f); err != nil || f.Type != FrameReady {
		t.Fatalf("ready = %+v %v", f, err)
	}
	return ws
}

// expectClose drains frames until the server closes, asserting the close code
// and that at most maxErrors error frames arrived first (no infinite errors).
func expectClose(t *testing.T, ws *websocket.Conn, code, maxErrors int) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	errFrames := 0
	for {
		var f Frame
		err := ws.ReadJSON(&f)
		if err != nil {
			var ce *websocket.CloseError
			if !errors.As(err, &ce) || ce.Code != code {
				t.Fatalf("want close %d, got %v", code, err)
			}
			break
		}
		if f.Type == FrameError {
			errFrames++
		}
	}
	if errFrames > maxErrors {
		t.Fatalf("got %d error frames before close, want <= %d", errFrames, maxErrors)
	}
}

// flood writes frames until the server stops accepting them or n is reached.
func flood(ws *websocket.Conn, n int, write func() error) {
	_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < n; i++ {
		if write() != nil {
			return
		}
	}
}

func TestWSFrameFloodCloses(t *testing.T) {
	clk := newFakeClock() // frozen clock: buckets never refill
	_, c, srv := newTestBroker(t, Config{Now: clk.Now, WSFrameBurst: 5, MaxPendingMessages: 1000, MaxMessagesPerSession: 1000})
	s := mustCreate(t, c)
	ws := rawWS(t, srv.URL, s.ID, s.HostToken)
	flood(ws, 1000, func() error { return ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"signal","body":1}`)) })
	expectClose(t, ws, WSCloseRateLimited, 0)

	// Only the frames within the burst reached the mailbox.
	msgs, err := c.Receive(context.Background(), s.ID, s.DeviceToken, 0, 0)
	if err != nil || len(msgs) > 5 {
		t.Fatalf("delivered %d msgs, err %v", len(msgs), err)
	}
}

func TestWSControlFrameFloodCloses(t *testing.T) {
	clk := newFakeClock()
	_, c, srv := newTestBroker(t, Config{Now: clk.Now, WSFrameBurst: 5})
	s := mustCreate(t, c)
	ws := rawWS(t, srv.URL, s.ID, s.HostToken)
	ws.SetPongHandler(func(string) error { return nil })
	flood(ws, 1000, func() error { return ws.WriteControl(websocket.PingMessage, []byte("x"), time.Now().Add(time.Second)) })
	expectClose(t, ws, WSCloseRateLimited, 0)
}

func TestWSByteRateCloses(t *testing.T) {
	clk := newFakeClock()
	_, c, srv := newTestBroker(t, Config{Now: clk.Now, MaxMessageBytes: 1024, WSByteBurst: 1, MaxPendingMessages: 100})
	s := mustCreate(t, c)
	ws := rawWS(t, srv.URL, s.ID, s.HostToken)
	// Byte burst is raised to one maximal frame (1024+256); three ~1000-byte frames exceed it.
	frame := []byte(`{"type":"signal","body":"` + strings.Repeat("a", 1000) + `"}`)
	flood(ws, 3, func() error { return ws.WriteMessage(websocket.TextMessage, frame) })
	expectClose(t, ws, WSCloseRateLimited, 0)
}

func TestWSMalformedFramesCloseAfterViolations(t *testing.T) {
	_, c, srv := newTestBroker(t, Config{WSMaxViolations: 3})
	s := mustCreate(t, c)
	ws := rawWS(t, srv.URL, s.ID, s.HostToken)
	bad := [][]byte{[]byte(`not json`), []byte(`{"type":"bogus"}`), []byte(`{"type":"signal"}`), []byte(`{"type":"signal","body":null}`)}
	_ = ws.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3})
	for _, b := range bad {
		_ = ws.WriteMessage(websocket.TextMessage, b)
	}
	expectClose(t, ws, WSCloseViolation, 2)
}

func TestWSMailboxFullRepeatedCloses(t *testing.T) {
	_, c, srv := newTestBroker(t, Config{WSMaxViolations: 4, MaxPendingMessages: 1})
	s := mustCreate(t, c)
	ws := rawWS(t, srv.URL, s.ID, s.HostToken)
	flood(ws, 20, func() error { return ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"signal","body":1}`)) })
	expectClose(t, ws, WSCloseViolation, 3)
}

func TestWSAfterLifetimeQuotaCloses(t *testing.T) {
	_, c, srv := newTestBroker(t, Config{MaxMessagesPerSession: 2, MaxPendingMessages: 10})
	s := mustCreate(t, c)
	ws := rawWS(t, srv.URL, s.ID, s.HostToken)
	flood(ws, 50, func() error { return ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"signal","body":1}`)) })
	expectClose(t, ws, WSCloseQuotaExceeded, 0)

	// The quota is per session: HTTP is refused too, and the peer still gets
	// the two accepted messages.
	ctx := context.Background()
	if _, err := c.Send(ctx, s.ID, s.DeviceToken, json.RawMessage(`1`)); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("HTTP after quota: %v", err)
	}
	msgs, err := c.Receive(ctx, s.ID, s.DeviceToken, 0, 0)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("delivered = %v %v", msgs, err)
	}
}

func TestWSNormalSignallingWithinLimits(t *testing.T) {
	// Default limits must comfortably allow a trickle-ICE style burst.
	_, c, _ := newTestBroker(t, Config{})
	s := mustCreate(t, c)
	host := dial(t, c, s.ID, s.HostToken)
	dev := dial(t, c, s.ID, s.DeviceToken)
	const n = 24
	for i := 0; i < n; i++ {
		if err := host.Send(json.RawMessage(`{"candidate":"c"}`)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		recvType(t, dev, FrameSignal)
	}
	if err := dev.Send(json.RawMessage(`{"answer":"a"}`)); err != nil {
		t.Fatal(err)
	}
	recvType(t, host, FrameSignal)
}

// TestWSFragmentedContinuationFloodCloses sends one unfinished text message
// followed by endless empty continuation frames. These never complete a
// message, so only the raw wire budget can stop them.
func TestWSFragmentedContinuationFloodCloses(t *testing.T) {
	clk := newFakeClock() // frozen: no refill
	_, c, srv := newTestBroker(t, Config{Now: clk.Now, MaxMessageBytes: 1024})
	s := mustCreate(t, c)
	ws := rawWS(t, srv.URL, s.ID, s.HostToken)
	raw := ws.NetConn()

	mask := []byte{1, 2, 3, 4}
	start := append([]byte{0x01, 0x80}, mask...) // text, FIN=0, masked, len 0
	cont := append([]byte{0x00, 0x80}, mask...)  // continuation, FIN=0, len 0
	var buf []byte
	for len(buf) < 64<<10 {
		buf = append(buf, cont...)
	}
	writeErr := make(chan error, 1)
	go func() {
		_ = raw.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := raw.Write(start)
		for i := 0; i < 64 && err == nil; i++ { // up to 4 MiB, ~700k frames
			_, err = raw.Write(buf)
		}
		writeErr <- err
	}()
	expectClose(t, ws, WSCloseRateLimited, 0)
	if err := <-writeErr; err == nil {
		t.Fatal("server consumed the whole 4 MiB flood without closing")
	}
}

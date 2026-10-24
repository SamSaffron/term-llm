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

func dial(t *testing.T, c *Client, id, tok string) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := c.Dial(ctx, id, tok)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	return conn
}

func recvType(t *testing.T, c *Conn, typ string) *Frame {
	t.Helper()
	for {
		f, err := c.Recv()
		if err != nil {
			t.Fatalf("Recv waiting for %s: %v", typ, err)
		}
		if f.Type == typ {
			return f
		}
	}
}

func TestWSRegistrationAndSignalling(t *testing.T) {
	_, c, _ := newTestBroker(t, Config{})
	ctx := context.Background()
	s := mustCreate(t, c)

	// Host sends before the device registers: buffered in the mailbox.
	if _, err := c.Send(ctx, s.ID, s.HostToken, json.RawMessage(`{"offer":"x"}`)); err != nil {
		t.Fatal(err)
	}
	host := dial(t, c, s.ID, s.HostToken)
	if host.Ready.Role != "host" || host.Ready.PeerConnected == nil || *host.Ready.PeerConnected {
		t.Fatalf("host ready = %+v", host.Ready)
	}
	dev := dial(t, c, s.ID, s.DeviceToken)
	if dev.Ready.Role != "device" || !*dev.Ready.PeerConnected {
		t.Fatalf("device ready = %+v", dev.Ready)
	}
	if f := recvType(t, dev, FrameSignal); string(f.Body) != `{"offer":"x"}` || f.Seq != 1 {
		t.Fatalf("buffered signal = %+v", f)
	}
	if f := recvType(t, host, FramePeer); !*f.PeerConnected {
		t.Fatalf("presence = %+v", f)
	}
	if err := dev.Send(json.RawMessage(`{"answer":"y"}`)); err != nil {
		t.Fatal(err)
	}
	if f := recvType(t, host, FrameSignal); string(f.Body) != `{"answer":"y"}` {
		t.Fatalf("relayed = %+v", f)
	}
	// Delivered messages were acked: an HTTP receive sees nothing.
	if msgs, err := c.Receive(ctx, s.ID, s.DeviceToken, 0, 0); err != nil || len(msgs) != 0 {
		t.Fatalf("leftover = %v %v", msgs, err)
	}
	_ = dev.Close()
	if f := recvType(t, host, FramePeer); *f.PeerConnected {
		t.Fatalf("presence after close = %+v", f)
	}
}

func TestWSAuthAndSubprotocolToken(t *testing.T) {
	_, c, srv := newTestBroker(t, Config{})
	s := mustCreate(t, c)
	ctx := context.Background()
	if _, err := c.Dial(ctx, s.ID, "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad token: %v", err)
	}
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/sessions/" + s.ID + "/ws"
	d := websocket.Dialer{Subprotocols: []string{WSSubprotocol, WSBearerProtocolPrefix + s.DeviceToken}}
	ws, resp, err := d.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("browser-style dial: %v", err)
	}
	defer ws.Close()
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != WSSubprotocol {
		t.Fatalf("echoed subprotocol %q", got)
	}
	var f Frame
	if err := ws.ReadJSON(&f); err != nil || f.Role != "device" {
		t.Fatalf("ready = %+v %v", f, err)
	}
	if _, resp, err := (&websocket.Dialer{}).Dial(wsURL, nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous dial: %v", err)
	}
}

func TestWSSupersedeAndLimits(t *testing.T) {
	_, c, _ := newTestBroker(t, Config{MaxConnectsPerSession: 2, MaxMessageBytes: 32, MaxPendingMessages: 1})
	s := mustCreate(t, c)
	old := dial(t, c, s.ID, s.HostToken)
	_ = dial(t, c, s.ID, s.HostToken)
	var ce *CloseError
	for {
		_, err := old.Recv()
		if err != nil {
			if !errors.As(err, &ce) || ce.Code != WSCloseSuperseded {
				t.Fatalf("old conn err = %v", err)
			}
			break
		}
	}
	if _, err := c.Dial(context.Background(), s.ID, s.DeviceToken); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("connect quota: %v", err)
	}

	s2 := mustCreate(t, c)
	host := dial(t, c, s2.ID, s2.HostToken)
	if err := host.Send(json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if err := host.Send(json.RawMessage(`2`)); err != nil {
		t.Fatal(err)
	}
	if f := recvType(t, host, FrameError); f.Error != CodeMailboxFull {
		t.Fatalf("error frame = %+v", f)
	}
	// Oversized frames exceed the read limit and close the socket.
	if err := host.Send(json.RawMessage(`"` + strings.Repeat("a", 400) + `"`)); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := host.Recv(); err != nil {
			break
		}
	}
}

func TestWSClosedOnDeleteAndExpiry(t *testing.T) {
	_, c, _ := newTestBroker(t, Config{})
	ctx := context.Background()
	s := mustCreate(t, c)
	host := dial(t, c, s.ID, s.HostToken)
	if err := c.Delete(ctx, s.ID, s.DeviceToken); err != nil {
		t.Fatal(err)
	}
	var ce *CloseError
	for {
		if _, err := host.Recv(); err != nil {
			if !errors.As(err, &ce) || ce.Code != WSCloseSessionClosed {
				t.Fatalf("close = %v", err)
			}
			break
		}
	}

	clk := newFakeClock()
	_, c2, _ := newTestBroker(t, Config{Now: clk.Now})
	s2 := mustCreate(t, c2)
	clk.Advance(time.Hour)
	if _, err := c2.Dial(ctx, s2.ID, s2.HostToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired dial: %v", err)
	}
}

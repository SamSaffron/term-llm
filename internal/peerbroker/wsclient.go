package peerbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const wsClientReadLimit = 2 << 20

// Conn is a registered WebSocket signalling connection.
type Conn struct {
	ws    *websocket.Conn
	wmu   sync.Mutex
	Ready Frame // the server's ready frame (role, peer presence, expiry)
}

// Dial registers on the session's WebSocket endpoint using the bearer
// capability in the Authorization header and waits for the ready frame.
func (c *Client) Dial(ctx context.Context, sessionID, token string) (*Conn, error) {
	if !validSessionID(sessionID) {
		return nil, errors.New("peerbroker: invalid session ID")
	}
	u := *c.base
	u.Path = c.base.Path + sessionPath(sessionID, "/ws")
	u.RawPath = ""
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	d := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 10 * time.Second,
		Subprotocols:     []string{WSSubprotocol},
	}
	if t, ok := c.http.Transport.(*http.Transport); ok && t.TLSClientConfig != nil {
		d.TLSClientConfig = t.TLSClientConfig.Clone()
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	ws, resp, err := d.DialContext(ctx, u.String(), hdr)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			return nil, decodeAPIError(resp)
		}
		return nil, fmt.Errorf("peerbroker: dial websocket: %w", stripURLError(err))
	}
	ws.SetReadLimit(wsClientReadLimit)
	conn := &Conn{ws: ws}
	if dl, ok := ctx.Deadline(); ok {
		_ = ws.SetReadDeadline(dl)
	}
	f, err := conn.Recv()
	_ = ws.SetReadDeadline(time.Time{})
	if err != nil || f.Type != FrameReady {
		_ = ws.Close()
		return nil, errors.New("peerbroker: websocket registration failed")
	}
	conn.Ready = *f
	return conn, nil
}

// Send writes an opaque JSON signal for the peer. Delivery is asynchronous;
// limit violations arrive later as FrameError frames.
func (c *Conn) Send(body json.RawMessage) error {
	if !json.Valid(body) {
		return errors.New("peerbroker: message body must be valid JSON")
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if err := c.ws.WriteJSON(Frame{Type: FrameSignal, Body: body}); err != nil {
		return fmt.Errorf("peerbroker: send: %w", err)
	}
	return nil
}

// Recv returns the next server frame. Call from a single goroutine. A closed
// connection returns *CloseError carrying the WSClose* code when known.
func (c *Conn) Recv() (*Frame, error) {
	_, data, err := c.ws.ReadMessage()
	if err != nil {
		var ce *websocket.CloseError
		if errors.As(err, &ce) {
			return nil, &CloseError{Code: ce.Code, Reason: ce.Text}
		}
		return nil, fmt.Errorf("peerbroker: receive: %w", err)
	}
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("peerbroker: decode frame: %w", err)
	}
	return &f, nil
}

// SetReadDeadline bounds the next Recv.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.ws.SetReadDeadline(t) }

// Close closes the connection (the session stays open; use Client.Delete).
func (c *Conn) Close() error {
	c.wmu.Lock()
	_ = c.ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	c.wmu.Unlock()
	return c.ws.Close()
}

// CloseError reports a WebSocket close frame from the broker.
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("peerbroker: websocket closed (%d %s)", e.Code, strings.TrimSpace(e.Reason))
}

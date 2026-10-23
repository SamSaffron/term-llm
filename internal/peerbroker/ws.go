package peerbroker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocket registration/signalling.
//
// GET /v1/sessions/{id}/ws registers the caller's role (host or device) on the
// session. Authentication uses either "Authorization: Bearer <cap>" (native
// clients) or, for browsers that cannot set headers, the subprotocol list
// [WSSubprotocol, WSBearerProtocolPrefix+<cap>]. The server only ever echoes
// WSSubprotocol. One connection per role: a newer registration supersedes the
// older one (which is closed with WSCloseSuperseded).
//
// Frames are JSON text messages. Signal bodies are opaque and are routed
// through the same bounded mailbox used by the HTTP API, so a peer that is not
// yet connected receives them on registration. A message is removed from the
// mailbox only after it was written to the recipient's socket.
const (
	WSSubprotocol          = "peerbroker.v1"
	WSBearerProtocolPrefix = "peerbroker.bearer."

	// Application close codes.
	WSCloseSuperseded    = 4001
	WSCloseSessionClosed = 4002
	WSCloseExpired       = 4003
	WSCloseBrokerClosed  = 4004
	// WSCloseRateLimited: per-connection frame or byte rate exceeded.
	WSCloseRateLimited = 4005
	// WSCloseViolation: too many rejected frames on this connection.
	WSCloseViolation = 4006
	// WSCloseQuotaExceeded: the session's lifetime message quota is spent.
	WSCloseQuotaExceeded = 4007
)

// Frame types.
const (
	FrameReady  = "ready"  // server→client after registration
	FrameSignal = "signal" // both directions; Body is opaque JSON
	FramePeer   = "peer"   // server→client presence change
	FrameError  = "error"  // server→client non-fatal error (e.g. mailbox_full)
)

// Frame is one WebSocket message in either direction.
type Frame struct {
	Type          string          `json:"type"`
	Seq           uint64          `json:"seq,omitempty"`
	Body          json.RawMessage `json:"body,omitempty"`
	Role          string          `json:"role,omitempty"`
	PeerConnected *bool           `json:"peer_connected,omitempty"`
	ExpiresAt     *time.Time      `json:"expires_at,omitempty"`
	Error         string          `json:"error,omitempty"`
}

const (
	wsWriteTimeout  = 10 * time.Second
	wsPingInterval  = 20 * time.Second
	wsPongTimeout   = 45 * time.Second
	wsFrameOverhead = 256
	wsErrQueue      = 4
)

type wsConn struct {
	kick chan struct{} // closed when superseded by a newer registration
}

func (r role) String() string {
	if r == roleHost {
		return "host"
	}
	return "device"
}

func wsToken(r *http.Request) (string, bool) {
	if tok, ok := bearerToken(r); ok {
		return tok, true
	}
	for _, p := range websocket.Subprotocols(r) {
		if tok, ok := strings.CutPrefix(p, WSBearerProtocolPrefix); ok && tok != "" && len(tok) <= maxBearerLen {
			return tok, true
		}
	}
	return "", false
}

func (h *Handler) handleWS(w http.ResponseWriter, r *http.Request) {
	tok, ok := wsToken(r)
	h.mu.Lock()
	var s *session
	var me role
	if ok {
		s, me, ok = h.authorizeTokenLocked(r.PathValue("id"), tok)
	}
	switch {
	case !ok:
		h.mu.Unlock()
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "")
		return
	case h.wsConns >= h.cfg.MaxConnections:
		h.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, CodeTooManyConns, "")
		return
	case s.connects >= h.cfg.MaxConnectsPerSession:
		h.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, CodeConnectQuota, "")
		return
	}
	s.connects++
	h.wsConns++
	h.mu.Unlock()

	up := websocket.Upgrader{
		// Non-zero so gorilla reads through its own bufio.Reader over the
		// rawLimitedConn returned by Hijack (it would otherwise reuse the
		// server's buffered reader and bypass the raw byte budget).
		ReadBufferSize:  4 << 10,
		WriteBufferSize: 4 << 10,
		Subprotocols:    []string{WSSubprotocol},
		// Capability bearer auth, no cookies: cross-origin pages gain nothing
		// they do not already hold.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	conn, err := up.Upgrade(&rawLimitedWriter{ResponseWriter: w, budget: h.newRawBudget()}, r, nil)
	if err != nil {
		h.mu.Lock()
		h.wsConns--
		h.mu.Unlock()
		return
	}
	h.serveWS(conn, s, me)
}

func (h *Handler) serveWS(conn *websocket.Conn, s *session, me role) {
	wc := &wsConn{kick: make(chan struct{})}
	h.mu.Lock()
	if s.deleted {
		h.wsConns--
		h.mu.Unlock()
		closeWS(conn, WSCloseSessionClosed, CodeSessionClosed)
		return
	}
	if old := s.conns[me]; old != nil {
		close(old.kick)
	}
	s.conns[me] = wc
	ib := s.inbox[me]
	ib.waitGen++ // supersede HTTP long-polls on this mailbox
	ib.signalLocked()
	s.inbox[me.peer()].signalLocked() // presence change for the peer
	expiresAt := s.expiresAt
	remaining := expiresAt.Sub(h.cfg.Now())
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		if s.conns[me] == wc {
			s.conns[me] = nil
			if !s.deleted {
				s.inbox[me.peer()].signalLocked()
			}
		}
		h.wsConns--
		h.mu.Unlock()
		_ = conn.Close()
	}()

	errs := make(chan string, wsErrQueue)
	fatal := make(chan wsCloseReq, 1)
	readerDone := make(chan struct{})
	go h.readWS(conn, s, me, errs, fatal, readerDone)

	code, reason := h.writeWS(conn, s, me, wc, expiresAt, remaining, errs, fatal, readerDone)
	if code != 0 {
		closeWS(conn, code, reason)
	}
	_ = conn.Close() // unblock the reader
	<-readerDone
}

// wsCloseReq is a fatal reader verdict that the writer turns into a close frame.
type wsCloseReq struct {
	code   int
	reason string
}

// wsInbound enforces per-connection message-level limits: each data message
// (when its first frame header arrives, before its payload is read) and each
// ping/pong is charged to the frame bucket, and each assembled message payload
// is charged to the byte bucket before JSON decoding. These are message, not
// frame-header, quotas: continuation frames are absorbed by gorilla. The
// rawLimitedConn byte budget beneath gorilla bounds all wire input, including
// continuation-frame headers, so parser work per connection stays bounded.
type wsInbound struct {
	frames     *tokenBucket
	bytes      *tokenBucket
	violations int
	maxViol    int
}

var errWSRateLimited = errors.New("peerbroker: websocket input rate exceeded")

func (h *Handler) newWSInbound() *wsInbound {
	return &wsInbound{
		frames:  newTokenBucket(h.cfg.WSFramesPerSecond, h.cfg.WSFrameBurst, h.cfg.Now),
		bytes:   newTokenBucket(h.cfg.WSBytesPerSecond, h.cfg.WSByteBurst, h.cfg.Now),
		maxViol: h.cfg.WSMaxViolations,
	}
}

func (h *Handler) readWS(conn *websocket.Conn, s *session, me role, errs chan<- string, fatal chan<- wsCloseReq, done chan<- struct{}) {
	defer close(done)
	in := h.newWSInbound()
	stop := func(code int, reason string) {
		select {
		case fatal <- wsCloseReq{code, reason}:
		default:
		}
	}
	conn.SetReadLimit(int64(h.cfg.MaxMessageBytes) + wsFrameOverhead)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
	conn.SetPongHandler(func(string) error {
		if !in.frames.allow() {
			return errWSRateLimited
		}
		return conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
	})
	conn.SetPingHandler(func(data string) error {
		if !in.frames.allow() {
			return errWSRateLimited
		}
		err := conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(wsWriteTimeout))
		if err != nil && !errors.Is(err, websocket.ErrCloseSent) {
			return err
		}
		return nil
	})
	for {
		mt, r, err := conn.NextReader()
		if err != nil {
			switch {
			case errors.Is(err, errWSRateLimited):
				stop(WSCloseRateLimited, CodeRateLimited)
			case errors.Is(err, websocket.ErrReadLimit):
				stop(websocket.CloseMessageTooBig, CodeTooLarge)
			}
			return
		}
		if !in.frames.allow() {
			stop(WSCloseRateLimited, CodeRateLimited)
			return
		}
		data, err := io.ReadAll(r) // bounded by SetReadLimit and the raw budget
		if err != nil {
			switch {
			case errors.Is(err, errWSRateLimited):
				stop(WSCloseRateLimited, CodeRateLimited)
			case errors.Is(err, websocket.ErrReadLimit):
				stop(websocket.CloseMessageTooBig, CodeTooLarge)
			}
			return
		}
		if !in.bytes.allowN(float64(len(data))) {
			stop(WSCloseRateLimited, CodeRateLimited)
			return
		}
		code := h.handleWSFrame(s, me, mt, data)
		switch code {
		case "":
			continue
		case CodeMessageLimit, CodeUnauthorized:
			// Lifetime quota exhausted or session gone: nothing further can succeed.
			stop(WSCloseQuotaExceeded, code)
			return
		}
		in.violations++
		if in.violations >= in.maxViol {
			stop(WSCloseViolation, code)
			return
		}
		select {
		case errs <- code:
		default:
		}
	}
}

// handleWSFrame validates and enqueues one inbound data frame, returning an
// error code or "" on success.
func (h *Handler) handleWSFrame(s *session, me role, mt int, data []byte) string {
	if mt != websocket.TextMessage {
		return CodeBadRequest
	}
	var f Frame
	if json.Unmarshal(data, &f) != nil || f.Type != FrameSignal {
		return CodeBadRequest
	}
	body := bytes.TrimSpace(f.Body)
	if len(body) == 0 || bytes.Equal(body, []byte("null")) {
		return CodeBadRequest
	}
	if !h.limiter.allow() {
		return CodeRateLimited
	}
	h.mu.Lock()
	_, _, code := h.enqueueLocked(s, me, body)
	h.mu.Unlock()
	return code
}

// writeWS owns all data writes. It returns a close code (0 if the reader ended).
func (h *Handler) writeWS(conn *websocket.Conn, s *session, me role, wc *wsConn, expiresAt time.Time, remaining time.Duration, errs <-chan string, fatal <-chan wsCloseReq, readerDone <-chan struct{}) (int, string) {
	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()
	expiry := time.NewTimer(max(remaining, 0))
	defer expiry.Stop()

	h.mu.Lock()
	peerUp := s.conns[me.peer()] != nil
	h.mu.Unlock()
	exp := expiresAt.UTC()
	if writeFrame(conn, Frame{Type: FrameReady, Role: me.String(), PeerConnected: &peerUp, ExpiresAt: &exp}) != nil {
		return 0, ""
	}
	ib := s.inbox[me]
	for {
		h.mu.Lock()
		if s.deleted {
			h.mu.Unlock()
			return WSCloseSessionClosed, CodeSessionClosed
		}
		pending := ib.snapshotLocked()
		nowUp := s.conns[me.peer()] != nil
		changed := ib.changed
		h.mu.Unlock()

		for _, m := range pending {
			if writeFrame(conn, Frame{Type: FrameSignal, Seq: m.Seq, Body: m.Body}) != nil {
				return 0, ""
			}
		}
		if len(pending) > 0 {
			h.mu.Lock()
			ib.ackLocked(pending[len(pending)-1].Seq)
			h.mu.Unlock()
		}
		if nowUp != peerUp {
			peerUp = nowUp
			if writeFrame(conn, Frame{Type: FramePeer, PeerConnected: &peerUp}) != nil {
				return 0, ""
			}
		}

		select {
		case <-changed:
		case <-s.done:
		case <-wc.kick:
			return WSCloseSuperseded, "superseded"
		case <-h.closedCh:
			return WSCloseBrokerClosed, CodeBrokerClosed
		case <-expiry.C:
			return WSCloseExpired, "expired"
		case req := <-fatal:
			return req.code, req.reason
		case <-readerDone:
			select {
			case req := <-fatal:
				return req.code, req.reason
			default:
				return 0, ""
			}
		case code := <-errs:
			if writeFrame(conn, Frame{Type: FrameError, Error: code}) != nil {
				return 0, ""
			}
		case <-ping.C:
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteTimeout)) != nil {
				return 0, ""
			}
		}
	}
}

func writeFrame(conn *websocket.Conn, f Frame) error {
	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return conn.WriteJSON(f)
}

func closeWS(conn *websocket.Conn, code int, reason string) {
	msg := websocket.FormatCloseMessage(code, reason)
	_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
}

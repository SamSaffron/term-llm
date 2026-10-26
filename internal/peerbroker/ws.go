package peerbroker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	WSBearerProtocolPrefix = "peerbroker.v2.bearer."
	WSCloseSuperseded      = 4001
	WSCloseLeaseExpired    = 4002
	WSCloseBrokerClosed    = 4003
	WSCloseRateLimited     = 4004
	WSCloseQuotaExceeded   = 4005
	WSCloseViolation       = 4006
	wsWriteTimeout         = 10 * time.Second
	wsReadyTimeout         = 10 * time.Second
	wsPongTimeout          = 45 * time.Second
	wsPingInterval         = 20 * time.Second
)

type wsPeer struct {
	queue chan Frame
	kick  chan struct{}
}
type wsRole struct{ kind, id string }

func wsBearer(r *http.Request) (string, bool) {
	if tok, ok := bearer(r); ok {
		return tok, true
	}
	for _, protocol := range websocket.Subprotocols(r) {
		if tok, ok := strings.CutPrefix(protocol, WSBearerProtocolPrefix); ok && tok != "" && len(tok) <= maxCredentialLen && !strings.ContainsAny(tok, " \t\r\n") {
			return tok, true
		}
	}
	return "", false
}
func (h *Handler) handleWS(w http.ResponseWriter, r *http.Request) {
	tok, hasToken := wsBearer(r)
	kind, id, validQuery := requestPeer(r)
	hostID := r.PathValue("host")
	h.mu.Lock()
	host := h.hosts[hostID]
	status, code := h.peerEndpointErrorLocked(hostID, host, kind, id, tok, hasToken, validQuery)
	valid := code == ""
	if !valid {
		h.mu.Unlock()
		if code == "" {
			status, code = http.StatusUnauthorized, CodeUnauthorized
		}
		writeError(w, status, code, "")
		return
	}
	if h.connections >= h.cfg.MaxConnections {
		h.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, CodeQuotaExceeded, "")
		return
	}
	h.connections++
	h.mu.Unlock()
	upgrader := websocket.Upgrader{ReadBufferSize: 4 << 10, WriteBufferSize: 4 << 10, Subprotocols: []string{WSSubprotocol}, CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(&rawLimitedWriter{ResponseWriter: w, budget: h.newRawBudget()}, r, nil)
	if err != nil {
		h.mu.Lock()
		h.connections--
		h.mu.Unlock()
		return
	}
	h.serveWS(conn, hostID, wsRole{kind: kind, id: id}, tok)
}
func (h *Handler) serveWS(conn *websocket.Conn, hostID string, role wsRole, tok string) {
	peer := &wsPeer{queue: make(chan Frame, h.cfg.MaxPendingSignals), kick: make(chan struct{})}
	h.mu.Lock()
	host := h.hosts[hostID]
	if !h.peerAuthorizedLocked(host, role.kind, role.id, tok) {
		h.connections--
		h.mu.Unlock()
		closeWS(conn, WSCloseLeaseExpired, CodeExpired)
		return
	}
	switch role.kind {
	case PeerHost:
		if host.host != nil {
			close(host.host.kick)
		}
		host.host = peer
	case PeerDevice:
		slot := host.devices[role.id]
		if slot.peer != nil {
			close(slot.peer.kick)
		}
		slot.peer = peer
	case PeerInvitation:
		slot := host.invitations[role.id]
		if slot.peer != nil {
			close(slot.peer.kick)
			h.notifyPresenceLocked(host, role, false)
		}
		slot.consumed = true
		slot.peer = peer
	}
	expires := host.expiresAt.UTC()
	connected := role.kind == PeerHost && anyPeerLocked(host) || role.kind != PeerHost && host.host != nil
	h.notifyPresenceLocked(host, role, true)
	stop := host.stop
	h.mu.Unlock()
	ready := Frame{Type: FrameReady, Role: role.kind, PeerID: role.id, PeerKind: role.kind, PeerConnected: &connected, LeaseExpiresAt: &expires}
	if writeWSFrame(conn, ready) != nil {
		h.detachWS(hostID, role, peer)
		_ = conn.Close()
		return
	}
	code, reason := h.runWS(conn, hostID, role, peer, stop)
	if code != 0 {
		closeWS(conn, code, reason)
	}
	h.detachWS(hostID, role, peer)
	_ = conn.Close()
}
func anyPeerLocked(host *hostLease) bool {
	for _, d := range host.devices {
		if d.peer != nil {
			return true
		}
	}
	for _, i := range host.invitations {
		if i.peer != nil {
			return true
		}
	}
	return false
}
func (h *Handler) notifyPresenceLocked(host *hostLease, role wsRole, connected bool) {
	frame := Frame{Type: FramePeer, PeerID: role.id, PeerKind: role.kind, PeerConnected: &connected}
	if role.kind == PeerHost {
		frame.PeerID = ""
		for _, d := range host.devices {
			if d.peer != nil {
				enqueueFrame(d.peer, frame)
			}
		}
		for _, i := range host.invitations {
			if i.peer != nil {
				enqueueFrame(i.peer, frame)
			}
		}
		return
	}
	if host.host != nil {
		enqueueFrame(host.host, frame)
	}
}
func enqueueFrame(peer *wsPeer, frame Frame) bool {
	select {
	case peer.queue <- frame:
		return true
	default:
		return false
	}
}
func (h *Handler) runWS(conn *websocket.Conn, hostID string, role wsRole, peer *wsPeer, stop <-chan struct{}) (int, string) {
	readerDone := make(chan wsClose, 1)
	go h.readWS(conn, hostID, role, peer, readerDone)
	ping := time.NewTicker(wsPingInterval)
	leaseCheck := time.NewTicker(time.Second)
	defer ping.Stop()
	defer leaseCheck.Stop()
	for {
		select {
		case frame := <-peer.queue:
			if writeWSFrame(conn, frame) != nil {
				return 0, ""
			}
		case result := <-readerDone:
			return result.code, result.reason
		case <-peer.kick:
			return WSCloseSuperseded, "superseded"
		case <-stop:
			return WSCloseLeaseExpired, CodeExpired
		case <-h.closedCh:
			return WSCloseBrokerClosed, CodeBrokerClosed
		case <-leaseCheck.C:
			h.mu.Lock()
			host := h.hosts[hostID]
			expired := host == nil || !host.expiresAt.After(h.cfg.Now())
			h.mu.Unlock()
			if expired {
				return WSCloseLeaseExpired, CodeExpired
			}
		case <-ping.C:
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteTimeout)) != nil {
				return 0, ""
			}
		}
	}
}

var errWSRateLimited = errors.New("peerbroker: websocket wire rate exceeded")

type wsClose struct {
	code   int
	reason string
}

func (h *Handler) readWS(conn *websocket.Conn, hostID string, role wsRole, sender *wsPeer, done chan<- wsClose) {
	finish := func(code int, reason string) {
		select {
		case done <- wsClose{code, reason}:
		default:
		}
	}
	conn.SetReadLimit(int64(h.cfg.MaxSignalBytes + 1024))
	_ = conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(wsPongTimeout)) })
	limiter := newTokenBucket(h.cfg.SignalRate, h.cfg.SignalBurst, h.cfg.Now)
	count := 0
	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			if errors.Is(err, errWSRateLimited) {
				finish(WSCloseRateLimited, CodeRateLimited)
			} else {
				finish(0, "")
			}
			return
		}
		if !limiter.allow() {
			finish(WSCloseRateLimited, CodeRateLimited)
			return
		}
		count++
		if count > h.cfg.MaxSignalsPerConnection {
			finish(WSCloseQuotaExceeded, CodeQuotaExceeded)
			return
		}
		if messageType != websocket.TextMessage || len(data) > h.cfg.MaxSignalBytes+512 {
			finish(WSCloseViolation, CodeBadRequest)
			return
		}
		var frame Frame
		if json.Unmarshal(data, &frame) != nil || frame.Type != FrameSignal || !validSignalBody(frame.Body) {
			finish(WSCloseViolation, CodeBadRequest)
			return
		}
		if len(frame.Body) > h.cfg.MaxSignalBytes {
			finish(websocket.CloseMessageTooBig, CodeTooLarge)
			return
		}
		code := h.routeSignal(hostID, role, frame)
		if code == CodeQuotaExceeded {
			finish(WSCloseQuotaExceeded, code)
			return
		}
		if code != "" && !enqueueFrame(sender, Frame{Type: FrameError, PeerID: frame.PeerID, PeerKind: frame.PeerKind, Error: code}) {
			finish(WSCloseRateLimited, CodeQueueFull)
			return
		}
	}
}
func validSignalBody(body json.RawMessage) bool {
	trimmed := bytes.TrimSpace(body)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) && json.Valid(trimmed)
}
func (h *Handler) routeSignal(hostID string, role wsRole, frame Frame) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	host := h.hosts[hostID]
	if host == nil || !host.expiresAt.After(h.cfg.Now()) {
		return CodeExpired
	}
	if host.signals >= h.cfg.MaxSignalsPerLease {
		return CodeQuotaExceeded
	}
	host.signals++
	var target *wsPeer
	if role.kind == PeerHost {
		switch frame.PeerKind {
		case PeerDevice:
			if slot := host.devices[frame.PeerID]; slot != nil {
				target = slot.peer
			}
		case PeerInvitation:
			if slot := host.invitations[frame.PeerID]; slot != nil {
				target = slot.peer
			}
		default:
			return CodeBadRequest
		}
	} else {
		if frame.PeerID != "" && frame.PeerID != role.id || frame.PeerKind != "" && frame.PeerKind != role.kind {
			return CodeUnauthorized
		}
		frame.PeerID, frame.PeerKind, target = role.id, role.kind, host.host
	}
	if target == nil {
		return CodePeerOffline
	}
	frame.Type, frame.Role, frame.PeerConnected, frame.LeaseExpiresAt, frame.Error = FrameSignal, "", nil, nil, ""
	if !enqueueFrame(target, frame) {
		return CodeQueueFull
	}
	return ""
}
func (h *Handler) detachWS(hostID string, role wsRole, peer *wsPeer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connections--
	host := h.hosts[hostID]
	if host == nil {
		return
	}
	switch role.kind {
	case PeerHost:
		if host.host != peer {
			return
		}
		host.host = nil
	case PeerDevice:
		slot := host.devices[role.id]
		if slot == nil || slot.peer != peer {
			return
		}
		slot.peer = nil
	case PeerInvitation:
		slot := host.invitations[role.id]
		if slot == nil || slot.peer != peer {
			return
		}
		slot.peer = nil
	}
	h.notifyPresenceLocked(host, role, false)
}
func writeWSFrame(conn *websocket.Conn, frame Frame) error {
	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return conn.WriteJSON(frame)
}
func closeWS(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}

type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("peerbroker: websocket closed (%d %s)", e.Code, strings.TrimSpace(e.Reason))
}

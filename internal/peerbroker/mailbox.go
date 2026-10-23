package peerbroker

import (
	"bytes"
	"net/http"
	"strconv"
	"time"
)

type session struct {
	id         string
	hostHash   [32]byte
	deviceHash [32]byte
	expiresAt  time.Time
	inbox      [2]*inbox // indexed by recipient role
	sent       int
	turnMints  int
	connects   int        // WebSocket registrations attempted
	conns      [2]*wsConn // active WebSocket per role
	deleted    bool
	done       chan struct{}
}

type inbox struct {
	msgs    []Message
	nextSeq uint64
	changed chan struct{} // closed and replaced on every state change
	waitGen uint64        // newest receiver wins; older receivers return empty
}

func newSession(id string, hostHash, deviceHash [32]byte, expiresAt time.Time) *session {
	s := &session{id: id, hostHash: hostHash, deviceHash: deviceHash, expiresAt: expiresAt, done: make(chan struct{})}
	for i := range s.inbox {
		s.inbox[i] = &inbox{nextSeq: 1, changed: make(chan struct{})}
	}
	return s
}

func (ib *inbox) signalLocked() {
	close(ib.changed)
	ib.changed = make(chan struct{})
}

// ackLocked drops messages the receiver has confirmed (seq <= after).
func (ib *inbox) ackLocked(after uint64) {
	i := 0
	for i < len(ib.msgs) && ib.msgs[i].Seq <= after {
		i++
	}
	if i > 0 {
		ib.msgs = append([]Message(nil), ib.msgs[i:]...)
	}
}

func (ib *inbox) snapshotLocked() []Message {
	return append([]Message(nil), ib.msgs...)
}

func (h *Handler) handleSend(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	s, from, ok := h.authorizeLocked(r)
	h.mu.Unlock()
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "")
		return
	}
	var req sendRequest
	limit := int64(h.cfg.MaxMessageBytes) + sendBodyOverhead
	if err := decodeJSONBody(w, r, limit, &req, false); err != nil {
		writeDecodeError(w, err)
		return
	}
	body := bytes.TrimSpace(req.Body)
	if len(body) == 0 || bytes.Equal(body, []byte("null")) {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "body is required")
		return
	}

	h.mu.Lock()
	seq, status, code := h.enqueueLocked(s, from, body)
	h.mu.Unlock()
	if code != "" {
		writeError(w, status, code, "")
		return
	}
	writeJSON(w, http.StatusCreated, sendResponse{Seq: seq})
}

// enqueueLocked appends body to the peer's mailbox, enforcing the session and
// mailbox limits. It returns an HTTP status and error code on failure.
func (h *Handler) enqueueLocked(s *session, from role, body []byte) (uint64, int, string) {
	switch {
	case s.deleted:
		return 0, http.StatusUnauthorized, CodeUnauthorized
	case len(body) > h.cfg.MaxMessageBytes:
		return 0, http.StatusRequestEntityTooLarge, CodeTooLarge
	case s.sent >= h.cfg.MaxMessagesPerSession:
		return 0, http.StatusTooManyRequests, CodeMessageLimit
	}
	ib := s.inbox[from.peer()]
	if len(ib.msgs) >= h.cfg.MaxPendingMessages {
		return 0, http.StatusTooManyRequests, CodeMailboxFull
	}
	seq := ib.nextSeq
	ib.nextSeq++
	s.sent++
	ib.msgs = append(ib.msgs, Message{Seq: seq, Body: append([]byte(nil), body...)})
	ib.signalLocked()
	return seq, 0, ""
}

func (h *Handler) handleReceive(w http.ResponseWriter, r *http.Request) {
	after, wait, ok := h.parseReceiveQuery(r)
	if !ok {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid after or wait")
		return
	}
	h.mu.Lock()
	s, me, ok := h.authorizeLocked(r)
	if !ok {
		h.mu.Unlock()
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "")
		return
	}
	ib := s.inbox[me]
	ib.ackLocked(after)
	if len(ib.msgs) > 0 || wait == 0 {
		msgs := ib.snapshotLocked()
		h.mu.Unlock()
		writeJSON(w, http.StatusOK, receiveResponse{Messages: msgs})
		return
	}
	if h.waiters >= h.cfg.MaxWaiters {
		h.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, CodeTooManyWaiters, "")
		return
	}
	if remaining := s.expiresAt.Sub(h.cfg.Now()); remaining < wait {
		wait = max(remaining, 0)
	}
	h.waiters++
	ib.waitGen++
	gen := ib.waitGen
	ib.signalLocked() // supersede any older receiver for this mailbox
	msgs, closed := h.waitLocked(r, s, ib, gen, wait)
	h.waiters--
	h.mu.Unlock()
	if closed {
		writeError(w, http.StatusGone, CodeSessionClosed, "")
		return
	}
	writeJSON(w, http.StatusOK, receiveResponse{Messages: msgs})
}

// waitLocked blocks (releasing h.mu while parked) until messages arrive, the
// wait elapses, the request ends, a newer receiver supersedes it, or the
// session/broker closes. It returns with h.mu held.
func (h *Handler) waitLocked(r *http.Request, s *session, ib *inbox, gen uint64, wait time.Duration) ([]Message, bool) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		ch := ib.changed
		h.mu.Unlock()
		stop := false
		select {
		case <-ch:
		case <-s.done:
		case <-h.closedCh:
		case <-timer.C:
			stop = true
		case <-r.Context().Done():
			stop = true
		}
		h.mu.Lock()
		if s.deleted {
			return nil, true
		}
		if len(ib.msgs) > 0 && gen == ib.waitGen {
			return ib.snapshotLocked(), false
		}
		if stop || gen != ib.waitGen {
			return []Message{}, false
		}
	}
}

func (h *Handler) parseReceiveQuery(r *http.Request) (uint64, time.Duration, bool) {
	q := r.URL.Query()
	var after uint64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		after = n
	}
	var wait time.Duration
	if v := q.Get("wait"); v != "" {
		secs, err := strconv.ParseFloat(v, 64)
		if err != nil || !(secs >= 0 && secs <= hardMaxWait.Seconds()) { // also rejects NaN
			return 0, 0, false
		}
		wait = min(time.Duration(secs*float64(time.Second)), h.cfg.MaxWait)
	}
	return after, wait, true
}

package peerbroker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	sessionIDBytes   = 16
	capabilityBytes  = 32
	maxBearerLen     = 128
	maxCreateBody    = 1 << 10
	sweepInterval    = time.Second
	sendBodyOverhead = 64
)

type role int

const (
	roleHost role = iota
	roleDevice
)

func (r role) peer() role { return 1 - r }

// Handler is the broker's http.Handler. Create it with NewHandler and call
// Close on shutdown to release waiting receivers.
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	limiter *tokenBucket

	mu        sync.Mutex
	sessions  map[string]*session
	waiters   int
	wsConns   int
	closed    bool
	closedCh  chan struct{}
	lastSweep time.Time
}

// NewHandler validates cfg and returns a ready broker handler.
func NewHandler(cfg Config) (*Handler, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	h := &Handler{
		cfg:      cfg,
		limiter:  newTokenBucket(cfg.RatePerSecond, cfg.RateBurst, cfg.Now),
		sessions: make(map[string]*session),
		closedCh: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sessions", h.handleCreate)
	mux.HandleFunc("DELETE /v1/sessions/{id}", h.handleDelete)
	mux.HandleFunc("POST /v1/sessions/{id}/messages", h.handleSend)
	mux.HandleFunc("GET /v1/sessions/{id}/messages", h.handleReceive)
	mux.HandleFunc("POST /v1/sessions/{id}/turn", h.handleTURN)
	mux.HandleFunc("GET /v1/sessions/{id}/ws", h.handleWS)
	h.mux = mux
	return h, nil
}

// Close rejects new requests, wakes all waiting receivers and drops all sessions.
func (h *Handler) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	close(h.closedCh)
	for _, s := range h.sessions {
		h.removeLocked(s)
	}
}

// SessionCount reports the number of live (possibly not yet swept) sessions.
func (h *Handler) SessionCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}

// ServeHTTP applies common headers, closed-state and global rate checks, then routes.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	h.mu.Lock()
	closed := h.closed
	if !closed {
		h.maybeSweepLocked()
	}
	h.mu.Unlock()
	if closed {
		writeError(w, http.StatusServiceUnavailable, CodeBrokerClosed, "")
		return
	}
	if !h.limiter.allow() {
		hdr.Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, CodeRateLimited, "")
		return
	}
	if _, pattern := h.mux.Handler(r); pattern == "" {
		writeError(w, http.StatusNotFound, CodeNotFound, "")
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CreateToken != "" {
		tok, ok := bearerToken(r)
		got, want := hashToken(tok), hashToken(h.cfg.CreateToken)
		if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			writeError(w, http.StatusUnauthorized, CodeUnauthorized, "")
			return
		}
	} else if !h.cfg.AllowUnauthenticatedCreate {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "")
		return
	}
	var req createRequest
	if err := decodeJSONBody(w, r, maxCreateBody, &req, true); err != nil {
		writeDecodeError(w, err)
		return
	}
	ttl := h.cfg.SessionTTL
	if req.TTLSeconds < 0 {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "ttl_seconds must be positive")
		return
	}
	if req.TTLSeconds > 0 {
		ttl = clampDuration(time.Duration(req.TTLSeconds)*time.Second, MinSessionTTL, h.cfg.SessionTTL)
	}
	hostTok, err1 := randomToken(capabilityBytes)
	devTok, err2 := randomToken(capabilityBytes)
	if err := errors.Join(err1, err2); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}

	h.mu.Lock()
	if len(h.sessions) >= h.cfg.MaxSessions {
		h.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, CodeSessionLimit, "")
		return
	}
	var id string
	for id == "" || h.sessions[id] != nil {
		if id, err1 = randomToken(sessionIDBytes); err1 != nil {
			h.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "internal", "")
			return
		}
	}
	s := newSession(id, hashToken(hostTok), hashToken(devTok), h.cfg.Now().Add(ttl))
	h.sessions[id] = s
	h.mu.Unlock()

	writeJSON(w, http.StatusCreated, Session{ID: id, HostToken: hostTok, DeviceToken: devTok, ExpiresAt: s.expiresAt.UTC()})
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	s, _, ok := h.authorizeLocked(r)
	if ok {
		h.removeLocked(s)
	}
	h.mu.Unlock()
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleTURN(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	s, _, ok := h.authorizeLocked(r)
	switch {
	case !ok:
		h.mu.Unlock()
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "")
		return
	case h.cfg.TURNMinter == nil:
		h.mu.Unlock()
		writeError(w, http.StatusNotFound, CodeTURNUnavailable, "")
		return
	case s.turnMints >= h.cfg.MaxTURNPerSession:
		h.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, CodeTURNQuota, "")
		return
	}
	s.turnMints++ // count attempts, not successes, so failures cannot be retried unbounded
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	creds, err := h.cfg.TURNMinter.MintICEServers(ctx, h.cfg.TURNTTL)
	if err != nil || creds == nil || len(creds.ICEServers) == 0 {
		// Never reflect minter errors: they may describe upstream credentials.
		writeError(w, http.StatusBadGateway, CodeTURNFailed, "")
		return
	}
	writeJSON(w, http.StatusOK, creds)
}

// authorizeLocked resolves the session and the caller's role from the path ID
// and bearer capability. Unknown, expired and mismatched all look identical.
func (h *Handler) authorizeLocked(r *http.Request) (*session, role, bool) {
	tok, ok := bearerToken(r)
	if !ok {
		return nil, 0, false
	}
	return h.authorizeTokenLocked(r.PathValue("id"), tok)
}

func (h *Handler) authorizeTokenLocked(id, tok string) (*session, role, bool) {
	if !validSessionID(id) || tok == "" || len(tok) > maxBearerLen {
		return nil, 0, false
	}
	s := h.sessions[id]
	if s == nil {
		return nil, 0, false
	}
	if !h.cfg.Now().Before(s.expiresAt) {
		h.removeLocked(s)
		return nil, 0, false
	}
	sum := hashToken(tok)
	isHost := subtle.ConstantTimeCompare(sum[:], s.hostHash[:])
	isDevice := subtle.ConstantTimeCompare(sum[:], s.deviceHash[:])
	switch {
	case isHost == 1:
		return s, roleHost, true
	case isDevice == 1:
		return s, roleDevice, true
	}
	return nil, 0, false
}

func (h *Handler) removeLocked(s *session) {
	if s.deleted {
		return
	}
	s.deleted = true
	close(s.done)
	delete(h.sessions, s.id)
	for _, ib := range s.inbox {
		ib.msgs = nil
	}
}

func (h *Handler) maybeSweepLocked() {
	now := h.cfg.Now()
	if now.Sub(h.lastSweep) < sweepInterval && !h.lastSweep.IsZero() {
		return
	}
	h.lastSweep = now
	for _, s := range h.sessions {
		if !now.Before(s.expiresAt) {
			h.removeLocked(s)
		}
	}
}

func bearerToken(r *http.Request) (string, bool) {
	v := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(v) <= len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(v[len(prefix):])
	if tok == "" || len(tok) > maxBearerLen {
		return "", false
	}
	return tok, true
}

func hashToken(tok string) [32]byte { return sha256.Sum256([]byte(tok)) }

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

var errBodyTooLarge = errors.New("body too large")

// decodeJSONBody decodes exactly one JSON value bounded by limit bytes.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, limit int64, dst any, allowEmpty bool) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			return errBodyTooLarge
		case errors.Is(err, io.EOF) && allowEmpty:
			return nil
		}
		return fmt.Errorf("decode body: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errBodyTooLarge
		}
		return errors.New("trailing data after JSON body")
	}
	return nil
}

func writeDecodeError(w http.ResponseWriter, err error) {
	if errors.Is(err, errBodyTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, "")
		return
	}
	writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid JSON body")
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorResponse{Error: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

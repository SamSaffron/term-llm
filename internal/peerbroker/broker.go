package peerbroker

import (
	"bytes"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type storedChallenge struct {
	client    string
	keyHash   [32]byte
	nonce     string
	expiresAt time.Time
}
type invitationSlot struct {
	id        string
	capHash   [32]byte
	expiresAt time.Time
	consumed  bool
	peer      *wsPeer
}
type deviceSlot struct {
	id      string
	capHash [32]byte
	peer    *wsPeer
}
type hostLease struct {
	id          string
	keyHash     [32]byte
	leaseHash   [32]byte
	expiresAt   time.Time
	invitations map[string]*invitationSlot
	devices     map[string]*deviceSlot
	host        *wsPeer
	iceMints    map[string]int
	signals     int
	stop        chan struct{}
	icePolicy   HostICEConfig // validated host-requested ICE policy
	client      string        // client network charged for this lease
}
type Handler struct {
	cfg          Config
	mux          *http.ServeMux
	limiter      *tokenBucket
	clients      *clientRegistry
	mu           sync.Mutex
	challenges   map[string]storedChallenge
	hosts        map[string]*hostLease
	expiredHosts map[string]time.Time
	connections  int
	closed       bool
	closedCh     chan struct{}
}

func NewHandler(cfg Config) (*Handler, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	h := &Handler{cfg: cfg, limiter: newTokenBucket(cfg.RequestRate, cfg.RequestBurst, cfg.Now), clients: newClientRegistry(cfg.Clients), challenges: map[string]storedChallenge{}, hosts: map[string]*hostLease{}, expiredHosts: map[string]time.Time{}, closedCh: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/challenges", h.handleChallenge)
	mux.HandleFunc("POST /v2/hosts", h.handleRegister)
	mux.HandleFunc("POST /v2/hosts/{host}/renew", h.handleRenew)
	mux.HandleFunc("POST /v2/hosts/{host}/invitations", h.handleInvitation)
	mux.HandleFunc("DELETE /v2/hosts/{host}/invitations/{invitation}", h.handleDeleteInvitation)
	mux.HandleFunc("GET /v2/hosts/{host}/ice", h.handleICE)
	mux.HandleFunc("GET /v2/hosts/{host}/ws", h.handleWS)
	h.mux = mux
	return h, nil
}
func (h *Handler) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return fmt.Sprintf("peerbroker.Handler{hosts:%d, challenges:%d, connections:%d, config:[REDACTED]}", len(h.hosts), len(h.challenges), h.connections)
}
func (h *Handler) GoString() string { return h.String() }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	h.mu.Lock()
	h.sweepLocked()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		writeError(w, http.StatusServiceUnavailable, CodeBrokerClosed, "")
		return
	}
	if !h.allowClientRequest(h.clientKey(r)) || !h.limiter.allow() {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, CodeRateLimited, "")
		return
	}
	if _, pattern := h.mux.Handler(r); pattern == "" {
		writeError(w, http.StatusNotFound, CodeNotFound, "")
		return
	}
	h.mux.ServeHTTP(w, r)
}
func (h *Handler) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	close(h.closedCh)
	for id, host := range h.hosts {
		h.removeHostLocked(host)
		delete(h.hosts, id)
	}
	clear(h.challenges)
	clear(h.expiredHosts)
	if h.clients != nil {
		clear(h.clients.clients)
	}
}
func (h *Handler) HostCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweepLocked()
	return len(h.hosts)
}

func (h *Handler) handleChallenge(w http.ResponseWriter, r *http.Request) {
	if !h.admitted(r) {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "")
		return
	}
	var req challengeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	key, err := decodeToken(req.PublicKey, ed25519.PublicKeySize)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid public_key")
		return
	}
	id, err1 := randomToken(idBytes)
	nonce, err2 := randomToken(32)
	if errors.Join(err1, err2) != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "")
		return
	}
	ch := storedChallenge{client: h.clientKey(r), keyHash: hashCapability(string(key)), nonce: nonce, expiresAt: h.cfg.Now().Add(h.cfg.ChallengeTTL)}
	h.mu.Lock()
	if len(h.challenges) >= h.cfg.MaxChallenges || !h.acquireChallengeLocked(ch.client) {
		h.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, CodeQuotaExceeded, "")
		return
	}
	h.challenges[id] = ch
	h.mu.Unlock()
	writeJSON(w, http.StatusCreated, Challenge{ID: id, Nonce: nonce, ExpiresAt: ch.expiresAt.UTC()})
}
func (h *Handler) admitted(r *http.Request) bool {
	if h.cfg.AdmissionCredential == "" {
		return true
	}
	tok, ok := bearer(r)
	got, want := hashCapability(tok), hashCapability(h.cfg.AdmissionCredential)
	return ok && subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func registrationPayload(publicKey []byte, challengeID, nonce string, devices []DeviceCapabilityHash, icePolicy *[]wireICEServer) []byte {
	canonical := append([]DeviceCapabilityHash(nil), devices...)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].DeviceID < canonical[j].DeviceID })
	encoded, _ := json.Marshal(canonical)
	var b bytes.Buffer
	b.WriteString("peerbroker.v2/register\x00")
	b.Write(publicKey)
	b.WriteByte(0)
	b.WriteString(challengeID)
	b.WriteByte(0)
	b.WriteString(nonce)
	b.WriteByte(0)
	b.Write(encoded)
	b.WriteByte(0)
	// ICE server order is significant to WebRTC preference, so bind the exact
	// ordered policy rather than treating it as a set.
	if icePolicy != nil {
		policyJSON, _ := json.Marshal(*icePolicy)
		b.Write(policyJSON)
	}
	return b.Bytes()
}
func parseDevices(devices []DeviceCapabilityHash, max int) (map[string]*deviceSlot, error) {
	if len(devices) > max {
		return nil, errors.New("too many devices")
	}
	out := make(map[string]*deviceSlot, len(devices))
	for _, device := range devices {
		if !validPeerID(device.DeviceID) {
			return nil, errors.New("invalid device_id")
		}
		hash, err := decodeCapabilityHash(device.Hash)
		if err != nil {
			return nil, err
		}
		if _, exists := out[device.DeviceID]; exists {
			return nil, errors.New("duplicate device_id")
		}
		out[device.DeviceID] = &deviceSlot{id: device.DeviceID, capHash: hash}
	}
	return out, nil
}
func (h *Handler) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	key, keyErr := decodeToken(req.PublicKey, ed25519.PublicKeySize)
	sig, sigErr := decodeToken(req.Signature, ed25519.SignatureSize)
	devices, devicesErr := parseDevices(req.Devices, h.cfg.MaxDevicesPerHost)
	if keyErr != nil || sigErr != nil || devicesErr != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid registration")
		return
	}

	var icePolicy HostICEConfig

	h.mu.Lock()
	ch, ok := h.challenges[req.ChallengeID]
	if ok {
		delete(h.challenges, req.ChallengeID)
		h.releaseChallengeLocked(ch.client)
	}
	h.mu.Unlock()
	keyHash := hashCapability(string(key))
	// Verify signature includes ICE policy
	if !ok || !ch.expiresAt.After(h.cfg.Now()) || subtle.ConstantTimeCompare(ch.keyHash[:], keyHash[:]) != 1 || !ed25519.Verify(key, registrationPayload(key, req.ChallengeID, ch.nonce, req.Devices, req.ICEPolicy), sig) {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "")
		return
	}
	var turnURLs []string
	if h.cfg.TURNProvider != nil {
		turnURLs = h.cfg.TURNProvider.URLs()
	}
	if req.ICEPolicy != nil {
		validated, err := validateHostICEPolicy(*req.ICEPolicy, turnURLs)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, fmt.Sprintf("invalid ice_policy: %s", err))
			return
		}
		icePolicy = validated
	}
	leaseCap, err := randomToken(capabilityBytes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "")
		return
	}
	hostID := hostIDForKey(key)
	lease := &hostLease{id: hostID, keyHash: keyHash, leaseHash: hashCapability(leaseCap), expiresAt: h.cfg.Now().Add(h.cfg.LeaseTTL), invitations: map[string]*invitationSlot{}, devices: devices, iceMints: map[string]int{}, stop: make(chan struct{}), icePolicy: icePolicy, client: h.clientKey(r)}
	h.mu.Lock()
	old := h.hosts[hostID]
	if old == nil && len(h.hosts) >= h.cfg.MaxHosts || !h.hostCapacityLocked(lease.client, old) {
		h.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, CodeQuotaExceeded, "")
		return
	}
	if old != nil {
		h.removeHostLocked(old)
	}
	h.acquireHostLocked(lease)
	h.hosts[hostID] = lease
	delete(h.expiredHosts, hostID)
	h.mu.Unlock()
	writeJSON(w, http.StatusCreated, Registration{HostID: hostID, LeaseCapability: leaseCap, LeaseExpiresAt: lease.expiresAt.UTC()})
}
func (h *Handler) handleRenew(w http.ResponseWriter, r *http.Request) {
	var req renewRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	devices, err := parseDevices(req.Devices, h.cfg.MaxDevicesPerHost)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid devices")
		return
	}
	tok, ok := bearer(r)
	h.mu.Lock()
	host := h.hosts[r.PathValue("host")]
	if !ok || !h.hostAuthorizedLocked(host, tok) {
		h.mu.Unlock()
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "")
		return
	}
	for id, old := range host.devices {
		if next := devices[id]; next != nil && subtle.ConstantTimeCompare(old.capHash[:], next.capHash[:]) == 1 {
			next.peer = old.peer
		} else if old.peer != nil {
			close(old.peer.kick)
		}
	}
	host.devices = devices
	host.expiresAt = h.cfg.Now().Add(h.cfg.LeaseTTL)
	clear(host.iceMints)
	host.signals = 0
	expires := host.expiresAt.UTC()
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, Lease{LeaseExpiresAt: expires})
}
func (h *Handler) handleInvitation(w http.ResponseWriter, r *http.Request) {
	var req invitationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	if !validPeerID(req.InvitationID) {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid invitation_id")
		return
	}
	capHash, err := decodeCapabilityHash(req.CapabilityHash)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid capability_hash")
		return
	}
	tok, ok := bearer(r)
	now := h.cfg.Now()
	h.mu.Lock()
	host := h.hosts[r.PathValue("host")]
	if !ok || !h.hostAuthorizedLocked(host, tok) {
		h.mu.Unlock()
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "")
		return
	}
	if !req.ExpiresAt.After(now.Add(MinInvitationTTL)) || req.ExpiresAt.After(now.Add(MaxInvitationTTL)) {
		h.mu.Unlock()
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid invitation expiry")
		return
	}
	if existing := host.invitations[req.InvitationID]; existing == nil && len(host.invitations) >= h.cfg.MaxInvitationsPerHost {
		h.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, CodeQuotaExceeded, "")
		return
	} else if existing != nil && existing.peer != nil {
		h.mu.Unlock()
		writeError(w, http.StatusConflict, CodeConflict, "")
		return
	}
	host.invitations[req.InvitationID] = &invitationSlot{id: req.InvitationID, capHash: capHash, expiresAt: req.ExpiresAt.UTC()}
	h.mu.Unlock()
	writeJSON(w, http.StatusCreated, InvitationSlot{InvitationID: req.InvitationID, ExpiresAt: req.ExpiresAt.UTC()})
}
func (h *Handler) handleDeleteInvitation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("invitation")
	tok, ok := bearer(r)
	if !validPeerID(id) {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid invitation_id")
		return
	}
	h.mu.Lock()
	host := h.hosts[r.PathValue("host")]
	if !ok || !h.hostAuthorizedLocked(host, tok) {
		h.mu.Unlock()
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "")
		return
	}
	slot := host.invitations[id]
	if slot == nil {
		h.mu.Unlock()
		writeError(w, http.StatusNotFound, CodeNotFound, "")
		return
	}
	delete(host.invitations, id)
	if slot.peer != nil {
		close(slot.peer.kick)
		h.notifyPresenceLocked(host, wsRole{kind: PeerInvitation, id: id}, false)
	}
	h.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) peerEndpointErrorLocked(hostID string, host *hostLease, kind, id, tok string, hasToken, validQuery bool) (int, string) {
	if host == nil {
		if until, ok := h.expiredHosts[hostID]; ok && until.After(h.cfg.Now()) {
			return http.StatusGone, CodeExpired
		}
		return http.StatusNotFound, CodeNotFound
	}
	if !host.expiresAt.After(h.cfg.Now()) {
		return http.StatusGone, CodeExpired
	}
	if !hasToken || !validQuery || !h.peerAuthorizedLocked(host, kind, id, tok) {
		return http.StatusUnauthorized, CodeUnauthorized
	}
	return 0, ""
}

func (h *Handler) hostAuthorizedLocked(host *hostLease, tok string) bool {
	return host != nil && host.expiresAt.After(h.cfg.Now()) && capabilityEqual(host.leaseHash, tok)
}
func (h *Handler) peerAuthorizedLocked(host *hostLease, kind, id, tok string) bool {
	if host == nil || !host.expiresAt.After(h.cfg.Now()) {
		return false
	}
	switch kind {
	case PeerHost:
		return id == "" && capabilityEqual(host.leaseHash, tok)
	case PeerDevice:
		device := host.devices[id]
		return device != nil && capabilityEqual(device.capHash, tok)
	case PeerInvitation:
		invite := host.invitations[id]
		return invite != nil && (!invite.consumed || invite.peer != nil) && invite.expiresAt.After(h.cfg.Now()) && capabilityEqual(invite.capHash, tok)
	default:
		return false
	}
}
func (h *Handler) sweepLocked() {
	now := h.cfg.Now()
	for id, ch := range h.challenges {
		if !ch.expiresAt.After(now) {
			delete(h.challenges, id)
			h.releaseChallengeLocked(ch.client)
		}
	}
	for id, until := range h.expiredHosts {
		if !until.After(now) {
			delete(h.expiredHosts, id)
		}
	}
	for id, host := range h.hosts {
		if !host.expiresAt.After(now) {
			h.removeHostLocked(host)
			delete(h.hosts, id)
			h.expiredHosts[id] = now.Add(h.cfg.LeaseTTL)
			continue
		}
		for iid, invite := range host.invitations {
			if !invite.expiresAt.After(now) && invite.peer == nil {
				delete(host.invitations, iid)
			}
		}
	}
	h.maybeSweepClientsLocked(now)
}
func (h *Handler) removeHostLocked(host *hostLease) {
	select {
	case <-host.stop:
	default:
		close(host.stop)
		h.releaseHostLocked(host)
	}
}

func decodeJSON(r *http.Request, out any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return errors.New("content type must be application/json")
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxJSONBody+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return errors.New("invalid JSON")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: code, Message: message})
}
func validPeerID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

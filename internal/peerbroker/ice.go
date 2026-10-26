package peerbroker

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func requestPeer(r *http.Request) (kind, id string, ok bool) {
	device, invitation := r.URL.Query().Get("device"), r.URL.Query().Get("invitation")
	switch {
	case device != "" && invitation == "" && validPeerID(device):
		return PeerDevice, device, true
	case invitation != "" && device == "" && validPeerID(invitation):
		return PeerInvitation, invitation, true
	case device == "" && invitation == "":
		return PeerHost, "", true
	default:
		return "", "", false
	}
}
func (h *Handler) handleICE(w http.ResponseWriter, r *http.Request) {
	tok, hasToken := bearer(r)
	kind, id, validQuery := requestPeer(r)
	h.mu.Lock()
	hostID := r.PathValue("host")
	host := h.hosts[hostID]
	if status, code := h.peerEndpointErrorLocked(hostID, host, kind, id, tok, hasToken, validQuery); code != "" {
		h.mu.Unlock()
		writeError(w, status, code, "")
		return
	}
	quotaKey := kind + ":" + id
	if h.cfg.Coturn != nil && host.iceMints[quotaKey] >= h.cfg.MaxICEMintsPerLease {
		h.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, CodeQuotaExceeded, "")
		return
	}
	if h.cfg.Coturn != nil {
		host.iceMints[quotaKey]++
	}
	mintHostID := host.id
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, h.mintICE(mintHostID, id))
}
func (h *Handler) mintICE(hostID, peerID string) ICECredentials {
	servers := make([]ICEServer, 0, 2)
	if len(h.cfg.STUNURLs) > 0 {
		servers = append(servers, ICEServer{URLs: append([]string(nil), h.cfg.STUNURLs...)})
	}
	out := ICECredentials{ICEServers: servers}
	if h.cfg.Coturn == nil {
		return out
	}
	expires := h.cfg.Now().Add(h.cfg.Coturn.TTL).UTC().Truncate(time.Second)
	subject := hostID
	if peerID != "" {
		subject = peerID
	}
	parts := []string{strconv.FormatInt(expires.Unix(), 10)}
	if h.cfg.Coturn.Username != "" {
		parts = append(parts, h.cfg.Coturn.Username)
	}
	parts = append(parts, subject)
	username := strings.Join(parts, ":")
	mac := hmac.New(sha1.New, []byte(h.cfg.Coturn.SharedSecret))
	_, _ = mac.Write([]byte(username))
	credential := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	out.ICEServers = append(out.ICEServers, ICEServer{URLs: append([]string(nil), h.cfg.Coturn.URLs...), Username: username, Credential: credential})
	out.ExpiresAt = expires
	return out
}

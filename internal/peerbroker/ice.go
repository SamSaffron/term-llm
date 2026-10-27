package peerbroker

import (
	"context"
	"errors"
	"net/http"
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

	policy := host.icePolicy
	willMint := kind != PeerInvitation && h.cfg.TURNProvider != nil
	if policy.Configured() {
		willMint = kind != PeerInvitation && hostPolicyUsesBroker(policy)
	}
	quotaKey := kind + ":" + id
	if willMint && host.iceMints[quotaKey] >= h.cfg.MaxICEMintsPerLease {
		h.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, CodeQuotaExceeded, "")
		return
	}
	if willMint {
		host.iceMints[quotaKey]++
	}
	mintHostID := host.id
	h.mu.Unlock()

	var (
		creds ICECredentials
		err   error
	)
	if policy.Configured() {
		creds, _, err = h.effectiveICE(r.Context(), policy, kind, id, mintHostID)
	} else {
		creds, _, err = h.mintICELegacy(r.Context(), mintHostID, kind, id)
	}
	if err != nil {
		if willMint {
			h.mu.Lock()
			if h.hosts[hostID] == host && host.iceMints[quotaKey] > 0 {
				host.iceMints[quotaKey]--
			}
			h.mu.Unlock()
		}
		writeError(w, http.StatusServiceUnavailable, CodeTURNUnavailable, "")
		return
	}
	h.mu.Lock()
	current := h.hosts[hostID]
	if status, code := h.peerEndpointErrorLocked(hostID, current, kind, id, tok, hasToken, validQuery); code != "" {
		if willMint && current == host && host.iceMints[quotaKey] > 0 {
			host.iceMints[quotaKey]--
		}
		h.mu.Unlock()
		writeError(w, status, code, "")
		return
	}
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, creds)
}

func hostPolicyUsesBroker(policy HostICEConfig) bool {
	for _, server := range policy.Servers() {
		if server.CredentialSource == CredentialSourceBroker {
			return true
		}
	}
	return false
}

// effectiveICE returns the effective ICE response for a peer based on the host
// policy. Invitations receive STUN only. Broker credentials come only from the
// configured server-side provider.
func (h *Handler) effectiveICE(ctx context.Context, policy HostICEConfig, peerKind, peerID, hostID string) (ICECredentials, bool, error) {
	servers := policy.Servers()
	out := make([]ICEServer, 0, len(servers))
	var expires time.Time
	var minted bool

	for _, server := range servers {
		switch server.CredentialSource {
		case CredentialSourceBroker:
			if peerKind == PeerInvitation {
				continue
			}
			if h.cfg.TURNProvider == nil {
				return ICECredentials{}, true, errors.New("TURN credential provider is unavailable")
			}
			subject := hostID
			if peerID != "" {
				subject = peerID
			}
			credentials, err := h.cfg.TURNProvider.Mint(ctx, subject, append([]string(nil), server.URLs...))
			if err != nil || !validMintedICE(credentials, server.URLs, h.cfg.Now()) {
				return ICECredentials{}, true, errors.New("TURN credential provider failed")
			}
			out = append(out, credentials.ICEServers...)
			if expires.IsZero() || credentials.ExpiresAt.Before(expires) {
				expires = credentials.ExpiresAt
			}
			minted = true
		case CredentialSourceStatic:
			if peerKind == PeerInvitation {
				continue
			}
			out = append(out, ICEServer{URLs: append([]string(nil), server.URLs...), Username: server.Username, Credential: server.Credential})
		default:
			out = append(out, ICEServer{URLs: append([]string(nil), server.URLs...)})
		}
	}

	credentials := ICECredentials{ICEServers: out}
	if !expires.IsZero() {
		credentials.ExpiresAt = expires
	}
	return credentials, minted, nil
}

func (h *Handler) mintICELegacy(ctx context.Context, hostID, peerKind, peerID string) (ICECredentials, bool, error) {
	servers := make([]ICEServer, 0, 2)
	if len(h.cfg.STUNURLs) > 0 {
		servers = append(servers, ICEServer{URLs: append([]string(nil), h.cfg.STUNURLs...)})
	}
	out := ICECredentials{ICEServers: servers}
	if peerKind == PeerInvitation || h.cfg.TURNProvider == nil {
		return out, false, nil
	}
	subject := hostID
	if peerID != "" {
		subject = peerID
	}
	turnURLs := h.cfg.TURNProvider.URLs()
	credentials, err := h.cfg.TURNProvider.Mint(ctx, subject, turnURLs)
	if err != nil || !validMintedICE(credentials, turnURLs, h.cfg.Now()) {
		return ICECredentials{}, true, errors.New("TURN credential provider failed")
	}
	out.ICEServers = append(out.ICEServers, credentials.ICEServers...)
	out.ExpiresAt = credentials.ExpiresAt
	return out, true, nil
}

func validMintedICE(credentials ICECredentials, requestedURLs []string, now time.Time) bool {
	if len(credentials.ICEServers) == 0 || !credentials.ExpiresAt.After(now) || credentials.ExpiresAt.After(now.Add(48*time.Hour)) {
		return false
	}
	for _, server := range credentials.ICEServers {
		if server.Username == "" || server.Credential == "" || len(server.URLs) == 0 {
			return false
		}
		for _, raw := range server.URLs {
			if !urlInList(raw, requestedURLs) {
				return false
			}
		}
	}
	return true
}

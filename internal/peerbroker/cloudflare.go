package peerbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CloudflareTURNBaseURL is the official Cloudflare Realtime TURN API origin.
const CloudflareTURNBaseURL = "https://rtc.live.cloudflare.com"

const (
	cfDefaultTimeout     = 10 * time.Second
	cfMaxResponseBytes   = 64 << 10
	cfMaxErrorSnippet    = 200
	cfMaxICEServers      = 16
	cfMaxURLsPerServer   = 16
	cfMaxURLLen          = 512
	cfMaxCredentialField = 1024
	cfMaxKeyIDLen        = 128
)

// CloudflareTURNConfig configures CloudflareTURNMinter.
type CloudflareTURNConfig struct {
	// KeyID is the TURN key ID (not secret, but kept out of logs anyway).
	KeyID string
	// APIToken is the TURN key API token (secret).
	APIToken string
	// BaseURL overrides CloudflareTURNBaseURL (tests). It must be https unless loopback.
	BaseURL string
	// Timeout bounds each mint request (default 10s, max 30s).
	Timeout time.Duration
	// Transport overrides the HTTP transport (tests). When nil a dedicated
	// transport with proxies disabled is used.
	Transport http.RoundTripper
}

// CloudflareTURNMinter mints ICE servers via Cloudflare's
// POST /v1/turn/keys/{keyID}/credentials/generate-ice-servers API.
// See https://developers.cloudflare.com/realtime/turn/generate-credentials/.
type CloudflareTURNMinter struct {
	endpoint string
	keyID    string
	token    string
	client   *http.Client
}

var _ TURNMinter = (*CloudflareTURNMinter)(nil)

// NewCloudflareTURNMinter validates cfg and returns a minter.
func NewCloudflareTURNMinter(cfg CloudflareTURNConfig) (*CloudflareTURNMinter, error) {
	if !validKeyID(cfg.KeyID) {
		return nil, errors.New("peerbroker: cloudflare TURN key ID missing or invalid")
	}
	if strings.TrimSpace(cfg.APIToken) == "" || strings.ContainsAny(cfg.APIToken, " \t\r\n") {
		return nil, errors.New("peerbroker: cloudflare TURN API token missing or invalid")
	}
	base := cfg.BaseURL
	if base == "" {
		base = CloudflareTURNBaseURL
	}
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return nil, errors.New("peerbroker: invalid cloudflare base URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return nil, errors.New("peerbroker: cloudflare base URL must use https")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = cfDefaultTimeout
	}
	timeout = min(timeout, 30*time.Second)
	transport := cfg.Transport
	if transport == nil {
		transport = &http.Transport{
			Proxy:                 nil, // never route the API token through an ambient proxy
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          2,
			IdleConnTimeout:       30 * time.Second,
			ForceAttemptHTTP2:     true,
		}
	}
	u.Path += "/v1/turn/keys/" + url.PathEscape(cfg.KeyID) + "/credentials/generate-ice-servers"
	return &CloudflareTURNMinter{
		endpoint: u.String(),
		keyID:    cfg.KeyID,
		token:    cfg.APIToken,
		client: &http.Client{
			Transport:     transport,
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func validKeyID(id string) bool {
	if id == "" || len(id) > cfMaxKeyIDLen {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// String redacts the API token.
func (m *CloudflareTURNMinter) String() string {
	return "peerbroker.CloudflareTURNMinter{token:[REDACTED]}"
}

// GoString redacts the API token in %#v output.
func (m *CloudflareTURNMinter) GoString() string { return m.String() }

type cfGenerateRequest struct {
	TTL int64 `json:"ttl"`
}

// MintICEServers requests credentials valid for ttl (5-30 minutes).
func (m *CloudflareTURNMinter) MintICEServers(ctx context.Context, ttl time.Duration) (*ICECredentials, error) {
	if ttl < MinTURNTTL || ttl > MaxTURNTTL {
		return nil, fmt.Errorf("peerbroker: TURN TTL %s outside [%s, %s]", ttl, MinTURNTTL, MaxTURNTTL)
	}
	payload, _ := json.Marshal(cfGenerateRequest{TTL: int64(ttl / time.Second)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("peerbroker: build cloudflare TURN request")
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	start := time.Now()
	resp, err := m.client.Do(req)
	if err != nil {
		// url.Error embeds the endpoint (key ID); report only the cause.
		return nil, fmt.Errorf("peerbroker: cloudflare TURN request failed: %s", m.redact(stripURLError(err).Error()))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, cfMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("peerbroker: read cloudflare TURN response: %s", m.redact(err.Error()))
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peerbroker: cloudflare TURN HTTP %d: %s", resp.StatusCode, m.redact(errorSnippet(data)))
	}
	if len(data) > cfMaxResponseBytes {
		return nil, errors.New("peerbroker: cloudflare TURN response too large")
	}
	servers, err := parseCloudflareICEServers(data)
	if err != nil {
		return nil, err
	}
	return &ICECredentials{ICEServers: servers, ExpiresAt: start.Add(ttl).UTC()}, nil
}

// parseCloudflareICEServers accepts the documented array shape and the older
// single-object shape returned by .../credentials/generate.
func parseCloudflareICEServers(data []byte) ([]ICEServer, error) {
	var env struct {
		ICEServers json.RawMessage `json:"iceServers"`
	}
	if err := json.Unmarshal(data, &env); err != nil || len(env.ICEServers) == 0 {
		return nil, errors.New("peerbroker: malformed cloudflare TURN response")
	}
	var servers []ICEServer
	if trimmed := bytes.TrimSpace(env.ICEServers); len(trimmed) > 0 && trimmed[0] == '{' {
		var one ICEServer
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil, errors.New("peerbroker: malformed cloudflare TURN response")
		}
		servers = []ICEServer{one}
	} else if err := json.Unmarshal(trimmed, &servers); err != nil {
		return nil, errors.New("peerbroker: malformed cloudflare TURN response")
	}
	if len(servers) == 0 || len(servers) > cfMaxICEServers {
		return nil, errors.New("peerbroker: cloudflare TURN response has invalid server count")
	}
	hasTURN := false
	for _, s := range servers {
		if len(s.URLs) == 0 || len(s.URLs) > cfMaxURLsPerServer ||
			len(s.Username) > cfMaxCredentialField || len(s.Credential) > cfMaxCredentialField {
			return nil, errors.New("peerbroker: cloudflare TURN response has invalid server entry")
		}
		for _, raw := range s.URLs {
			scheme, _, _ := strings.Cut(raw, ":")
			switch {
			case len(raw) > cfMaxURLLen:
				return nil, errors.New("peerbroker: cloudflare TURN response has oversized URL")
			case scheme == "stun":
			case scheme == "turn" || scheme == "turns":
				if s.Username == "" || s.Credential == "" {
					return nil, errors.New("peerbroker: cloudflare TURN server missing credentials")
				}
				hasTURN = true
			default:
				return nil, errors.New("peerbroker: cloudflare TURN response has unsupported URL scheme")
			}
		}
	}
	if !hasTURN {
		return nil, errors.New("peerbroker: cloudflare TURN response has no TURN servers")
	}
	return servers, nil
}

// redact removes the API token and key ID from upstream-derived text.
func (m *CloudflareTURNMinter) redact(s string) string {
	s = strings.ReplaceAll(s, m.token, "[REDACTED]")
	return strings.ReplaceAll(s, m.keyID, "[KEY]")
}

// errorSnippet returns a short printable excerpt of an error body.
func errorSnippet(data []byte) string {
	var b strings.Builder
	for _, r := range string(data) {
		if b.Len() >= cfMaxErrorSnippet {
			b.WriteString("…")
			break
		}
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

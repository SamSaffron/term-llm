package peerbroker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	clientTimeout          = 20 * time.Second
	maxClientResponseBytes = 1 << 20
	maxClientErrorBytes    = 4 << 10
)

type Client struct {
	base *url.URL
	http *http.Client
}

func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("peerbroker: parse base URL: %w", err)
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("peerbroker: base URL must be scheme://host[/path] without credentials, query, or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil, errors.New("peerbroker: HTTPS required except for loopback")
	}
	var hc http.Client
	if httpClient == nil {
		hc.Timeout = clientTimeout
	} else {
		hc = *httpClient
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: u, http: &hc}, nil
}
func GenerateHostKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// RegisterHost proves the host key and installs the complete initial set of
// device reconnect capability hashes. The broker returns only its own lease
// capability; device capabilities remain host-generated.
func (c *Client) RegisterHost(ctx context.Context, privateKey ed25519.PrivateKey, admissionCredential string, devices []DeviceCapabilityHash) (*Registration, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("peerbroker: invalid Ed25519 private key")
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return c.RegisterHostWithSigner(ctx, publicKey, admissionCredential, devices, func(payload []byte) ([]byte, error) {
		return ed25519.Sign(privateKey, payload), nil
	})
}

// RegisterHostWithSigner proves a host identity without requiring callers to
// export its private key. sign must produce an Ed25519 signature over payload.
func (c *Client) RegisterHostWithSigner(ctx context.Context, publicKey ed25519.PublicKey, admissionCredential string, devices []DeviceCapabilityHash, sign func([]byte) ([]byte, error)) (*Registration, error) {
	if len(publicKey) != ed25519.PublicKeySize || sign == nil {
		return nil, errors.New("peerbroker: invalid Ed25519 signer")
	}
	devices = append([]DeviceCapabilityHash{}, devices...)
	encodedKey := base64.RawURLEncoding.EncodeToString(publicKey)
	var challenge Challenge
	if err := c.do(ctx, http.MethodPost, "/v2/challenges", admissionCredential, challengeRequest{PublicKey: encodedKey}, http.StatusCreated, &challenge); err != nil {
		return nil, err
	}
	if _, err := decodeToken(challenge.ID, idBytes); err != nil {
		return nil, errors.New("peerbroker: malformed challenge response")
	}
	if _, err := decodeToken(challenge.Nonce, 32); err != nil || challenge.ExpiresAt.IsZero() {
		return nil, errors.New("peerbroker: malformed or expired challenge response")
	}
	signature, err := sign(registrationPayload(publicKey, challenge.ID, challenge.Nonce, devices))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, errors.New("peerbroker: host signer failed")
	}
	req := registerRequest{PublicKey: encodedKey, ChallengeID: challenge.ID, Signature: base64.RawURLEncoding.EncodeToString(signature), Devices: devices}
	var registration Registration
	if err := c.do(ctx, http.MethodPost, "/v2/hosts", "", req, http.StatusCreated, &registration); err != nil {
		return nil, err
	}
	if registration.HostID != hostIDForKey(publicKey) || registration.LeaseCapability == "" {
		return nil, errors.New("peerbroker: malformed registration response")
	}
	return &registration, nil
}

// Renew atomically replaces the active device reconnect hash set and renews
// the host lease. Omitting a device revokes that reconnect capability.
func (c *Client) Renew(ctx context.Context, hostID, leaseCapability string, devices []DeviceCapabilityHash) (*Lease, error) {
	devices = append([]DeviceCapabilityHash{}, devices...)
	var out Lease
	if err := c.do(ctx, http.MethodPost, hostPath(hostID, "/renew"), leaseCapability, renewRequest{Devices: devices}, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RegisterInvitation registers a host-generated invitation capability hash.
// The plaintext capability never leaves the host except when given to device.
func (c *Client) RegisterInvitation(ctx context.Context, hostID, leaseCapability, invitationID, capabilityHash string, expiry time.Time) (*InvitationSlot, error) {
	var out InvitationSlot
	req := invitationRequest{InvitationID: invitationID, CapabilityHash: capabilityHash, ExpiresAt: expiry.UTC()}
	if err := c.do(ctx, http.MethodPost, hostPath(hostID, "/invitations"), leaseCapability, req, http.StatusCreated, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (c *Client) ICE(ctx context.Context, hostID, peerKind, peerID, capability string) (*ICECredentials, error) {
	path, err := peerPath(hostID, "/ice", peerKind, peerID)
	if err != nil {
		return nil, err
	}
	var out ICECredentials
	if err := c.do(ctx, http.MethodGet, path, capability, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func hostPath(hostID, suffix string) string { return "/v2/hosts/" + url.PathEscape(hostID) + suffix }
func peerPath(hostID, suffix, kind, id string) (string, error) {
	path := hostPath(hostID, suffix)
	if kind == "" || kind == PeerHost {
		if id != "" {
			return "", errors.New("peerbroker: host peer has no ID")
		}
		return path, nil
	}
	if !validPeerID(id) {
		return "", errors.New("peerbroker: invalid peer ID")
	}
	q := url.Values{}
	switch kind {
	case PeerDevice:
		q.Set("device", id)
	case PeerInvitation:
		q.Set("invitation", id)
	default:
		return "", errors.New("peerbroker: invalid peer kind")
	}
	return path + "?" + q.Encode(), nil
}
func validHostID(id string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(id)
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == id
}
func (c *Client) do(ctx context.Context, method, path, credential string, in any, want int, out any) error {
	pathOnly, query := path, ""
	if before, after, ok := strings.Cut(path, "?"); ok {
		pathOnly, query = before, after
	}
	if rest, ok := strings.CutPrefix(pathOnly, "/v2/hosts/"); ok {
		id, _, _ := strings.Cut(rest, "/")
		decoded, err := url.PathUnescape(id)
		if err != nil || !validHostID(decoded) {
			return errors.New("peerbroker: invalid host ID")
		}
	}
	u := *c.base
	u.Path, u.RawPath, u.RawQuery = c.base.Path+pathOnly, "", query
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("peerbroker: encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return fmt.Errorf("peerbroker: build request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("peerbroker: %s request: %w", method, stripURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		return decodeAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxClientErrorBytes))
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxClientResponseBytes+1))
	if err != nil {
		return fmt.Errorf("peerbroker: read response: %w", err)
	}
	if len(data) > maxClientResponseBytes {
		return errors.New("peerbroker: response too large")
	}
	if json.Unmarshal(data, out) != nil {
		return errors.New("peerbroker: malformed response")
	}
	return nil
}
func decodeAPIError(resp *http.Response) error {
	out := &APIError{StatusCode: resp.StatusCode, Code: fmt.Sprintf("http_%d", resp.StatusCode)}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxClientErrorBytes))
	var wire errorResponse
	if json.Unmarshal(data, &wire) == nil && wire.Error != "" {
		out.Code, out.Message = truncate(wire.Error, 64), truncate(wire.Message, 200)
	}
	return out
}
func stripURLError(err error) error {
	var target *url.Error
	if errors.As(err, &target) {
		return target.Err
	}
	return err
}
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type Conn struct {
	ws    *websocket.Conn
	write sync.Mutex
	Ready Frame
}

func (c *Client) DialHost(ctx context.Context, hostID, leaseCapability string) (*Conn, error) {
	return c.dial(ctx, hostID, PeerHost, "", leaseCapability)
}
func (c *Client) DialInvitation(ctx context.Context, hostID, invitationID, capability string) (*Conn, error) {
	return c.dial(ctx, hostID, PeerInvitation, invitationID, capability)
}
func (c *Client) DialDevice(ctx context.Context, hostID, deviceID, reconnectCapability string) (*Conn, error) {
	return c.dial(ctx, hostID, PeerDevice, deviceID, reconnectCapability)
}
func (c *Client) ReconnectDevice(ctx context.Context, hostID, deviceID, reconnectCapability string) (*Conn, error) {
	return c.DialDevice(ctx, hostID, deviceID, reconnectCapability)
}
func (c *Client) dial(ctx context.Context, hostID, kind, id, credential string) (*Conn, error) {
	if !validHostID(hostID) {
		return nil, errors.New("peerbroker: invalid host ID")
	}
	path, err := peerPath(hostID, "/ws", kind, id)
	if err != nil {
		return nil, err
	}
	pathOnly, query := path, ""
	if before, after, ok := strings.Cut(path, "?"); ok {
		pathOnly, query = before, after
	}
	u := *c.base
	u.Path, u.RawPath, u.RawQuery = c.base.Path+pathOnly, "", query
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 10 * time.Second, Subprotocols: []string{WSSubprotocol}}
	if transport, ok := c.http.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil {
		dialer.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	header := http.Header{"Authorization": []string{"Bearer " + credential}}
	ws, resp, err := dialer.DialContext(ctx, u.String(), header)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			return nil, decodeAPIError(resp)
		}
		return nil, fmt.Errorf("peerbroker: dial websocket: %w", stripURLError(err))
	}
	ws.SetReadLimit(2 << 20)
	conn := &Conn{ws: ws}
	if deadline, ok := ctx.Deadline(); ok {
		_ = ws.SetReadDeadline(deadline)
	}
	frame, err := conn.Recv()
	_ = ws.SetReadDeadline(time.Time{})
	if err != nil || frame.Type != FrameReady {
		_ = ws.Close()
		return nil, errors.New("peerbroker: websocket registration failed")
	}
	conn.Ready = *frame
	return conn, nil
}
func (c *Conn) Send(peerKind, peerID string, body json.RawMessage) error {
	if !validSignalBody(body) {
		return errors.New("peerbroker: signal body must be non-null JSON")
	}
	c.write.Lock()
	defer c.write.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if err := c.ws.WriteJSON(Frame{Type: FrameSignal, PeerKind: peerKind, PeerID: peerID, Body: body}); err != nil {
		return fmt.Errorf("peerbroker: send signal: %w", err)
	}
	return nil
}
func (c *Conn) Recv() (*Frame, error) {
	_, data, err := c.ws.ReadMessage()
	if err != nil {
		var closeErr *websocket.CloseError
		if errors.As(err, &closeErr) {
			return nil, &CloseError{Code: closeErr.Code, Reason: closeErr.Text}
		}
		return nil, fmt.Errorf("peerbroker: receive: %w", err)
	}
	var frame Frame
	if json.Unmarshal(data, &frame) != nil {
		return nil, errors.New("peerbroker: malformed websocket frame")
	}
	return &frame, nil
}
func (c *Conn) SetReadDeadline(deadline time.Time) error { return c.ws.SetReadDeadline(deadline) }
func (c *Conn) Close() error {
	c.write.Lock()
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	c.write.Unlock()
	return c.ws.Close()
}

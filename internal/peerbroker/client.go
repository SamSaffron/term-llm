package peerbroker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxClientResponseBytes = 4 << 20
	maxClientErrorBytes    = 4 << 10
	defaultClientTimeout   = 90 * time.Second
)

// Client talks to a peerbroker Handler. Capabilities are only ever sent in
// the Authorization header, never in URLs, and redirects are not followed.
type Client struct {
	base *url.URL
	http *http.Client
}

// CreateOptions configures Client.Create.
type CreateOptions struct {
	// TTL requests a session lifetime; the broker clamps it to its own bounds.
	TTL time.Duration
	// CreateToken is the operator bearer if the broker requires one.
	CreateToken string
}

// NewClient returns a client for baseURL (the Handler's mount point). baseURL
// must be https unless it targets a loopback host. If httpClient is nil a
// default client with a 90s timeout is used. Redirects are never followed.
func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("peerbroker: parse base URL: %w", err)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" {
		return nil, errors.New("peerbroker: base URL must be scheme://host[/path] without credentials, query or fragment")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
	default:
		return nil, errors.New("peerbroker: base URL must use https (http allowed only for loopback)")
	}
	var hc http.Client
	if httpClient != nil {
		hc = *httpClient
	} else {
		hc.Timeout = defaultClientTimeout
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: u, http: &hc}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Create opens a new rendezvous session.
func (c *Client) Create(ctx context.Context, opts CreateOptions) (*Session, error) {
	var req createRequest
	if opts.TTL > 0 {
		req.TTLSeconds = int(opts.TTL / time.Second)
	}
	var out Session
	if err := c.do(ctx, http.MethodPost, "/v1/sessions", nil, opts.CreateToken, req, http.StatusCreated, &out); err != nil {
		return nil, err
	}
	if out.ID == "" || out.HostToken == "" || out.DeviceToken == "" {
		return nil, errors.New("peerbroker: incomplete create response")
	}
	return &out, nil
}

// Send delivers an opaque JSON body to the other side's mailbox and returns its sequence number.
func (c *Client) Send(ctx context.Context, sessionID, token string, body json.RawMessage) (uint64, error) {
	if !json.Valid(body) {
		return 0, errors.New("peerbroker: message body must be valid JSON")
	}
	var out sendResponse
	if err := c.do(ctx, http.MethodPost, sessionPath(sessionID, "/messages"), nil, token, sendRequest{Body: body}, http.StatusCreated, &out); err != nil {
		return 0, err
	}
	return out.Seq, nil
}

// Receive acknowledges every message with Seq <= after and returns the
// remaining pending messages, long-polling up to wait (broker-bounded) when
// none are pending. An empty result means the wait elapsed or was superseded.
func (c *Client) Receive(ctx context.Context, sessionID, token string, after uint64, wait time.Duration) ([]Message, error) {
	q := url.Values{}
	q.Set("after", strconv.FormatUint(after, 10))
	if wait > 0 {
		q.Set("wait", strconv.FormatFloat(wait.Seconds(), 'f', 3, 64))
	}
	var out receiveResponse
	if err := c.do(ctx, http.MethodGet, sessionPath(sessionID, "/messages"), q, token, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return out.Messages, nil
}

// Delete closes the session for both sides.
func (c *Client) Delete(ctx context.Context, sessionID, token string) error {
	return c.do(ctx, http.MethodDelete, sessionPath(sessionID, ""), nil, token, nil, http.StatusNoContent, nil)
}

// MintTURN asks the broker for short-lived ICE servers.
func (c *Client) MintTURN(ctx context.Context, sessionID, token string) (*ICECredentials, error) {
	var out ICECredentials
	if err := c.do(ctx, http.MethodPost, sessionPath(sessionID, "/turn"), nil, token, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func sessionPath(id, suffix string) string {
	return "/v1/sessions/" + id + suffix
}

// SessionIDLen is the byte length of a session ID. The textual ID is exactly
// the pairing link's `sid`: unpadded base64url of these 16 bytes (22 chars).
const SessionIDLen = sessionIDBytes

// FormatSessionID encodes raw session ID bytes as the broker's textual ID.
func FormatSessionID(raw [SessionIDLen]byte) string {
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// ParseSessionID decodes a textual session ID (pairing `sid`) to raw bytes.
// It accepts only canonical unpadded base64url of exactly 16 bytes.
func ParseSessionID(id string) ([SessionIDLen]byte, error) {
	var out [SessionIDLen]byte
	b, err := base64.RawURLEncoding.Strict().DecodeString(id)
	if err != nil || len(b) != SessionIDLen {
		return out, errors.New("peerbroker: invalid session ID")
	}
	copy(out[:], b)
	return out, nil
}

// validSessionID accepts canonical broker IDs only, so IDs can never alter
// the request path.
func validSessionID(id string) bool {
	if len(id) != base64.RawURLEncoding.EncodedLen(SessionIDLen) {
		return false
	}
	_, err := ParseSessionID(id)
	return err == nil
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, token string, in any, want int, out any) error {
	if id, ok := strings.CutPrefix(path, "/v1/sessions/"); ok {
		id, _, _ = strings.Cut(id, "/")
		if !validSessionID(id) {
			return errors.New("peerbroker: invalid session ID")
		}
	}
	u := *c.base
	u.Path = c.base.Path + path
	u.RawPath = ""
	if q != nil {
		u.RawQuery = q.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("peerbroker: encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return fmt.Errorf("peerbroker: build request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("peerbroker: %s %s: %w", method, redactedPath(path), stripURLError(err))
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
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("peerbroker: decode response: %w", err)
	}
	return nil
}

func decodeAPIError(resp *http.Response) error {
	apiErr := &APIError{StatusCode: resp.StatusCode, Code: "http_" + strconv.Itoa(resp.StatusCode)}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxClientErrorBytes))
	var er errorResponse
	if json.Unmarshal(data, &er) == nil && er.Error != "" {
		apiErr.Code = truncate(er.Error, 64)
		apiErr.Message = truncate(er.Message, 200)
	}
	return apiErr
}

// redactedPath hides the session ID from error strings.
func redactedPath(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) > 3 {
		parts[3] = "{id}"
	}
	return strings.Join(parts, "/")
}

// stripURLError drops the *url.Error wrapper (which embeds the full URL).
func stripURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

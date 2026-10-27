package peer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/http/httpguts"
)

const (
	// ProtocolVersion is the only wire version this package speaks.
	ProtocolVersion = 1
	// ChannelLabel is the DataChannel label that carries the HTTP bridge.
	ChannelLabel = "term-llm-http-v1"
)

const (
	// PeerErrorHeader distinguishes bridge authorization results from ordinary
	// application 401/403 responses.
	PeerErrorHeader = "X-Term-LLM-Peer-Error"
	// PeerErrorAuthorizationRevoked means the durable device grant is no longer valid.
	PeerErrorAuthorizationRevoked = "authorization_revoked"
	// PeerErrorScopeDenied means the grant is valid but the requested route is outside its scope.
	PeerErrorScopeDenied = "scope_denied"
)

// Frame types.
const (
	TypeRequest       = "request"
	TypeResponseStart = "response_start"
	TypeResponseBody  = "response_body"
	TypeResponseEnd   = "response_end"
	TypeCancel        = "cancel"
)

// Frame is one wire message. Body marshals as base64; Header as arrays.
type Frame struct {
	Version int         `json:"version"`
	Type    string      `json:"type"`
	ID      string      `json:"id"`
	Method  string      `json:"method,omitempty"`
	Path    string      `json:"path,omitempty"`
	Header  http.Header `json:"header,omitempty"`
	Body    []byte      `json:"body,omitempty"`
	Status  int         `json:"status,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// Default limits.
const (
	DefaultMaxFrameBytes        = 64 << 10
	DefaultMaxConcurrentStreams = 32
	DefaultMaxBufferedBytes     = 1 << 20
	DefaultBodyChunkBytes       = 16 << 10

	maxIDLength   = 64
	maxPathLength = 8 << 10
	// frameOverhead reserves room for the JSON envelope around a body chunk.
	frameOverhead = 1 << 10
)

// Limits bounds resource use on one bridge. Zero fields take defaults.
type Limits struct {
	// MaxFrameBytes bounds every encoded frame in either direction.
	MaxFrameBytes int
	// MaxRequestBodyBytes bounds request bodies; it can never exceed
	// what fits in one frame.
	MaxRequestBodyBytes int
	// MaxConcurrentStreams bounds in-flight requests per Server or Client.
	MaxConcurrentStreams int
	// MaxBufferedBytes is the DataChannel send-buffer high-water mark;
	// senders block above it until the peer drains.
	MaxBufferedBytes int
	// BodyChunkBytes is the largest raw body slice per response_body frame.
	BodyChunkBytes int
}

func (l Limits) withDefaults() Limits {
	if l.MaxFrameBytes <= 0 {
		l.MaxFrameBytes = DefaultMaxFrameBytes
	}
	if l.MaxFrameBytes < 2*frameOverhead {
		l.MaxFrameBytes = 2 * frameOverhead
	}
	// Base64 expands 3 bytes to 4.
	fit := (l.MaxFrameBytes - frameOverhead) / 4 * 3
	if l.MaxRequestBodyBytes <= 0 || l.MaxRequestBodyBytes > fit {
		l.MaxRequestBodyBytes = fit
	}
	if l.BodyChunkBytes <= 0 {
		l.BodyChunkBytes = DefaultBodyChunkBytes
	}
	if l.BodyChunkBytes > fit {
		l.BodyChunkBytes = fit
	}
	if l.MaxConcurrentStreams <= 0 {
		l.MaxConcurrentStreams = DefaultMaxConcurrentStreams
	}
	if l.MaxBufferedBytes <= 0 {
		l.MaxBufferedBytes = DefaultMaxBufferedBytes
	}
	return l
}

var errFrameTooLarge = errors.New("peer: frame exceeds size limit")

func encodeFrame(f *Frame, maxBytes int) ([]byte, error) {
	f.Version = ProtocolVersion
	b, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("encode %s frame: %w", f.Type, err)
	}
	if len(b) > maxBytes {
		return nil, fmt.Errorf("%s frame of %d bytes: %w", f.Type, len(b), errFrameTooLarge)
	}
	return b, nil
}

func decodeFrame(data []byte, maxBytes int) (*Frame, error) {
	if len(data) > maxBytes {
		return nil, errFrameTooLarge
	}
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}
	if f.Version != ProtocolVersion {
		return &f, fmt.Errorf("unsupported protocol version %d", f.Version)
	}
	if f.ID == "" || len(f.ID) > maxIDLength {
		return &f, errors.New("invalid frame id")
	}
	return &f, nil
}

var allowedMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
	http.MethodOptions: true,
}

// cleanRequestPath validates an origin-relative request target such as
// "/api/x?y=1". It rejects absolute or scheme-relative URLs, dot segments,
// encoded separators, backslashes, control bytes and fragments.
func cleanRequestPath(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > maxPathLength {
		return nil, errors.New("invalid path length")
	}
	if raw[0] != '/' || strings.HasPrefix(raw, "//") {
		return nil, errors.New("path must be origin-relative")
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c < 0x21 || c == 0x7f || c == '\\' || c == '#' {
			return nil, errors.New("path contains forbidden characters")
		}
	}
	pathname := raw
	if query := strings.IndexByte(pathname, '?'); query >= 0 {
		pathname = pathname[:query]
	}
	lowerPath := strings.ToLower(pathname)
	for _, bad := range []string{"%2f", "%5c", "%00", "%2e"} {
		if strings.Contains(lowerPath, bad) {
			return nil, errors.New("path contains forbidden escapes")
		}
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return nil, fmt.Errorf("parse path: %w", err)
	}
	if u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.User != nil {
		return nil, errors.New("path must be origin-relative")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return nil, errors.New("path contains dot segments")
		}
	}
	return u, nil
}

// strippedRequestHeaders are never accepted from the remote peer: the
// peer authenticates through the session gate, not through ambient
// credentials, and must not be able to spoof proxy or transport metadata.
var strippedRequestHeaders = map[string]bool{
	"Authorization": true, "Proxy-Authorization": true, "Proxy-Authenticate": true,
	"Cookie": true, "Forwarded": true, "Via": true, "Host": true,
	"Connection": true, "Keep-Alive": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true, "Content-Length": true,
	"Origin": true, "Referer": true, "Expect": true, "Http2-Settings": true,
}

func strippedHeader(key string) bool {
	if strippedRequestHeaders[key] {
		return true
	}
	return strings.HasPrefix(key, "X-Forwarded-") || strings.HasPrefix(key, "Proxy-") ||
		strings.HasPrefix(key, "Sec-") || key == "X-Real-Ip" || key == "X-Client-Ip"
}

// sanitizeRequestHeader canonicalises keys, rejects malformed fields and
// drops headers the remote peer may not set.
func sanitizeRequestHeader(in http.Header) (http.Header, error) {
	out := make(http.Header, len(in))
	for k, vs := range in {
		if !httpguts.ValidHeaderFieldName(k) {
			return nil, fmt.Errorf("invalid header name %q", k)
		}
		ck := http.CanonicalHeaderKey(k)
		if strippedHeader(ck) {
			continue
		}
		for _, v := range vs {
			if !httpguts.ValidHeaderFieldValue(v) {
				return nil, fmt.Errorf("invalid value for header %q", ck)
			}
			out[ck] = append(out[ck], v)
		}
	}
	return out, nil
}

var hopByHopResponseHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer",
	"Transfer-Encoding", "Upgrade",
}

func sanitizeResponseHeader(in http.Header) http.Header {
	out := in.Clone()
	if out == nil {
		out = http.Header{}
	}
	for _, k := range hopByHopResponseHeaders {
		out.Del(k)
	}
	return out
}

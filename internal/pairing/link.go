package pairing

import (
	"crypto/ed25519"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// LinkScheme is the URI scheme of pairing links; the host part carries the version.
const LinkScheme = "term-llm-pair"

// Link size limits.
const (
	MaxLinkBytes       = 4096
	MaxCapabilityBytes = 128 // matches the broker's bearer limit
)

// Link is the content of a pairing QR code.
//
// Secret (ps) and Capability (cap) are bearer material: String places them in
// the URI fragment, never the query, so they are not part of anything a URL
// handler would normally send or log. Use Redacted for logs.
type Link struct {
	Broker    string
	SessionID []byte // raw bytes of the broker session ID (base64url-decoded)
	HostKey   ed25519.PublicKey
	HostNonce []byte
	Secret    []byte
	ExpiresAt time.Time
	Name      string
	// Capability is the broker's device signalling capability. It is
	// independent of Secret, optional, and is never part of any transcript.
	Capability string
}

// BrokerSessionID returns the broker session ID exactly as the broker issued it.
func (l Link) BrokerSessionID() string { return B64(l.SessionID).String() }

func (l Link) query() string {
	var b strings.Builder
	b.WriteString("broker=" + url.QueryEscape(l.Broker))
	b.WriteString("&sid=" + B64(l.SessionID).String())
	b.WriteString("&hk=" + B64(l.HostKey).String())
	b.WriteString("&hn=" + B64(l.HostNonce).String())
	b.WriteString("&exp=" + strconv.FormatInt(l.ExpiresAt.Unix(), 10))
	if l.Name != "" {
		b.WriteString("&name=" + url.QueryEscape(l.Name))
	}
	return b.String()
}

// String encodes the link as
// term-llm-pair://v1?broker=..&sid=..&hk=..&hn=..&exp=..[&name=..]#ps=..[&cap=..].
func (l Link) String() string {
	s := LinkScheme + "://v" + strconv.Itoa(Version) + "?" + l.query() + "#ps=" + B64(l.Secret).String()
	if l.Capability != "" {
		s += "&cap=" + l.Capability
	}
	return s
}

// Redacted returns the link without its fragment (no secret, no capability).
func (l Link) Redacted() string {
	return LinkScheme + "://v" + strconv.Itoa(Version) + "?" + l.query()
}

// ParseLink strictly decodes and validates a pairing link. Unknown or
// duplicate parameters, secrets in the query, and non-canonical encodings are
// rejected. It does not check expiry.
func ParseLink(s string) (Link, error) {
	if len(s) > MaxLinkBytes {
		return Link{}, fmt.Errorf("%w: link too long", ErrInvalid)
	}
	u, err := url.Parse(s)
	if err != nil {
		return Link{}, fmt.Errorf("%w: parse link: %v", ErrInvalid, err)
	}
	if u.Scheme != LinkScheme || u.Opaque != "" || u.User != nil || u.Path != "" {
		return Link{}, fmt.Errorf("%w: not a %s://v%d link", ErrInvalid, LinkScheme, Version)
	}
	if u.Host != "v"+strconv.Itoa(Version) {
		return Link{}, fmt.Errorf("%w: unsupported link version %q", ErrInvalid, u.Host)
	}
	q, err := strictParams(u.RawQuery, "broker", "sid", "hk", "hn", "exp", "name")
	if err != nil {
		return Link{}, fmt.Errorf("query: %w", err)
	}
	frag, err := strictParams(u.EscapedFragment(), "ps", "cap")
	if err != nil {
		return Link{}, fmt.Errorf("fragment: %w", err)
	}
	var l Link
	l.Broker = q["broker"]
	if err := validateBroker(l.Broker); err != nil {
		return Link{}, err
	}
	fields := []struct {
		key string
		val string
		dst *[]byte
		n   int
	}{
		{"sid", q["sid"], &l.SessionID, SessionIDSize},
		{"hk", q["hk"], (*[]byte)(&l.HostKey), ed25519.PublicKeySize},
		{"hn", q["hn"], &l.HostNonce, NonceSize},
		{"ps", frag["ps"], &l.Secret, SecretSize},
	}
	for _, f := range fields {
		v, err := DecodeB64(f.val)
		if err != nil {
			return Link{}, fmt.Errorf("%w: %s: %v", ErrInvalid, f.key, err)
		}
		if err := wantLen(f.key, v, f.n); err != nil {
			return Link{}, err
		}
		*f.dst = v
	}
	expStr := q["exp"]
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || exp <= 0 || strconv.FormatInt(exp, 10) != expStr {
		return Link{}, fmt.Errorf("%w: exp", ErrInvalid)
	}
	l.ExpiresAt = time.Unix(exp, 0)
	if name, ok := q["name"]; ok {
		if err := ValidateDeviceName(name); err != nil {
			return Link{}, fmt.Errorf("name: %w", err)
		}
		l.Name = name
	}
	if c, ok := frag["cap"]; ok {
		if err := ValidateCapability(c); err != nil {
			return Link{}, err
		}
		l.Capability = c
	}
	return l, nil
}

// strictParams parses k=v pairs separated by '&'. Every key must be in
// allowed, appear at most once, and have a value. Keys other than the
// optional "name" and "cap" are required.
func strictParams(raw string, allowed ...string) (map[string]string, error) {
	out := map[string]string{}
	if raw == "" {
		return nil, fmt.Errorf("%w: missing parameters", ErrInvalid)
	}
	for _, part := range strings.Split(raw, "&") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("%w: malformed parameter %q", ErrInvalid, k)
		}
		known := false
		for _, a := range allowed {
			known = known || a == k
		}
		if !known {
			return nil, fmt.Errorf("%w: unknown parameter %q", ErrInvalid, k)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("%w: duplicate parameter %q", ErrInvalid, k)
		}
		uv, err := url.QueryUnescape(v)
		if err != nil {
			return nil, fmt.Errorf("%w: parameter %q: %v", ErrInvalid, k, err)
		}
		out[k] = uv
	}
	for _, a := range allowed {
		if _, ok := out[a]; !ok && a != "name" && a != "cap" {
			return nil, fmt.Errorf("%w: missing parameter %q", ErrInvalid, a)
		}
	}
	return out, nil
}

func validateBroker(s string) error {
	bu, err := url.Parse(s)
	if err != nil || (bu.Scheme != "https" && bu.Scheme != "http") || bu.Host == "" ||
		bu.User != nil || bu.RawQuery != "" || bu.Fragment != "" || bu.Opaque != "" {
		return fmt.Errorf("%w: broker must be a plain http(s) URL", ErrInvalid)
	}
	return nil
}

// ValidateCapability checks that c is a plausible broker bearer capability:
// 1..MaxCapabilityBytes of base64url alphabet.
func ValidateCapability(c string) error {
	if c == "" || len(c) > MaxCapabilityBytes {
		return fmt.Errorf("%w: capability length", ErrInvalid)
	}
	for _, r := range c {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return fmt.Errorf("%w: capability must be base64url", ErrInvalid)
		}
	}
	return nil
}

package pairing

import (
	"crypto/ed25519"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	LinkScheme         = "termllm"
	LinkHost           = "pair"
	MaxLinkBytes       = 4096
	MaxCapabilityBytes = 128
)

// Link is the strict v2 invitation carried by a pairing QR code.
type Link struct {
	Broker               string
	HostKey              ed25519.PublicKey
	InvitationID         []byte
	ExpiresAt            time.Time
	HostLabel            string
	PairingSecret        []byte
	InvitationCapability string
}

// BrokerInvitationID returns the canonical broker invitation identifier.
func (l Link) BrokerInvitationID() string { return B64(l.InvitationID).String() }

func (l Link) query() string {
	var b strings.Builder
	b.WriteString("v=" + strconv.Itoa(WireVersion))
	b.WriteString("&b=" + url.QueryEscape(l.Broker))
	b.WriteString("&h=" + B64(l.HostKey).String())
	b.WriteString("&i=" + B64(l.InvitationID).String())
	b.WriteString("&e=" + strconv.FormatInt(l.ExpiresAt.Unix(), 10))
	if l.HostLabel != "" {
		b.WriteString("&n=" + url.QueryEscape(l.HostLabel))
	}
	return b.String()
}

func (l Link) String() string {
	s := LinkScheme + "://" + LinkHost + "?" + l.query() + "#s=" + B64(l.PairingSecret).String()
	if l.InvitationCapability != "" {
		s += "&c=" + l.InvitationCapability
	}
	return s
}

func (l Link) Redacted() string { return LinkScheme + "://" + LinkHost + "?" + l.query() }

// ParseLink strictly parses termllm://pair. The only accepted query names are
// v, broker, h, i, e, n; the only fragment names are s and c.
func ParseLink(raw string) (Link, error) {
	if len(raw) > MaxLinkBytes {
		return Link{}, fmt.Errorf("%w: link too long", ErrInvalid)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != LinkScheme || u.Host != LinkHost || u.Opaque != "" || u.User != nil || u.Path != "" || u.ForceQuery {
		return Link{}, fmt.Errorf("%w: not a %s://%s link", ErrInvalid, LinkScheme, LinkHost)
	}
	q, err := strictParams(u.RawQuery, []string{"v", "b", "h", "i", "e", "n"}, map[string]bool{"n": true})
	if err != nil {
		return Link{}, fmt.Errorf("query: %w", err)
	}
	frag, err := strictParams(u.EscapedFragment(), []string{"s", "c"}, map[string]bool{"c": true})
	if err != nil {
		return Link{}, fmt.Errorf("fragment: %w", err)
	}
	if q["v"] != strconv.Itoa(WireVersion) {
		return Link{}, fmt.Errorf("%w: unsupported link version", ErrInvalid)
	}
	l := Link{Broker: q["b"]}
	if err := validateBroker(l.Broker); err != nil {
		return Link{}, err
	}
	hostKey, err := DecodeB64(q["h"])
	if err != nil {
		return Link{}, fmt.Errorf("%w: h", ErrInvalid)
	}
	invitationID, err := DecodeB64(q["i"])
	if err != nil {
		return Link{}, fmt.Errorf("%w: i", ErrInvalid)
	}
	secret, err := DecodeB64(frag["s"])
	if err != nil {
		return Link{}, fmt.Errorf("%w: s", ErrInvalid)
	}
	if err := wantLen("h", hostKey, ed25519.PublicKeySize); err != nil {
		return Link{}, err
	}
	if err := wantLen("i", invitationID, InvitationIDSize); err != nil {
		return Link{}, err
	}
	if err := wantLen("s", secret, SecretSize); err != nil {
		return Link{}, err
	}
	l.HostKey, l.InvitationID, l.PairingSecret = hostKey, invitationID, secret
	exp, err := strconv.ParseInt(q["e"], 10, 64)
	if err != nil || exp <= 0 || strconv.FormatInt(exp, 10) != q["e"] {
		return Link{}, fmt.Errorf("%w: e", ErrInvalid)
	}
	l.ExpiresAt = time.Unix(exp, 0)
	if label, ok := q["n"]; ok {
		if err := ValidateDeviceName(label); err != nil {
			return Link{}, fmt.Errorf("n: %w", err)
		}
		l.HostLabel = label
	}
	if capability, ok := frag["c"]; ok {
		if err := ValidateCapability(capability); err != nil {
			return Link{}, err
		}
		l.InvitationCapability = capability
	}
	return l, nil
}

func strictParams(raw string, allowed []string, optional map[string]bool) (map[string]string, error) {
	if raw == "" {
		return nil, fmt.Errorf("%w: missing parameters", ErrInvalid)
	}
	out, known := map[string]string{}, map[string]bool{}
	for _, k := range allowed {
		known[k] = true
	}
	for _, part := range strings.Split(raw, "&") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("%w: malformed parameter", ErrInvalid)
		}
		if !known[k] {
			return nil, fmt.Errorf("%w: unknown parameter %q", ErrInvalid, k)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("%w: duplicate parameter %q", ErrInvalid, k)
		}
		value, err := url.QueryUnescape(v)
		if err != nil || url.QueryEscape(value) != v {
			return nil, fmt.Errorf("%w: non-canonical parameter %q", ErrInvalid, k)
		}
		out[k] = value
	}
	for _, k := range allowed {
		if _, ok := out[k]; !ok && !optional[k] {
			return nil, fmt.Errorf("%w: missing parameter %q", ErrInvalid, k)
		}
	}
	return out, nil
}

func validateBroker(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("%w: broker must be a plain http(s) URL", ErrInvalid)
	}
	return nil
}

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

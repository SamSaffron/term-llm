package peerbroker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
)

// PreissuedTURN distributes only parent-provisioned, already expiring test
// credentials. It never reads a TURN shared master key, generates credentials,
// contacts/configures/restarts a relay, or claims Cloudflare provenance.
type PreissuedTURN struct{ credentials ICECredentials }

// NewPreissuedTURNFile accepts a private JSON ICECredentials file with a finite
// expiry no more than 30m away. This is for the approved self-hosted demo only.
func NewPreissuedTURNFile(path string, now time.Time) (*PreissuedTURN, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 64<<10 {
		return nil, errors.New("preissued TURN credentials require a private regular file, at most 64KiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, actual) || !actual.Mode().IsRegular() || actual.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("preissued TURN file changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 64<<10 {
		return nil, errors.New("preissued TURN file too large")
	}
	var creds ICECredentials
	if err = json.Unmarshal(b, &creds); err != nil {
		return nil, errors.New("invalid preissued TURN credential JSON")
	}
	if !creds.ExpiresAt.After(now) || creds.ExpiresAt.After(now.Add(MaxTURNTTL)) || len(creds.ICEServers) == 0 || len(creds.ICEServers) > 16 {
		return nil, errors.New("preissued TURN credentials need 1-16 servers and expiry within 30m")
	}
	foundTURN := false
	for _, server := range creds.ICEServers {
		if len(server.URLs) == 0 || len(server.URLs) > 16 || len(server.Username) > 1024 || len(server.Credential) > 1024 {
			return nil, errors.New("invalid preissued ICE server limits")
		}
		for _, raw := range server.URLs {
			u, err := url.Parse(raw)
			if err != nil || len(raw) > 512 || strings.ContainsAny(raw, "\r\n\x00") || u.User != nil || u.Fragment != "" || u.Opaque == "" {
				return nil, errors.New("invalid preissued ICE URL")
			}
			switch u.Scheme {
			case "turn", "turns":
				if server.Username == "" || server.Credential == "" {
					return nil, errors.New("preissued TURN username and credential required")
				}
				foundTURN = true
			case "stun", "stuns":
			default:
				return nil, errors.New("preissued ICE URL must use stun/stuns/turn/turns")
			}
		}
	}
	if !foundTURN {
		return nil, errors.New("preissued credentials contain no TURN server")
	}
	return &PreissuedTURN{credentials: creds}, nil
}

func (p *PreissuedTURN) MintICEServers(ctx context.Context, ttl time.Duration) (*ICECredentials, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl < MinTURNTTL || ttl > MaxTURNTTL || time.Until(p.credentials.ExpiresAt) < ttl {
		return nil, errors.New("preissued self-hosted TURN credentials have insufficient remaining lifetime")
	}
	out := p.credentials
	out.ICEServers = make([]ICEServer, len(p.credentials.ICEServers))
	for i, s := range p.credentials.ICEServers {
		out.ICEServers[i] = s
		out.ICEServers[i].URLs = append([]string(nil), s.URLs...)
	}
	return &out, nil
}
func (*PreissuedTURN) String() string {
	return "peerbroker.PreissuedTURN{SELF-HOSTED, credentials:[REDACTED]}"
}
func (p *PreissuedTURN) GoString() string { return p.String() }

package peerbroker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultLeaseTTL     = 3 * time.Minute
	MinLeaseTTL         = time.Minute
	MaxLeaseTTL         = 10 * time.Minute
	DefaultChallengeTTL = 30 * time.Second
	MinInvitationTTL    = 10 * time.Second
	MaxInvitationTTL    = 10 * time.Minute
	MinCredentialLen    = 32
	maxCredentialLen    = 256
)

type CoturnConfig struct {
	URLs         []string
	SharedSecret string
	TTL          time.Duration
	Username     string
}

// TURNCredentialProvider mints bounded credentials for an operator-approved set
// of TURN URLs. Implementations may use local TURN-REST signing or a managed
// server-side API; provider secrets must never be returned in ICECredentials.
type TURNCredentialProvider interface {
	URLs() []string
	Mint(context.Context, string, []string) (ICECredentials, error)
}

func (c CoturnConfig) String() string {
	return fmt.Sprintf("peerbroker.CoturnConfig{URLs:%v, SharedSecret:[REDACTED], TTL:%s, Username:%q}", c.URLs, c.TTL, c.Username)
}
func (c CoturnConfig) GoString() string { return c.String() }

type Config struct {
	LeaseTTL     time.Duration
	ChallengeTTL time.Duration

	MaxHosts                int
	MaxChallenges           int
	MaxInvitationsPerHost   int
	MaxDevicesPerHost       int
	MaxConnections          int
	MaxSignalBytes          int
	MaxPendingSignals       int
	MaxSignalsPerConnection int
	MaxSignalsPerLease      int
	MaxICEMintsPerLease     int

	RequestRate          float64
	RequestBurst         int
	SignalRate           float64
	SignalBurst          int
	SignalBytesPerSecond float64
	SignalByteBurst      int

	AdmissionCredential string
	STUNURLs            []string
	Coturn              *CoturnConfig
	TURNProvider        TURNCredentialProvider
	Now                 func() time.Time
}

func (c Config) String() string {
	turn := "<nil>"
	if c.Coturn != nil {
		turn = c.Coturn.String()
	} else if c.TURNProvider != nil {
		turn = "<configured>"
	}
	admission := ""
	if c.AdmissionCredential != "" {
		admission = "[REDACTED]"
	}
	return fmt.Sprintf("peerbroker.Config{LeaseTTL:%s, AdmissionCredential:%s, STUNURLs:%v, Coturn:%s}", c.LeaseTTL, admission, c.STUNURLs, turn)
}
func (c Config) GoString() string { return c.String() }

func (c Config) withDefaults() (Config, error) {
	if c.LeaseTTL < 0 || c.ChallengeTTL < 0 || c.MaxHosts < 0 ||
		c.MaxChallenges < 0 || c.MaxInvitationsPerHost < 0 || c.MaxDevicesPerHost < 0 ||
		c.MaxConnections < 0 || c.MaxSignalBytes < 0 || c.MaxPendingSignals < 0 ||
		c.MaxSignalsPerConnection < 0 || c.MaxSignalsPerLease < 0 || c.MaxICEMintsPerLease < 0 || c.RequestRate < 0 ||
		c.RequestBurst < 0 || c.SignalRate < 0 || c.SignalBurst < 0 ||
		c.SignalBytesPerSecond < 0 || c.SignalByteBurst < 0 {
		return c, errors.New("peerbroker: negative config value")
	}
	c.LeaseTTL = defaultDuration(c.LeaseTTL, DefaultLeaseTTL)
	if c.LeaseTTL < MinLeaseTTL || c.LeaseTTL > MaxLeaseTTL {
		return c, fmt.Errorf("peerbroker: lease TTL must be between %s and %s", MinLeaseTTL, MaxLeaseTTL)
	}
	c.ChallengeTTL = defaultDuration(c.ChallengeTTL, DefaultChallengeTTL)
	if c.ChallengeTTL < 5*time.Second || c.ChallengeTTL > time.Minute {
		return c, errors.New("peerbroker: challenge TTL must be between 5s and 1m")
	}
	c.MaxHosts = defaultInt(c.MaxHosts, 256)
	c.MaxChallenges = defaultInt(c.MaxChallenges, 512)
	c.MaxInvitationsPerHost = defaultInt(c.MaxInvitationsPerHost, 16)
	c.MaxDevicesPerHost = defaultInt(c.MaxDevicesPerHost, 32)
	c.MaxConnections = defaultInt(c.MaxConnections, 512)
	c.MaxSignalBytes = min(defaultInt(c.MaxSignalBytes, 256<<10), 1<<20)
	c.MaxPendingSignals = min(defaultInt(c.MaxPendingSignals, 32), 256)
	c.MaxSignalsPerConnection = defaultInt(c.MaxSignalsPerConnection, 512)
	c.MaxSignalsPerLease = defaultInt(c.MaxSignalsPerLease, 4096)
	c.MaxICEMintsPerLease = defaultInt(c.MaxICEMintsPerLease, 8)
	if c.RequestRate == 0 {
		c.RequestRate = 50
	}
	c.RequestBurst = defaultInt(c.RequestBurst, 100)
	if c.SignalRate == 0 {
		c.SignalRate = 20
	}
	c.SignalBurst = defaultInt(c.SignalBurst, 40)
	if c.SignalBytesPerSecond == 0 {
		c.SignalBytesPerSecond = float64(c.MaxSignalBytes) * c.SignalRate
	}
	c.SignalByteBurst = max(defaultInt(c.SignalByteBurst, 2*c.MaxSignalBytes), c.MaxSignalBytes+1024)
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.AdmissionCredential != "" {
		if err := validateCredential(c.AdmissionCredential); err != nil {
			return c, fmt.Errorf("peerbroker: admission credential: %w", err)
		}
	}
	for _, raw := range c.STUNURLs {
		if err := validateICEURL(raw, false); err != nil {
			return c, fmt.Errorf("peerbroker: STUN URL: %w", err)
		}
	}
	if c.Coturn != nil && c.TURNProvider != nil {
		return c, errors.New("peerbroker: configure either coturn or a TURN credential provider, not both")
	}
	if c.Coturn != nil {
		coturn := *c.Coturn
		coturn.URLs = append([]string(nil), c.Coturn.URLs...)
		c.Coturn = &coturn
		if len(c.Coturn.URLs) == 0 {
			return c, errors.New("peerbroker: coturn requires at least one TURN URL")
		}
		if err := validateCredential(c.Coturn.SharedSecret); err != nil {
			return c, fmt.Errorf("peerbroker: coturn shared secret: %w", err)
		}
		for _, raw := range c.Coturn.URLs {
			if err := validateICEURL(raw, true); err != nil {
				return c, fmt.Errorf("peerbroker: TURN URL: %w", err)
			}
		}
		c.Coturn.TTL = defaultDuration(c.Coturn.TTL, time.Hour)
		if c.Coturn.TTL < time.Minute || c.Coturn.TTL > 24*time.Hour {
			return c, errors.New("peerbroker: coturn TTL must be between 1m and 24h")
		}
		if strings.ContainsAny(c.Coturn.Username, ":\r\n\x00") || len(c.Coturn.Username) > 64 {
			return c, errors.New("peerbroker: invalid coturn username")
		}
		c.TURNProvider = coturnRESTProvider{config: *c.Coturn, now: c.Now}
	} else if c.TURNProvider != nil {
		urls := c.TURNProvider.URLs()
		if len(urls) == 0 || len(urls) > 16 {
			return c, errors.New("peerbroker: TURN credential provider requires 1-16 TURN URLs")
		}
		for _, raw := range urls {
			if err := validateICEURL(raw, true); err != nil {
				return c, fmt.Errorf("peerbroker: TURN credential provider URL: %w", err)
			}
		}
	}
	return c, nil
}

func validateCredential(s string) error {
	if len(s) < MinCredentialLen || len(s) > maxCredentialLen {
		return fmt.Errorf("must be %d-%d printable non-space ASCII characters", MinCredentialLen, maxCredentialLen)
	}
	for i := range len(s) {
		if s[i] < 0x21 || s[i] > 0x7e {
			return errors.New("must contain only printable non-space ASCII")
		}
	}
	return nil
}

func validateICEURL(raw string, turn bool) error {
	if len(raw) > 512 || strings.ContainsAny(raw, "\r\n\x00") {
		return errors.New("invalid or oversized URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" || u.Opaque == "" {
		return errors.New("invalid URL")
	}
	if turn {
		if u.Scheme != "turn" && u.Scheme != "turns" {
			return errors.New("URL must use turn or turns")
		}
	} else if u.Scheme != "stun" && u.Scheme != "stuns" {
		return errors.New("URL must use stun or stuns")
	}
	return nil
}

// validateHostICEPolicy validates a host-requested ICE policy.
// It enforces limits on count, URL validity, credential source rules, and authority class consistency.
func validateHostICEPolicy(policy []wireICEServer, brokerTURNURLs []string) (HostICEConfig, error) {
	const maxServers = 8
	const maxURLsPerEntry = 8
	const maxTotalURLs = 16

	if len(policy) > maxServers {
		return HostICEConfig{}, fmt.Errorf("too many ICE server entries (max %d)", maxServers)
	}

	var totalURLs int
	servers := make([]ConfiguredICEServer, 0, len(policy))

	for i, wire := range policy {
		if len(wire.URLs) == 0 {
			return HostICEConfig{}, fmt.Errorf("server[%d]: no URLs", i)
		}
		if len(wire.URLs) > maxURLsPerEntry {
			return HostICEConfig{}, fmt.Errorf("server[%d]: too many URLs (max %d)", i, maxURLsPerEntry)
		}
		totalURLs += len(wire.URLs)
		if totalURLs > maxTotalURLs {
			return HostICEConfig{}, fmt.Errorf("too many total URLs (max %d)", maxTotalURLs)
		}

		// Determine authority class from first URL
		isSTUN, isTURN := false, false
		for j, raw := range wire.URLs {
			if len(raw) > 512 || strings.ContainsAny(raw, "\r\n\x00") {
				return HostICEConfig{}, fmt.Errorf("server[%d].urls[%d]: invalid URL", i, j)
			}
			u, err := url.Parse(raw)
			if err != nil {
				return HostICEConfig{}, fmt.Errorf("server[%d].urls[%d]: invalid URL", i, j)
			}
			// Check scheme first
			switch u.Scheme {
			case "stun", "stuns":
				isSTUN = true
			case "turn", "turns":
				isTURN = true
			default:
				return HostICEConfig{}, fmt.Errorf("server[%d].urls[%d]: invalid scheme %q", i, j, u.Scheme)
			}
			if u.User != nil || u.Fragment != "" || u.Opaque == "" {
				return HostICEConfig{}, fmt.Errorf("server[%d].urls[%d]: URL must not have userinfo or fragment", i, j)
			}
		}

		// Mixed STUN/TURN in a single entry is forbidden
		if isSTUN && isTURN {
			return HostICEConfig{}, fmt.Errorf("server[%d]: mixed STUN and TURN URLs", i)
		}

		// Validate credential source
		switch wire.CredentialSource {
		case "":
			// STUN only
			if !isSTUN {
				return HostICEConfig{}, fmt.Errorf("server[%d]: empty credential_source requires STUN URLs", i)
			}
			if wire.Username != "" || wire.Credential != "" {
				return HostICEConfig{}, fmt.Errorf("server[%d]: STUN entry must not have username or credential", i)
			}
			servers = append(servers, ConfiguredICEServer{URLs: append([]string(nil), wire.URLs...)})

		case "broker":
			// TURN only, broker-minted credentials
			if !isTURN {
				return HostICEConfig{}, fmt.Errorf("server[%d]: credential_source=broker requires TURN URLs", i)
			}
			if wire.Username != "" || wire.Credential != "" {
				return HostICEConfig{}, fmt.Errorf("server[%d]: broker entry must not have username or credential", i)
			}
			// Validate that all URLs match the configured credential provider.
			for _, reqURL := range wire.URLs {
				if !urlInList(reqURL, brokerTURNURLs) {
					return HostICEConfig{}, fmt.Errorf("server[%d]: broker TURN URL not in configured credential provider URLs", i)
				}
			}
			servers = append(servers, ConfiguredICEServer{
				URLs:             append([]string(nil), wire.URLs...),
				CredentialSource: CredentialSourceBroker,
			})

		case "static":
			// TURN only, static credentials
			if !isTURN {
				return HostICEConfig{}, fmt.Errorf("server[%d]: credential_source=static requires TURN URLs", i)
			}
			if wire.Username == "" || wire.Credential == "" {
				return HostICEConfig{}, fmt.Errorf("server[%d]: static entry requires username and credential", i)
			}
			if strings.ContainsAny(wire.Username, "\r\n\x00") || len(wire.Username) > 256 {
				return HostICEConfig{}, fmt.Errorf("server[%d]: invalid username", i)
			}
			if strings.ContainsAny(wire.Credential, "\r\n\x00") || len(wire.Credential) > 256 {
				return HostICEConfig{}, fmt.Errorf("server[%d]: invalid credential", i)
			}
			servers = append(servers, ConfiguredICEServer{
				URLs:             append([]string(nil), wire.URLs...),
				CredentialSource: CredentialSourceStatic,
				Username:         wire.Username,
				Credential:       wire.Credential,
			})

		default:
			return HostICEConfig{}, fmt.Errorf("server[%d]: invalid credential_source %q", i, wire.CredentialSource)
		}
	}

	return NewHostICEConfig(servers), nil
}

// urlInList checks if a URL exactly matches one in the list.
func urlInList(needle string, haystack []string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

func defaultInt(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}
func defaultDuration(v, d time.Duration) time.Duration {
	if v == 0 {
		return d
	}
	return v
}

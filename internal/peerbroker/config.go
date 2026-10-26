package peerbroker

import (
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
	Now                 func() time.Time
}

func (c Config) String() string {
	turn := "<nil>"
	if c.Coturn != nil {
		turn = c.Coturn.String()
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

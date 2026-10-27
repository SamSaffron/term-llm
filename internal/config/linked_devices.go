package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// LinkedDevicesConfig configures the linked-device peer broker and ICE servers.
type LinkedDevicesConfig struct {
	BrokerURL           string                         `mapstructure:"broker_url" yaml:"broker_url,omitempty"`
	AdmissionCredential string                         `mapstructure:"admission_credential" yaml:"admission_credential,omitempty"`
	Diagnostics         bool                           `mapstructure:"diagnostics" yaml:"diagnostics"`
	ICEServers          []LinkedDevicesICEServerConfig `mapstructure:"ice_servers" yaml:"ice_servers"`
}

// LinkedDevicesICEServerConfig describes a single ICE server (STUN/TURN) entry.
type LinkedDevicesICEServerConfig struct {
	URLs             []string `mapstructure:"urls" yaml:"urls" json:"urls"`
	CredentialSource string   `mapstructure:"credential_source" yaml:"credential_source,omitempty" json:"credential_source,omitempty"`
	Username         string   `mapstructure:"username" yaml:"username,omitempty" json:"username,omitempty"`
	Credential       string   `mapstructure:"credential" yaml:"credential,omitempty" json:"credential,omitempty"`
}

func (c LinkedDevicesConfig) String() string {
	admission := ""
	if c.AdmissionCredential != "" {
		admission = "[REDACTED]"
	}
	return fmt.Sprintf("config.LinkedDevicesConfig{BrokerURL:%q, AdmissionCredential:%s, Diagnostics:%t, ICEServers:%v}", c.BrokerURL, admission, c.Diagnostics, c.ICEServers)
}

func (c LinkedDevicesConfig) GoString() string { return c.String() }

func (c LinkedDevicesICEServerConfig) String() string {
	username, credential := "", ""
	if c.Username != "" {
		username = "[REDACTED]"
	}
	if c.Credential != "" {
		credential = "[REDACTED]"
	}
	return fmt.Sprintf("config.LinkedDevicesICEServerConfig{URLs:%v, CredentialSource:%q, Username:%s, Credential:%s}", c.URLs, c.CredentialSource, username, credential)
}

func (c LinkedDevicesICEServerConfig) GoString() string { return c.String() }

// initializeLinkedDevices applies defaults and validates the linked_devices configuration.
func (c *Config) initializeLinkedDevices() error {
	if c == nil {
		return nil
	}
	// Apply LinkedDevices defaults when ICEServers is nil (omitted from config).
	// An explicit ice_servers: [] preserves the empty list.
	if c.LinkedDevices.ICEServers == nil {
		c.LinkedDevices.ICEServers = DefaultLinkedDevicesICEServers()
	}
	return c.ValidateLinkedDevices()
}

// ValidateLinkedDevices validates the linked_devices configuration without resolving
// deferred credentials. It enforces limits, URL schemes, and credential source rules.
func (c *Config) ValidateLinkedDevices() error {
	if c == nil {
		return nil
	}
	ld := &c.LinkedDevices
	if brokerURL := strings.TrimSpace(ld.BrokerURL); brokerURL != "" && !linkedDevicesDeferredValue(brokerURL) {
		if err := validateLinkedDevicesBrokerURL(brokerURL); err != nil {
			return fmt.Errorf("linked_devices.broker_url: %w", err)
		}
	}
	if credential := strings.TrimSpace(ld.AdmissionCredential); credential != "" && !linkedDevicesDeferredValue(credential) {
		if err := validateResolvedBrokerCredential(credential); err != nil {
			return fmt.Errorf("linked_devices.admission_credential: %w", err)
		}
	}

	// Validate server count and URL limits
	if len(ld.ICEServers) > 8 {
		return fmt.Errorf("linked_devices.ice_servers: at most 8 server entries allowed, got %d", len(ld.ICEServers))
	}

	totalURLs := 0
	for i, server := range ld.ICEServers {
		if len(server.URLs) == 0 {
			return fmt.Errorf("linked_devices.ice_servers[%d].urls: cannot be empty", i)
		}
		if len(server.URLs) > 8 {
			return fmt.Errorf("linked_devices.ice_servers[%d].urls: at most 8 URLs per entry allowed, got %d", i, len(server.URLs))
		}
		totalURLs += len(server.URLs)

		// Validate credential_source and detect URL class
		source := strings.TrimSpace(server.CredentialSource)
		if source != server.CredentialSource {
			return fmt.Errorf("linked_devices.ice_servers[%d].credential_source: surrounding whitespace is not allowed", i)
		}
		var isSTUN, isTURN bool

		for j, rawURL := range server.URLs {
			if len(rawURL) > 512 {
				return fmt.Errorf("linked_devices.ice_servers[%d].urls[%d]: URL length %d exceeds 512 bytes", i, j, len(rawURL))
			}

			// Check for control characters
			if containsControlChars(rawURL) {
				return fmt.Errorf("linked_devices.ice_servers[%d].urls[%d]: URL contains control characters", i, j)
			}

			u, err := parseICEURL(rawURL)
			if err != nil {
				return fmt.Errorf("linked_devices.ice_servers[%d].urls[%d]: %w", i, j, err)
			}

			// Detect class
			switch u.Scheme {
			case "stun", "stuns":
				isSTUN = true
			case "turn", "turns":
				isTURN = true
			default:
				return fmt.Errorf("linked_devices.ice_servers[%d].urls[%d]: invalid scheme %q, expected stun, stuns, turn, or turns", i, j, u.Scheme)
			}
			if u.Opaque == "" {
				return fmt.Errorf("linked_devices.ice_servers[%d].urls[%d]: invalid ICE URL", i, j)
			}
		}

		// Reject mixed STUN/TURN entries
		if isSTUN && isTURN {
			return fmt.Errorf("linked_devices.ice_servers[%d]: cannot mix STUN and TURN URLs in a single entry", i)
		}

		// Validate credential_source rules
		switch source {
		case "":
			// STUN only
			if isTURN {
				return fmt.Errorf("linked_devices.ice_servers[%d]: TURN URLs require credential_source (broker or static)", i)
			}
			if server.Username != "" || server.Credential != "" {
				return fmt.Errorf("linked_devices.ice_servers[%d]: username and credential forbidden for STUN URLs", i)
			}
		case "broker":
			// TURN only, no username/credential
			if !isTURN {
				return fmt.Errorf("linked_devices.ice_servers[%d]: credential_source=broker requires TURN URLs", i)
			}
			if server.Username != "" || server.Credential != "" {
				return fmt.Errorf("linked_devices.ice_servers[%d]: username and credential forbidden for credential_source=broker", i)
			}
		case "static":
			// TURN only, both username and credential required
			if !isTURN {
				return fmt.Errorf("linked_devices.ice_servers[%d]: credential_source=static requires TURN URLs", i)
			}
			if strings.TrimSpace(server.Username) == "" {
				return fmt.Errorf("linked_devices.ice_servers[%d]: credential_source=static requires username", i)
			}
			if strings.TrimSpace(server.Credential) == "" {
				return fmt.Errorf("linked_devices.ice_servers[%d]: credential_source=static requires credential", i)
			}
			if !linkedDevicesDeferredValue(server.Username) {
				if err := validateResolvedStaticValue(server.Username); err != nil {
					return fmt.Errorf("linked_devices.ice_servers[%d].username: %w", i, err)
				}
			}
			if !linkedDevicesDeferredValue(server.Credential) {
				if err := validateResolvedStaticValue(server.Credential); err != nil {
					return fmt.Errorf("linked_devices.ice_servers[%d].credential: %w", i, err)
				}
			}
		default:
			return fmt.Errorf("linked_devices.ice_servers[%d]: invalid credential_source %q, expected empty, broker, or static", i, server.CredentialSource)
		}
	}

	if totalURLs > 16 {
		return fmt.Errorf("linked_devices.ice_servers: total URL count %d exceeds maximum of 16", totalURLs)
	}

	return nil
}

// parseICEURL validates an ICE URL format without parsing userinfo or fragment.
func parseICEURL(rawURL string) (*url.URL, error) {
	// Check for userinfo (@ before first /) and fragment (#) directly
	// since url.Parse doesn't reliably detect these for non-HTTP schemes
	schemeEnd := strings.Index(rawURL, ":")
	if schemeEnd < 0 {
		return nil, fmt.Errorf("invalid URL: missing scheme")
	}

	remainder := rawURL[schemeEnd+1:]

	// Check for fragment
	if strings.Contains(remainder, "#") {
		return nil, fmt.Errorf("URL fragment not allowed")
	}

	// Check for userinfo: @ before first /
	slashPos := strings.Index(remainder, "/")
	checkPart := remainder
	if slashPos >= 0 {
		checkPart = remainder[:slashPos]
	}
	if strings.Contains(checkPart, "@") {
		return nil, fmt.Errorf("URL userinfo not allowed")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid ICE URL")
	}
	return u, nil
}

// containsControlChars returns true if s contains ASCII control characters.
func containsControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7F {
			return true
		}
	}
	return false
}

// ResolveLinkedDevices resolves deferred credentials in linked_devices configuration.
// It applies the broker URL precedence (see LinkedDevicesBrokerURL) and only
// resolves broker_url, admission_credential, and static TURN credentials.
// This should only be called when the runtime is actually being constructed.
func (c *Config) ResolveLinkedDevices() error {
	if c == nil {
		return nil
	}
	ld := &c.LinkedDevices
	ld.BrokerURL = c.LinkedDevicesBrokerURL()
	if strings.TrimSpace(ld.AdmissionCredential) == "" {
		ld.AdmissionCredential = strings.TrimSpace(os.Getenv("TERM_LLM_LINKED_DEVICES_ADMISSION"))
	}

	// Resolve broker_url if set
	if strings.TrimSpace(ld.BrokerURL) != "" {
		resolved, err := ResolveValue(ld.BrokerURL)
		if err != nil {
			return fmt.Errorf("linked_devices.broker_url: %w", err)
		}
		if strings.TrimSpace(resolved) == "" {
			return fmt.Errorf("linked_devices.broker_url: resolved value is empty")
		}
		if err := validateLinkedDevicesBrokerURL(resolved); err != nil {
			return fmt.Errorf("linked_devices.broker_url: %w", err)
		}
		ld.BrokerURL = resolved
	}

	// Resolve admission_credential if set.
	if strings.TrimSpace(ld.AdmissionCredential) != "" {
		resolved, err := ResolveValue(ld.AdmissionCredential)
		if err != nil {
			return fmt.Errorf("linked_devices.admission_credential: %w", err)
		}
		if err := validateResolvedBrokerCredential(resolved); err != nil {
			return fmt.Errorf("linked_devices.admission_credential: %w", err)
		}
		ld.AdmissionCredential = resolved
	}

	// Resolve static TURN credentials
	for i, server := range ld.ICEServers {
		if strings.TrimSpace(server.CredentialSource) != "static" {
			continue
		}

		// Resolve username
		username, err := ResolveValue(server.Username)
		if err != nil {
			return fmt.Errorf("linked_devices.ice_servers[%d].username: %w", i, err)
		}
		if err := validateResolvedStaticValue(username); err != nil {
			return fmt.Errorf("linked_devices.ice_servers[%d].username: %w", i, err)
		}
		ld.ICEServers[i].Username = username

		// Resolve credential
		credential, err := ResolveValue(server.Credential)
		if err != nil {
			return fmt.Errorf("linked_devices.ice_servers[%d].credential: %w", i, err)
		}
		if err := validateResolvedStaticValue(credential); err != nil {
			return fmt.Errorf("linked_devices.ice_servers[%d].credential: %w", i, err)
		}
		ld.ICEServers[i].Credential = credential
	}

	return nil
}

func validateLinkedDevicesBrokerURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must be scheme://host[/path] without credentials, query, or fragment")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && linkedDevicesLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("HTTPS is required except for loopback")
}

func linkedDevicesLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func linkedDevicesDeferredValue(value string) bool {
	value = strings.TrimSpace(value)
	return needsLazyResolve(value) || envRefName(value) != ""
}

func validateResolvedStaticValue(value string) error {
	if len(value) == 0 || len(value) > 256 {
		return fmt.Errorf("resolved value must be 1-256 bytes")
	}
	if containsControlChars(value) {
		return fmt.Errorf("resolved value contains control characters")
	}
	return nil
}

func validateResolvedBrokerCredential(value string) error {
	if len(value) < 32 || len(value) > 256 {
		return fmt.Errorf("resolved value must be 32-256 printable non-space ASCII characters")
	}
	for i := range len(value) {
		if value[i] < 0x21 || value[i] > 0x7e {
			return fmt.Errorf("resolved value must contain only printable non-space ASCII")
		}
	}
	return nil
}

// LinkedDevicesBrokerURL returns the unresolved rendezvous broker URL with
// precedence explicit linked_devices.broker_url > TERM_LLM_LINKED_DEVICES_BROKER_URL
// > DefaultLinkedDevicesBrokerURL. Deferred references are resolved later by
// ResolveLinkedDevices.
func (c *Config) LinkedDevicesBrokerURL() string {
	if c == nil {
		return ""
	}
	if explicit := strings.TrimSpace(c.LinkedDevices.BrokerURL); explicit != "" {
		return explicit
	}
	if env := strings.TrimSpace(os.Getenv("TERM_LLM_LINKED_DEVICES_BROKER_URL")); env != "" {
		return env
	}
	return DefaultLinkedDevicesBrokerURL
}

// LinkedDevicesEnabled reports whether a rendezvous broker is available to the
// linked devices runtime. The hosted default makes this true for an ordinary
// configuration; endpoint availability is not sharing consent, which still
// requires the owner to select Connect.
func (c *Config) LinkedDevicesEnabled() bool {
	return c.LinkedDevicesBrokerURL() != ""
}

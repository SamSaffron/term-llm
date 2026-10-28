package serve

import (
	"fmt"
	"strings"
	"time"
)

type Options struct {
	Host                   string
	Port                   int
	Token                  string
	AllowNoAuth            bool
	AuthMode               string
	BasePath               string
	PublicURL              string
	Title                  string
	DisableLocationSharing bool
	CORSOrigins            []string
	SessionTTL             time.Duration
	SessionMax             int
	Debug                  bool
	Search                 bool
	NoSearch               bool
	Provider               string
	MCP                    string
	NativeSearch           bool
	NoNativeSearch         bool
	MaxTurns               int
	Tools                  string
	ReadDirs               []string
	WriteDirs              []string
	ShellAllow             []string
	SystemMessage          string
	AllowProjectSkillTools *bool
	Agent                  string
	Approval               string
	Yolo                   bool
	Auto                   bool
	TelegramCarryoverChars int
	JobsWorkers            int
	Setup                  bool
	Verbose                bool
	FilterServerTools      bool
	SidebarSessions        string
	ToolMap                []string
	FilesDir               string
	DisableWidgets         bool
	WidgetsDir             string
	ExtensionsDir          string
	ExtensionIDs           []string
	DisableExtensions      bool
	ResponseTimeout        time.Duration
	EnableFileTracking     bool
	HubURL                 string
	HubNodeID              string
	HubNodeName            string
	HubConnect             string
	HubRegister            bool
	HubRegistrationToken   string
	Projects               bool
	NoProjects             bool
	PasskeyAuthFile        string
	PasskeyBootstrapFile   string
	PasskeyRecoveryFile    string
	PrintPasskeyBootstrap  bool
	PasskeyTrustedProxies  []string
}

// Validate checks the server limits before configuration or listener setup.
// responseTimeoutChanged distinguishes an explicit invalid flag from its
// configurable default, matching Cobra's flag precedence.
func (o Options) Validate(responseTimeoutChanged bool) error {
	if o.Port <= 0 || o.Port > 65535 {
		return fmt.Errorf("invalid --port %d (must be 1-65535)", o.Port)
	}
	if o.SessionTTL <= 0 {
		return fmt.Errorf("invalid --session-ttl %s (must be > 0)", o.SessionTTL)
	}
	if o.SessionMax <= 0 {
		return fmt.Errorf("invalid --session-max %d (must be > 0)", o.SessionMax)
	}
	if o.TelegramCarryoverChars < 0 {
		return fmt.Errorf("invalid --telegram-carryover-chars %d (must be >= 0)", o.TelegramCarryoverChars)
	}
	if o.JobsWorkers <= 0 {
		return fmt.Errorf("invalid --jobs-workers %d (must be > 0)", o.JobsWorkers)
	}
	if responseTimeoutChanged && o.ResponseTimeout <= 0 {
		return fmt.Errorf("invalid --response-timeout %s (must be > 0)", o.ResponseTimeout)
	}
	return nil
}

func ParseToolMap(entries []string) (map[string]string, error) {
	var toolMap map[string]string
	for _, entry := range entries {
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid --tool-map %q (expected ClientName:ServerName)", entry)
		}
		if toolMap == nil {
			toolMap = make(map[string]string)
		}
		toolMap[parts[0]] = parts[1]
	}
	return toolMap, nil
}

func ResolveAuthMode(authFlagSet bool, authMode string, allowNoAuthSet bool, allowNoAuth bool) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(authMode))
	if mode == "" {
		mode = "bearer"
	}
	if mode != "bearer" && mode != "none" && mode != "passkey" {
		return "", fmt.Errorf("invalid --auth %q (must be bearer, passkey, or none)", authMode)
	}

	if allowNoAuthSet {
		aliasMode := "bearer"
		if allowNoAuth {
			aliasMode = "none"
		}
		if authFlagSet && mode != aliasMode {
			return "", fmt.Errorf("--auth %s conflicts with --no-auth=%v", mode, allowNoAuth)
		}
		mode = aliasMode
	}

	return mode, nil
}

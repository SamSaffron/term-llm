package linkeddevices

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// LiveStartRequest is the shared wire contract for starting owner or linked Live.
// Linked admission applies additional constraints before this request is used.
type LiveStartRequest struct {
	SDP            string `json:"sdp"`
	SessionID      string `json:"session_id"`
	AudioTransport string `json:"audio_transport"`
	// DelegationMode selects who executes this call's delegations: the host
	// ("server", the default) or the authenticated client that started the call
	// ("client").
	DelegationMode string `json:"delegation_mode"`
	// DelegationContext describes what the device executing delegations can do. It
	// is prompt text, not a tool schema: tool authority stays on the device, which
	// declares its real tools per request.
	DelegationContext string `json:"delegation_context"`
	// ClientTools are the page tools (Responses function definitions) a server-mode
	// client offers its delegated turns and runs for them. Declaring them here,
	// rather than after the call starts, is what lets the first spoken request use
	// them.
	ClientTools []json.RawMessage `json:"client_tools"`
}

// Delegation validates the delegation half of a start request. Mode and
// context are validated together because the pairing is the security-relevant
// part: a capability hint accepted in server mode would make the host promise
// device tools no delegation can ever reach.
func (request LiveStartRequest) Delegation(contextLimit int) (string, string, error) {
	mode := strings.TrimSpace(request.DelegationMode)
	switch mode {
	case "":
		mode = "server"
	case "server", "client":
	default:
		return "", "", fmt.Errorf("delegation_mode must be %q or %q", "server", "client")
	}
	hint := strings.TrimSpace(request.DelegationContext)
	if len(hint) > contextLimit {
		return "", "", fmt.Errorf("delegation_context must be at most %d bytes", contextLimit)
	}
	if !liveContextIsPlainText(hint) {
		return "", "", errors.New("delegation_context must be plain text")
	}
	if hint != "" && mode != "client" {
		return "", "", errors.New(`delegation_context is only accepted with "delegation_mode":"client"`)
	}
	return mode, hint, nil
}

// liveContextIsPlainText rejects control characters other than the
// newline a client may reasonably use to separate sentences. The prompt layer
// sanitises again; this is the transport saying no to bytes that only make sense
// as an attempt to forge structure in a prompt or a log line.
func liveContextIsPlainText(hint string) bool {
	if !utf8.ValidString(hint) {
		return false
	}
	for _, r := range hint {
		if r == '\n' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

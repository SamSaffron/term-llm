package cmd

import "github.com/samsaffron/term-llm/internal/app/serve/protocol"

// Keep the existing shell/controller integration while the protocol implementation
// and its tests live in a separately testable, dependency-free package.
const (
	serveShellProtocolNonceSize = protocol.NonceSize
	serveShellCommandBytes      = protocol.CommandBytes
	serveShellPhysicalLineBytes = protocol.PhysicalLineBytes
	serveShellCaptureBytes      = protocol.CaptureBytes
)

type serveShellProtocolMarker = protocol.Marker
type serveShellProtocolParser = protocol.Parser

func newServeShellProtocolNonce() (string, error)       { return protocol.NewNonce() }
func buildServeShellProbe(nonce string) ([]byte, error) { return protocol.BuildProbe(nonce) }
func buildServeShellCommandPayload(nonce, command string) ([]byte, error) {
	return protocol.BuildCommandPayload(nonce, command)
}
func sanitizeServeShellText(raw []byte, limit int) (string, bool) {
	return protocol.SanitizeText(raw, limit)
}

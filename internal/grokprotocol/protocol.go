// Package grokprotocol defines wire metadata shared by Grok OAuth and
// subscription proxy clients.
package grokprotocol

const (
	// ClientVersion is the Grok subscription proxy compatibility version. It is
	// sent as x-grok-client-version and is not term-llm's release version.
	ClientVersion = "1.0.6"

	ClientIdentifier   = "term-llm"
	ClientSurfaceCLI   = "cli"
	ClientModeHeadless = "headless"
)

package cmd

import (
	"context"
	"fmt"

	"github.com/samsaffron/term-llm/internal/peerbroker"
	"github.com/spf13/cobra"
)

type peerBrokerOptions struct {
	host                                 string
	port                                 int
	registrationFile                     string
	tlsCert, tlsKey                      string
	cloudflareKeyID, cloudflareTokenFile string
	maxSessions                          int
}

var peerBrokerFlags = peerBrokerOptions{}
var servePeerBrokerCmd = &cobra.Command{
	Use: "peer-broker", Short: "Run an authenticated bounded peer signalling broker (foreground)",
	Long: `Run the peer pilot signalling API and WebSocket endpoint. This broker never
receives the pairing PSK and has no HTTP/chat payload relay. A private pilot
registration capability is required; per-peer capabilities gate all signalling.

Default binding is loopback for an approved HTTPS reverse proxy. Non-loopback
binds require --tls-cert and --tls-key. Cloudflare TURN minting is disabled unless
both key ID and private API token file are supplied. Starting this command is a
separate operator action; contain pair never installs or deploys this service.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPeerBroker(cmd.Context(), peerBrokerFlags, func(message string) { fmt.Fprintln(cmd.ErrOrStderr(), message) })
	},
}

func init() {
	serveCmd.AddCommand(servePeerBrokerCmd)
	f := servePeerBrokerCmd.Flags()
	f.StringVar(&peerBrokerFlags.host, "host", "127.0.0.1", "Bind host (public bind requires TLS)")
	f.IntVar(&peerBrokerFlags.port, "port", 8091, "Bind port")
	f.StringVar(&peerBrokerFlags.registrationFile, "registration-token-file", "", "Private file containing pilot registration capability")
	f.StringVar(&peerBrokerFlags.tlsCert, "tls-cert", "", "TLS certificate file")
	f.StringVar(&peerBrokerFlags.tlsKey, "tls-key", "", "TLS private key file")
	f.StringVar(&peerBrokerFlags.cloudflareKeyID, "cloudflare-turn-key-id", "", "Cloudflare TURN key ID (minting disabled by default)")
	f.StringVar(&peerBrokerFlags.cloudflareTokenFile, "cloudflare-turn-api-token-file", "", "Private file containing Cloudflare TURN API token")
	f.IntVar(&peerBrokerFlags.maxSessions, "max-sessions", 16, "Concurrent rendezvous limit (1-64 for pilot)")
	_ = servePeerBrokerCmd.MarkFlagRequired("registration-token-file")
}
func peerBrokerServeOptions(o peerBrokerOptions) peerbroker.ServeOptions {
	return peerbroker.ServeOptions{Host: o.host, Port: o.port, RegistrationFile: o.registrationFile, TLSCert: o.tlsCert, TLSKey: o.tlsKey, CloudflareKeyID: o.cloudflareKeyID, CloudflareTokenFile: o.cloudflareTokenFile, MaxSessions: o.maxSessions}
}
func validatePeerBrokerOptions(o peerBrokerOptions) error {
	return peerBrokerServeOptions(o).Validate()
}
func runPeerBroker(ctx context.Context, o peerBrokerOptions, status func(string)) error {
	return peerbroker.Run(ctx, peerBrokerServeOptions(o), status)
}

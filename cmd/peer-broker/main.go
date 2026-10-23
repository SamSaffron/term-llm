// peer-broker is the small standalone signalling-only pilot executable. It has
// no dependency on the core CLI, Hub, agent engine, frontend or Pion WebRTC.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/samsaffron/term-llm/internal/peerbroker"
)

func main() {
	var o peerbroker.ServeOptions
	flag.StringVar(&o.Host, "host", "127.0.0.1", "Bind host (public binding requires TLS)")
	flag.IntVar(&o.Port, "port", 8091, "Bind port")
	flag.StringVar(&o.RegistrationFile, "registration-token-file", "", "Private pilot registration capability file (required)")
	flag.StringVar(&o.TLSCert, "tls-cert", "", "TLS certificate file")
	flag.StringVar(&o.TLSKey, "tls-key", "", "TLS private key file")
	flag.StringVar(&o.CloudflareKeyID, "cloudflare-turn-key-id", "", "Optional Cloudflare TURN key ID")
	flag.StringVar(&o.CloudflareTokenFile, "cloudflare-turn-api-token-file", "", "Private Cloudflare TURN API token file")
	flag.IntVar(&o.MaxSessions, "max-sessions", 16, "Concurrent rendezvous cap (1-64)")
	flag.StringVar(&o.PreissuedTURNFile, "turn-credentials-file", "", "Private preissued finite SELF-HOSTED demo ICE credentials JSON (not a master key, NOT Cloudflare)")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err := peerbroker.Run(ctx, o, func(message string) { fmt.Fprintln(os.Stderr, message) })
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

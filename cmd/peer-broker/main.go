// peer-broker runs the standalone version 2 linked-device rendezvous broker.
// term-llm's hosted default rendezvous runs it with --public; it embeds no
// TURN relay, credential issuer or deployment automation.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/samsaffron/term-llm/internal/peerbroker"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("URL cannot be empty")
	}
	*s = append(*s, value)
	return nil
}

func main() {
	var options peerbroker.ServeOptions
	var stun, turn, proxies stringList
	flag.StringVar(&options.Host, "host", "127.0.0.1", "Bind host (TLS required off loopback)")
	flag.IntVar(&options.Port, "port", 8091, "Bind port")
	flag.StringVar(&options.TLSCert, "tls-cert", "", "TLS certificate file")
	flag.StringVar(&options.TLSKey, "tls-key", "", "TLS private-key file")
	flag.StringVar(&options.AdmissionFile, "admission-credential-file", "", "Optional private admission credential file")
	flag.Var(&stun, "stun", "Static STUN URL (repeatable; no default)")
	flag.Var(&turn, "turn", "coturn TURN URL (repeatable; no default)")
	flag.StringVar(&options.TURNSecretFile, "turn-rest-secret-file", "", "Private coturn TURN REST shared-secret file")
	flag.StringVar(&options.TURNUsername, "turn-username", "", "Optional coturn username namespace")
	flag.DurationVar(&options.TURNTTL, "turn-ttl", time.Hour, "Minted coturn credential lifetime")
	flag.DurationVar(&options.LeaseTTL, "lease-ttl", peerbroker.DefaultLeaseTTL, "Renewable host lease lifetime")
	flag.IntVar(&options.MaxHosts, "max-hosts", 256, "Concurrent host lease limit")
	flag.BoolVar(&options.Public, "public", false, "Open host admission with per-client-network rate, quota and socket-lifetime limits")
	flag.Var(&proxies, "trusted-proxy", "Reverse-proxy IP or CIDR whose X-Real-IP header names the client (repeatable)")
	flag.DurationVar(&options.MaxConnectionLifetime, "max-connection-lifetime", 0, "Close WebSockets after this duration so clients reconnect (0: 12h with --public, otherwise unbounded)")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}
	options.STUNURLs, options.TURNURLs, options.TrustedProxies = stun, turn, proxies
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := peerbroker.Run(ctx, options, func(message string) { fmt.Fprintln(os.Stderr, message) }); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

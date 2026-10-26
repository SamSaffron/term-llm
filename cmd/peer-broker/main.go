// peer-broker runs the standalone version 2 mobile-share rendezvous broker.
// It provides no hosted defaults or deployment automation.
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
	var stun, turn stringList
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
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}
	options.STUNURLs, options.TURNURLs = stun, turn
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := peerbroker.Run(ctx, options, func(message string) { fmt.Fprintln(os.Stderr, message) }); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

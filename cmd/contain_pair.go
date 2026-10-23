package cmd

import (
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
	"github.com/spf13/cobra"
	"time"
)

var containPairOptions = apphub.Options{Host: "127.0.0.1", Port: 8090, Contain: true, AuthMode: "bearer", BasePath: "/"}

var containPairCmd = &cobra.Command{
	Use:   "pair",
	Short: "Pair the local contain Hub with the native app over WebRTC (foreground pilot)",
	Long: `Run a local Hub over existing contain workspaces and pair a native device.

Requires an approved HTTPS signalling broker and a private registration capability
file. The one-use native link contains a fresh pairing secret, not a Hub token.
Review and approve the device key in this terminal before any agent access.

Pilot scope: Hub reads and agent chat on all discovered nodes. Hub security and
node-registration management are excluded. Device grants expire after 24h, but
reconnect discovery is limited to the original live rendezvous (at most 30m).
Host restart or rendezvous expiry requires new pairing. This command does not
create containers, install services, deploy a broker, or configure TURN.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return appserve.RunHub(cmd.Context(), containPairOptions, apphub.Invocation{Stdin: cmd.InOrStdin(), Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()})
	},
}

func init() {
	containCmd.AddCommand(containPairCmd)
	f := containPairCmd.Flags()
	f.StringVar(&containPairOptions.PeerBroker, "broker", "", "Approved signalling broker URL (HTTPS; HTTP only on loopback)")
	f.StringVar(&containPairOptions.PeerRegistrationFile, "registration-token-file", "", "Private file containing the broker pilot registration capability")
	f.StringVar(&containPairOptions.PeerStateDir, "peer-state-dir", "", "Private persistent peer identity/grant directory (default beside Hub node store)")
	f.DurationVar(&containPairOptions.PeerPairingTTL, "pairing-ttl", 5*time.Minute, "One-use pairing link lifetime (1m-30m; never beyond broker rendezvous)")
	f.StringVar(&containPairOptions.PeerAuditFile, "audit-file", "", "NEW private bounded request audit JSONL (hashes/IDs only; no prompts/SDP/credentials)")
	f.StringVar(&containPairOptions.PeerPairLinkFile, "pair-link-file", "", "Write the secret one-use pairing link to a NEW private file instead of terminal output")
	f.StringVar(&containPairOptions.PeerICEFile, "ice-servers-file", "", "Private finite preissued SELF-HOSTED test ICECredentials JSON (never a TURN master key)")
	f.StringVar(&containPairOptions.Config, "config", "", "Private static Hub node config (YAML/JSON) in addition to contain discovery")
	f.StringVar(&containPairOptions.PeerName, "name", "", "Display name of this Hub (not a credential)")
	f.BoolVar(&containPairOptions.PeerTURN, "turn", false, "Request short-lived TURN credentials from the broker (fails if unavailable)")
	f.BoolVar(&containPairOptions.PeerRelayOnly, "relay-only", false, "Diagnostic: require relay ICE candidates (requires --turn or --ice-servers-file; stats still required)")
	f.StringSliceVar(&containPairOptions.PeerSTUN, "stun", []string{"stun:stun.cloudflare.com:3478"}, "STUN URL for direct ICE discovery (repeatable)")
	f.IntVar(&containPairOptions.Port, "port", 8090, "Local loopback Hub HTTP port")
	f.StringVar(&containPairOptions.NodesFile, "nodes-file", "", "Hub nodes JSON store")
	_ = containPairCmd.MarkFlagRequired("broker")
	_ = containPairCmd.MarkFlagRequired("registration-token-file")
}

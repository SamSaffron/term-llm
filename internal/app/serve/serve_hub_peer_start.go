package serve

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/pion/webrtc/v4"
	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
	"github.com/samsaffron/term-llm/internal/pairing"
	"github.com/samsaffron/term-llm/internal/peer"
	"github.com/samsaffron/term-llm/internal/peerbroker"
	"github.com/samsaffron/term-llm/internal/peerconnect"
)

func (s *hubServer) newPeerHost(ctx context.Context, opts *apphub.Options, deps hubDeps, nodesFile string) (*peerconnect.Host, error) {
	if opts.PeerRelayOnly && !opts.PeerTURN && opts.PeerICEFile == "" {
		return nil, fmt.Errorf("--relay-only requires --turn or a private --ice-servers-file with finite preissued credentials")
	}
	token, err := peerconnect.ReadCapabilityFile(opts.PeerRegistrationFile)
	if err != nil {
		return nil, err
	}
	if deps.Stdin == nil || !hubOutputIsTerminal(deps.Stdin) || !hubOutputIsTerminal(deps.Stdout) {
		return nil, fmt.Errorf("contain pair requires an interactive terminal for secret link display and explicit device approval")
	}
	stateDir := opts.PeerStateDir
	if stateDir == "" {
		stateDir = filepath.Join(filepath.Dir(nodesFile), "peer")
	}
	var ice []webrtc.ICEServer
	if len(opts.PeerSTUN) > 0 {
		ice = append(ice, webrtc.ICEServer{URLs: opts.PeerSTUN})
	}
	if opts.PeerICEFile != "" {
		if opts.PeerTURN {
			return nil, fmt.Errorf("choose broker TURN minting or preissued SELF-HOSTED credentials, not both")
		}
		minter, err := peerbroker.NewPreissuedTURNFile(opts.PeerICEFile, time.Now())
		if err != nil {
			return nil, err
		}
		creds, err := minter.MintICEServers(ctx, peerbroker.MinTURNTTL)
		if err != nil {
			return nil, err
		}
		for _, server := range creds.ICEServers {
			ice = append(ice, webrtc.ICEServer{URLs: server.URLs, Username: server.Username, Credential: server.Credential})
		}
		fmt.Fprintln(deps.Stderr, "Peer TURN credential source: preissued SELF-HOSTED demo; NOT Cloudflare.")
	}
	// The transport's per-request gate consults the persisted device grant.
	// This private ingress additionally limits operations to the pilot scope.
	var audit *peerconnect.AuditFile
	if opts.PeerAuditFile != "" {
		audit, err = peerconnect.NewAuditFile(opts.PeerAuditFile)
		if err != nil {
			return nil, err
		}
	}
	var peerHost *peerconnect.Host
	handler := s.peerHandler(func(r *http.Request) bool {
		grant, ok := peerconnect.AuthenticatedGrant(r.Context())
		if !ok || peerHost == nil {
			return false
		}
		_, err := peerHost.Grants().Authorize(grant.ID, ed25519.PublicKey(grant.DeviceKey), time.Now())
		return err == nil
	}, func(string) bool { return true })
	peerHost, err = peerconnect.NewHost(ctx, peerconnect.Config{
		PairingTTL: opts.PeerPairingTTL, Audit: audit, BrokerURL: opts.PeerBroker, RegistrationToken: token, StateDir: stateDir, Name: opts.PeerName,
		Handler: handler, Endpoint: peer.EndpointConfig{ICEServers: ice, ICETransportPolicy: peerICEPolicy(opts.PeerRelayOnly)}, TURN: opts.PeerTURN,
		Approver: pairing.TerminalApprover{In: deps.Stdin, Out: deps.Stdout},
		OnPairingLink: func(link pairing.Link) error {
			if _, err := fmt.Fprintf(deps.Stdout, "\nPair this Hub with the native app (one use; expires %s).\nScope: Hub browsing and agent chat on ALL discovered nodes; no Hub security or node-registration management.\n", link.ExpiresAt.Format("15:04:05")); err != nil {
				return err
			}
			if opts.PeerPairLinkFile != "" {
				if err := writePeerPairingLinkFile(opts.PeerPairLinkFile, link); err != nil {
					return err
				}
				fmt.Fprintln(deps.Stdout, "Pairing link written to the requested private file; secret not printed.")
			} else {
				fmt.Fprintln(deps.Stdout, link.String())
			}
			_, err := fmt.Fprintln(deps.Stdout, "Keep the fragment private; this is not the Hub master token. Reconnect is limited to this live broker rendezvous (at most 30 minutes); host restart or mailbox expiry requires new pairing.")
			return err
		},
		OnStatus: func(status string) { fmt.Fprintln(deps.Stderr, status) },
	})
	if err != nil && audit != nil {
		_ = audit.Close()
	}
	return peerHost, err
}

func peerICEPolicy(relayOnly bool) webrtc.ICETransportPolicy {
	if relayOnly {
		return webrtc.ICETransportPolicyRelay
	}
	return webrtc.ICETransportPolicyAll
}

func writePeerPairingLinkFile(path string, link pairing.Link) error {
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("pairing link file requires an existing private (0700), non-symlink parent directory")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(link.String() + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

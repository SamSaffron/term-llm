package peer

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/pion/turn/v5"
	"github.com/pion/webrtc/v4"
)

// countingPacketConn counts bytes crossing the TURN server's client-facing
// socket, proving traffic actually traversed the relay.
type countingPacketConn struct {
	net.PacketConn
	in, out atomic.Uint64
}

func (c *countingPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	c.in.Add(uint64(n))
	return n, addr, err
}

func (c *countingPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(p, addr)
	c.out.Add(uint64(n))
	return n, err
}

// startLocalTestTURN runs an in-process Pion TURN server for this test only.
// It is a LOCAL TEST TURN, not Cloudflare or any deployed relay: it binds an
// ephemeral loopback UDP port, relays on loopback, uses random throwaway
// credentials, and is closed when the test ends.
func startLocalTestTURN(t *testing.T) (webrtc.ICEServer, *countingPacketConn) {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen local test TURN: %v", err)
	}
	counted := &countingPacketConn{PacketConn: pc}
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	const realm, user = "local-test-turn.invalid", "local-test"
	password := hex.EncodeToString(secret)
	key := turn.GenerateAuthKey(user, realm, password)
	srv, err := turn.NewServer(turn.ServerConfig{
		Realm: realm,
		AuthHandler: func(ra *turn.RequestAttributes) (string, []byte, bool) {
			if ra.Username != user {
				return "", nil, false
			}
			return user, key, true
		},
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn: counted,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
				RelayAddress: net.ParseIP("127.0.0.1"),
				Address:      "127.0.0.1",
			},
		}},
	})
	if err != nil {
		t.Fatalf("start local test TURN: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return webrtc.ICEServer{
		URLs:       []string{fmt.Sprintf("turn:%s?transport=udp", pc.LocalAddr())},
		Username:   user,
		Credential: password,
	}, counted
}

func TestBridgeRelayOnlyThroughLocalTestTURN(t *testing.T) {
	iceServer, turnConn := startLocalTestTURN(t)
	payload := bytes.Repeat([]byte("relay-bytes-"), 64<<10) // 768 KiB
	p := newPair(t, pairConfig{
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) }),
		endpoint: &EndpointConfig{
			ICEServers:         []webrtc.ICEServer{iceServer},
			ICETransportPolicy: webrtc.ICETransportPolicyRelay,
			SettingEngine:      loopbackSettings(),
		},
	})

	before := selectedRelayPair(t, p.cliEP)
	turnBefore := turnConn.in.Load() + turnConn.out.Load()

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/relay", nil)
	resp, err := p.httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("relayed body: %d bytes, %v", len(got), err)
	}

	cli := selectedRelayPair(t, p.cliEP)
	srv := selectedRelayPair(t, p.srvEP)
	n := uint64(len(payload))
	if delta := cli.BytesReceived - before.BytesReceived; delta < n {
		t.Fatalf("client pair received %d bytes during transfer, want >= %d", delta, n)
	}
	if srv.BytesSent < n {
		t.Fatalf("server pair sent %d bytes, want >= %d", srv.BytesSent, n)
	}
	// Each relayed byte enters the TURN socket from the server peer and
	// leaves it towards the client peer.
	if delta := turnConn.in.Load() + turnConn.out.Load() - turnBefore; delta < 2*n {
		t.Fatalf("local test TURN carried %d bytes, want >= %d", delta, 2*n)
	}
	t.Logf("client pair %s:%d(%s) -> %s:%d(%s), server sent %d", cli.LocalAddress, cli.LocalPort,
		cli.LocalCandidateType, cli.RemoteAddress, cli.RemotePort, cli.RemoteCandidateType, srv.BytesSent)
}

func selectedRelayPair(t *testing.T, ep *Endpoint) SelectedPairStats {
	t.Helper()
	s, ok, err := ep.SelectedPair()
	if err != nil || !ok {
		t.Fatalf("selected pair: ok=%v err=%v", ok, err)
	}
	relay := webrtc.ICECandidateTypeRelay.String()
	if s.LocalCandidateType != relay || s.RemoteCandidateType != relay || !s.Relayed() {
		t.Fatalf("selected pair is not relay/relay: %+v", s)
	}
	return s
}

func TestSelectedPairReportsHostCandidatesWithoutTURN(t *testing.T) {
	p := newPair(t, pairConfig{handler: http.NotFoundHandler()})
	s, ok, err := p.cliEP.SelectedPair()
	if err != nil || !ok {
		t.Fatalf("selected pair: ok=%v err=%v", ok, err)
	}
	if s.Relayed() || s.LocalCandidateType != webrtc.ICECandidateTypeHost.String() || s.BytesSent == 0 {
		t.Fatalf("unexpected direct pair %+v", s)
	}
	ep, err := NewEndpoint(EndpointConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()
	if _, ok, err := ep.SelectedPair(); ok || err != nil {
		t.Fatalf("unconnected endpoint reported a pair: ok=%v err=%v", ok, err)
	}
}

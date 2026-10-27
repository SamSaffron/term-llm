package linkeddevices_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/turn/v5"
	"github.com/pion/webrtc/v4"
	"github.com/samsaffron/term-llm/internal/linkeddevices"
	"github.com/samsaffron/term-llm/internal/linkeddevices/testdevice"
	"github.com/samsaffron/term-llm/internal/peer"
	"github.com/samsaffron/term-llm/internal/peerbroker"
)

type countedTURNPacketConn struct {
	net.PacketConn
	in  atomic.Uint64
	out atomic.Uint64
}

func (c *countedTURNPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	c.in.Add(uint64(n))
	return n, addr, err
}

func (c *countedTURNPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(p, addr)
	c.out.Add(uint64(n))
	return n, err
}

func startLinkedDevicesTestTURN(t *testing.T) (peerbroker.ConfiguredICEServer, *countedTURNPacketConn) {
	t.Helper()
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	counted := &countedTURNPacketConn{PacketConn: packetConn}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	const realm, username = "linked-devices-test.invalid", "linked-device"
	password := hex.EncodeToString(secret)
	authKey := turn.GenerateAuthKey(username, realm, password)
	server, err := turn.NewServer(turn.ServerConfig{
		Realm: realm,
		AuthHandler: func(attributes *turn.RequestAttributes) (string, []byte, bool) {
			if attributes.Username != username {
				return "", nil, false
			}
			return username, authKey, true
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
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return peerbroker.ConfiguredICEServer{
		URLs:             []string{fmt.Sprintf("turn:%s?transport=udp", packetConn.LocalAddr())},
		CredentialSource: peerbroker.CredentialSourceStatic,
		Username:         username,
		Credential:       password,
	}, counted
}

func linkedDevicesRelayEndpoint() peer.EndpointConfig {
	settings := &webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settings.SetInterfaceFilter(func(name string) bool { return name == "lo" || name == "lo0" })
	return peer.EndpointConfig{ICETransportPolicy: webrtc.ICETransportPolicyRelay, SettingEngine: settings}
}

func TestConfiguredStaticTURNRelaysBothPeersAndActualBytes(t *testing.T) {
	turnServer, turnConn := startLinkedDevicesTestTURN(t)
	policy := peerbroker.NewHostICEConfig([]peerbroker.ConfiguredICEServer{turnServer})
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	broker := httptest.NewServer(brokerHandler)
	defer broker.Close()
	defer brokerHandler.Close()

	payload := bytes.Repeat([]byte("linked-device-relay-bytes-"), 18<<10)
	runtime, err := linkeddevices.New(linkeddevices.Options{
		DataDir: t.TempDir(), Target: "relay-policy", StoreInstanceID: "relay-policy-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL, ICEPolicy: &policy, Endpoint: linkedDevicesRelayEndpoint(),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/v1/sessions" {
				http.NotFound(w, request)
				return
			}
			_, _ = w.Write(payload)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sas := make(chan string, 1)
	device, err := testdevice.New(testdevice.Config{
		DeviceName: "Relay device", StatePath: t.TempDir() + "/device.json", Endpoint: linkedDevicesRelayEndpoint(),
		OnSAS: func(code string) { sas <- code },
	})
	if err != nil {
		t.Fatal(err)
	}
	pairRuntimeDevice(t, ctx, runtime, device, sas)
	session, err := device.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	waitRuntimeStatus(t, ctx, runtime, func(status linkeddevices.Status) bool {
		return len(status.Devices) == 1 && status.Devices[0].Connection == "relay"
	})
	selected, ok, err := session.SelectedPair()
	if err != nil || !ok || !selected.Relayed() || selected.LocalCandidateType != "relay" || selected.RemoteCandidateType != "relay" {
		t.Fatalf("device selected pair: ok=%v err=%v stats=%+v", ok, err, selected)
	}

	before := turnConn.in.Load() + turnConn.out.Load()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://peer/v1/sessions", nil)
	response, err := session.HTTPClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || !bytes.Equal(body, payload) {
		t.Fatalf("relayed response bytes = %d, err=%v", len(body), readErr)
	}
	if carried := turnConn.in.Load() + turnConn.out.Load() - before; carried < uint64(len(payload)) {
		t.Fatalf("local TURN carried %d bytes, want at least %d", carried, len(payload))
	}
}

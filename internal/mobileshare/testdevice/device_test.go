package testdevice

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/samsaffron/term-llm/internal/pairing"
	"github.com/samsaffron/term-llm/internal/peer"
	"github.com/samsaffron/term-llm/internal/peerbroker"
)

func TestPairConnectPersistAndReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	brokerServer := httptest.NewServer(brokerHandler)
	t.Cleanup(func() {
		brokerHandler.Close()
		brokerServer.Close()
	})
	brokerClient, err := peerbroker.NewClient(brokerServer.URL, brokerServer.Client())
	if err != nil {
		t.Fatal(err)
	}

	hostIdentity, err := pairing.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	grants, err := pairing.OpenGrantStore(filepath.Join(t.TempDir(), "host", "grants.json"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := pairing.NewHost(hostIdentity, grants, pairing.HostOptions{Broker: brokerServer.URL, HostLabel: "Test host", BasePath: "/api/"})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := brokerClient.RegisterHostWithSigner(ctx, hostIdentity.PublicKey(), "", nil, func(payload []byte) ([]byte, error) {
		return hostIdentity.Sign(payload), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	invitationID := make([]byte, pairing.InvitationIDSize)
	if _, err := rand.Read(invitationID); err != nil {
		t.Fatal(err)
	}
	invitationCapability, err := peerbroker.GenerateCapability()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Minute).Truncate(time.Second)
	invitationName := pairing.B64(invitationID).String()
	if _, err := brokerClient.RegisterInvitation(ctx, registration.HostID, registration.LeaseCapability, invitationName, peerbroker.HashCapability(invitationCapability), expires); err != nil {
		t.Fatal(err)
	}
	link, err := host.NewPairingForInvitation(invitationName, invitationCapability, expires)
	if err != nil {
		t.Fatal(err)
	}

	hostConn, err := brokerClient.DialHost(ctx, registration.HostID, registration.LeaseCapability)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hostConn.Close() })
	settings := loopbackSettings()
	hostErrors := make(chan error, 1)
	hostSAS := make(chan string, 1)
	go runFakeHost(ctx, hostConn, brokerClient, registration, host, settings, hostSAS, hostErrors)

	statePath := filepath.Join(t.TempDir(), "device", "state.json")
	var deviceSAS string
	device, err := New(Config{
		DeviceName: "Synthetic phone", StatePath: statePath, BrokerHTTP: brokerServer.Client(),
		Endpoint: peer.EndpointConfig{SettingEngine: settings}, OnSAS: func(s string) { deviceSAS = s },
	})
	if err != nil {
		t.Fatal(err)
	}
	sas, err := device.Pair(ctx, link.String())
	if err != nil {
		t.Fatal(err)
	}
	if sas == "" || sas != deviceSAS {
		t.Fatalf("SAS return/callback = %q/%q", sas, deviceSAS)
	}
	select {
	case got := <-hostSAS:
		if got != sas {
			t.Fatalf("host/device SAS = %q/%q", got, sas)
		}
	default:
		t.Fatal("fake host did not expose SAS")
	}
	if info, err := os.Stat(statePath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state permissions: info=%v err=%v", info, err)
	}
	assertBridgeRequest(t, ctx, device)

	reloaded, err := New(Config{StatePath: statePath, BrokerHTTP: brokerServer.Client(), Endpoint: peer.EndpointConfig{SettingEngine: loopbackSettings()}})
	if err != nil {
		t.Fatal(err)
	}
	if paired, ok := reloaded.Pairing(); !ok || paired.DeviceID == "" || paired.BasePath != "/api/" {
		t.Fatalf("reloaded pairing = %+v, %v", paired, ok)
	}
	assertBridgeRequest(t, ctx, reloaded)

	select {
	case err := <-hostErrors:
		t.Fatal(err)
	default:
	}
}

func assertBridgeRequest(t *testing.T, ctx context.Context, device *Device) {
	t.Helper()
	session, err := device.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://synthetic.invalid/api/ping?from=device", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := session.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "peer:/api/ping?from=device" {
		t.Fatalf("bridge response = %d %q", resp.StatusCode, body)
	}
}

func runFakeHost(ctx context.Context, conn *peerbroker.Conn, broker *peerbroker.Client, registration *peerbroker.Registration, host *pairing.Host, settings *webrtc.SettingEngine, sas chan<- string, failures chan<- error) {
	server, err := peer.NewServer(peer.ServerConfig{
		Handler:     http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "peer:"+r.URL.RequestURI()) }),
		SessionGate: func(context.Context) error { return nil },
		Authorize: func(r *http.Request) error {
			if !strings.HasPrefix(r.URL.Path, "/api/") {
				return pairing.ErrNotApproved
			}
			return nil
		},
	})
	if err != nil {
		reportHostFailure(ctx, failures, err)
		return
	}
	var endpoints []*peer.Endpoint
	defer func() {
		for _, endpoint := range endpoints {
			_ = endpoint.Close()
		}
	}()
	for {
		frame, err := conn.Recv()
		if err != nil {
			reportHostFailure(ctx, failures, err)
			return
		}
		if frame.Type != peerbroker.FrameSignal {
			continue
		}
		message, err := pairing.DecodeMessage(frame.Body)
		if err != nil {
			reportHostFailure(ctx, failures, err)
			return
		}
		var response any
		switch message := message.(type) {
		case *pairing.PairRequest:
			pending, err := host.VerifyPairRequest(message)
			if err != nil {
				reportHostFailure(ctx, failures, err)
				return
			}
			sas <- pending.SAS
			if err := host.Approve(ctx, pending, pairing.ApproverFunc(func(context.Context, pairing.PairingPrompt) (bool, error) { return true, nil })); err != nil {
				reportHostFailure(ctx, failures, err)
				return
			}
			answer, grant, err := host.CompletePairing(pending)
			if err != nil {
				reportHostFailure(ctx, failures, err)
				return
			}
			capability := answer.ReconnectCapability.String()
			devices := []peerbroker.DeviceCapabilityHash{{DeviceID: grant.DeviceID, Hash: peerbroker.HashCapability(capability)}}
			if _, err := broker.Renew(ctx, registration.HostID, registration.LeaseCapability, devices); err != nil {
				reportHostFailure(ctx, failures, err)
				return
			}
			response = answer
		case *pairing.TransportHello:
			response, err = host.NewTransportChallenge(message)
			if err != nil {
				reportHostFailure(ctx, failures, err)
				return
			}
		case *pairing.TransportRequest:
			verified, err := host.VerifyTransport(message)
			if err != nil {
				reportHostFailure(ctx, failures, err)
				return
			}
			endpoint, err := peer.NewEndpoint(peer.EndpointConfig{SettingEngine: settings})
			if err != nil {
				reportHostFailure(ctx, failures, err)
				return
			}
			endpoints = append(endpoints, endpoint)
			endpoint.Serve(ctx, server)
			answer, err := endpoint.AcceptOffer(ctx, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: verified.OfferSDP})
			if err != nil {
				reportHostFailure(ctx, failures, err)
				return
			}
			response, err = host.AnswerTransport(verified, answer.SDP)
			if err != nil {
				reportHostFailure(ctx, failures, err)
				return
			}
		default:
			continue
		}
		body, err := json.Marshal(response)
		if err == nil {
			err = conn.Send(frame.PeerKind, frame.PeerID, body)
		}
		if err != nil {
			reportHostFailure(ctx, failures, err)
			return
		}
	}
}

func reportHostFailure(ctx context.Context, failures chan<- error, err error) {
	if ctx.Err() != nil {
		return
	}
	select {
	case failures <- err:
	default:
	}
}

func loopbackSettings() *webrtc.SettingEngine {
	settings := &webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settings.SetInterfaceFilter(func(name string) bool { return name == "lo" })
	return settings
}

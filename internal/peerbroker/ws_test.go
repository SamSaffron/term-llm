package peerbroker

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func recvType(t *testing.T, conn *Conn, want string) *Frame {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		frame, err := conn.Recv()
		if err != nil {
			t.Fatalf("receive %s: %v", want, err)
		}
		if frame.Type == want {
			_ = conn.SetReadDeadline(time.Time{})
			return frame
		}
	}
}

func TestDialCancellationClosesSocketWhileWaitingForReady(t *testing.T) {
	upgraded := make(chan struct{})
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		close(upgraded)
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		close(closed)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	key := mustKey(t)
	hostID := hostIDForKey(key.Public().(ed25519.PublicKey))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, dialErr := client.DialDevice(ctx, hostID, "device", strings.Repeat("d", MinCredentialLen))
		result <- dialErr
	}()
	select {
	case <-upgraded:
	case <-time.After(time.Second):
		t.Fatal("websocket was not upgraded")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dial cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dial remained blocked waiting for ready after cancellation")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("canceled registration socket remained open")
	}
}

func TestRoutedDeviceSignallingAndReconnect(t *testing.T) {
	_, client, _ := newBrokerClient(t, Config{})
	deviceCap := "host-generated-device-reconnect"
	devices := []DeviceCapabilityHash{{DeviceID: "phone_1", Hash: HashCapability(deviceCap)}}
	registration := mustRegister(t, client, mustKey(t), "", devices)
	host, err := client.DialHost(context.Background(), registration.HostID, registration.LeaseCapability)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	device, err := client.DialDevice(context.Background(), registration.HostID, "phone_1", deviceCap)
	if err != nil {
		t.Fatal(err)
	}
	if device.Ready.PeerKind != PeerDevice || device.Ready.PeerID != "phone_1" || !*device.Ready.PeerConnected {
		t.Fatalf("device ready = %+v", device.Ready)
	}

	fromDevice := json.RawMessage(`{"sdp":"offer"}`)
	if err := device.Send("", "", fromDevice); err != nil {
		t.Fatal(err)
	}
	got := recvType(t, host, FrameSignal)
	if got.PeerKind != PeerDevice || got.PeerID != "phone_1" || string(got.Body) != string(fromDevice) {
		t.Fatalf("host signal = %+v", got)
	}
	fromHost := json.RawMessage(`{"candidate":"one"}`)
	if err := host.Send(PeerDevice, "phone_1", fromHost); err != nil {
		t.Fatal(err)
	}
	got = recvType(t, device, FrameSignal)
	if string(got.Body) != string(fromHost) {
		t.Fatalf("device signal = %+v", got)
	}

	_ = device.Close()
	reconnected, err := client.ReconnectDevice(context.Background(), registration.HostID, "phone_1", deviceCap)
	if err != nil {
		t.Fatal(err)
	}
	_ = reconnected.Close()
	if _, err := client.Renew(context.Background(), registration.HostID, registration.LeaseCapability, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DialDevice(context.Background(), registration.HostID, "phone_1", deviceCap); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked device error = %v", err)
	}
}

func TestInvitationRoutingWrongCapabilityReplayAndNoSecretFrame(t *testing.T) {
	_, client, _ := newBrokerClient(t, Config{})
	registration := mustRegister(t, client, mustKey(t), "", nil)
	invitationCap := "host-owned-invitation-capability"
	_, err := client.RegisterInvitation(context.Background(), registration.HostID, registration.LeaseCapability, "pair_1", HashCapability(invitationCap), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.DialInvitation(context.Background(), registration.HostID, "pair_1", "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong capability error = %v", err)
	}
	host, err := client.DialHost(context.Background(), registration.HostID, registration.LeaseCapability)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	guest, err := client.DialInvitation(context.Background(), registration.HostID, "pair_1", invitationCap)
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(guest.Ready); string(encoded) == "" || containsBytes(encoded, []byte(invitationCap)) {
		t.Fatalf("ready leaked capability: %s", encoded)
	}
	if err := guest.Send(PeerDevice, "other", json.RawMessage(`{"offer":1}`)); err != nil {
		t.Fatal(err)
	}
	frame := recvType(t, guest, FrameError)
	if frame.Error != CodeUnauthorized {
		t.Fatalf("spoof route error = %+v", frame)
	}
	if err := guest.Send("", "", json.RawMessage(`{"offer":2}`)); err != nil {
		t.Fatal(err)
	}
	frame = recvType(t, host, FrameSignal)
	if frame.PeerKind != PeerInvitation || frame.PeerID != "pair_1" {
		t.Fatalf("invitation route = %+v", frame)
	}
	_ = guest.Close()
	redeemer, err := client.DialInvitation(context.Background(), registration.HostID, "pair_1", invitationCap)
	if err != nil {
		t.Fatalf("authenticated duplicate redemption must reach the host: %v", err)
	}
	_ = redeemer.Close()
}

func TestOfflineAndSignalLimits(t *testing.T) {
	_, client, _ := newBrokerClient(t, Config{MaxSignalBytes: 64, MaxSignalsPerConnection: 2, SignalBurst: 10})
	registration := mustRegister(t, client, mustKey(t), "", []DeviceCapabilityHash{{DeviceID: "offline", Hash: HashCapability("offline-secret")}})
	host, err := client.DialHost(context.Background(), registration.HostID, registration.LeaseCapability)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if err := host.Send(PeerDevice, "offline", json.RawMessage(`{"candidate":1}`)); err != nil {
		t.Fatal(err)
	}
	if frame := recvType(t, host, FrameError); frame.Error != CodePeerOffline {
		t.Fatalf("offline error = %+v", frame)
	}
	large := make([]byte, 80)
	for i := range large {
		large[i] = 'x'
	}
	body, _ := json.Marshal(string(large))
	if err := host.Send(PeerDevice, "offline", body); err != nil {
		t.Fatal(err)
	}
	_, err = host.Recv()
	var closeErr *CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != 1009 {
		t.Fatalf("oversize close = %v", err)
	}
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}

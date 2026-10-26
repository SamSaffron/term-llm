package linkeddevices

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/pairing"
	"github.com/samsaffron/term-llm/internal/peer"
	"github.com/samsaffron/term-llm/internal/peerbroker"
)

func TestRuntimeOffAndUnavailableMakeNoOutboundCalls(t *testing.T) {
	var calls atomic.Int64
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer broker.Close()
	for _, authenticated := range []bool{true, false} {
		runtime, err := New(Options{DataDir: t.TempDir(), Target: "agent", StoreInstanceID: "store", Authenticated: authenticated, BrokerURL: broker.URL, Handler: http.NotFoundHandler()})
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.Start(); err != nil {
			t.Fatal(err)
		}
		if err := runtime.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("off/unavailable runtime made %d outbound calls", got)
	}
}

func TestTransportReservationsBoundConcurrencyAndGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := &Runtime{
		ctx: ctx, cancel: cancel, settings: settingsState{Enabled: true}, sessions: map[string]*deviceSession{},
		pending: map[string]*pendingRequest{},
	}
	runtime.startSharingLocked()
	for _, id := range []string{"one", "two", "three"} {
		runtime.sessions[id] = &deviceSession{endpoint: &peer.Endpoint{}}
	}
	reservedCtx, generation, newDevice, ok := runtime.reserveTransport("four")
	if !ok || !newDevice {
		t.Fatal("fourth distinct device did not reserve the last connection slot")
	}
	if _, _, _, ok := runtime.reserveTransport("five"); ok {
		t.Fatal("fifth distinct device passed concurrent capacity reservation")
	}
	if _, _, replacement, ok := runtime.reserveTransport("one"); !ok || replacement {
		t.Fatal("bounded replacement handshake was refused or counted as a new device")
	}
	runtime.releaseTransport(false)
	runtime.mu.Lock()
	runtime.stopSharingLocked()
	runtime.mu.Unlock()
	select {
	case <-reservedCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stopping did not cancel reserved handshakes")
	}
	if runtime.beginPeerRequest(generation) {
		t.Fatal("stale generation admitted a peer request")
	}
	runtime.releaseTransport(true)
}

func TestSessionCleanupUsesEndpointIdentity(t *testing.T) {
	oldEndpoint, replacement := &peer.Endpoint{}, &peer.Endpoint{}
	runtime := &Runtime{sessions: map[string]*deviceSession{"device": {endpoint: replacement}}}
	runtime.removeSession("device", oldEndpoint)
	if runtime.sessions["device"] == nil {
		t.Fatal("old disconnect deleted its replacement")
	}
	runtime.removeSession("device", replacement)
	if runtime.sessions["device"] != nil {
		t.Fatal("terminal endpoint remained counted as connected")
	}
}

func TestRenewRetriesSingleTransientFailure(t *testing.T) {
	var renewCalls atomic.Int64
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/renew") {
			http.NotFound(w, request)
			return
		}
		if renewCalls.Add(1) == 1 {
			http.Error(w, "transient", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(peerbroker.Lease{LeaseExpiresAt: time.Now().Add(time.Minute)})
	}))
	defer broker.Close()
	runtime, err := New(Options{
		DataDir: t.TempDir(), Target: "renew-retry", StoreInstanceID: "renew-retry-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	runtime.mu.Lock()
	runtime.hostID = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	runtime.leaseCapability = "test-lease-capability"
	runtime.brokerConnected = true
	runtime.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.renewWithRetry(ctx); err != nil {
		t.Fatalf("single transient renewal failure was not retried: %v", err)
	}
	if got := renewCalls.Load(); got != 2 {
		t.Fatalf("renew calls = %d, want 2", got)
	}
}

func TestRenewSnapshotsAreSerializedAcrossRevocation(t *testing.T) {
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer brokerHandler.Close()
	firstRenew := make(chan struct{})
	secondRenew := make(chan struct{})
	releaseFirst := make(chan struct{})
	var renewCalls atomic.Int64
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/renew") {
			switch renewCalls.Add(1) {
			case 1:
				close(firstRenew)
				<-releaseFirst
			case 2:
				close(secondRenew)
			}
		}
		brokerHandler.ServeHTTP(w, request)
	}))
	defer broker.Close()
	runtime, err := New(Options{
		DataDir: t.TempDir(), Target: "renew", StoreInstanceID: "renew-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL, Handler: http.NotFoundHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	idBytes := make([]byte, pairing.DeviceIDSize)
	idBytes[0] = 1
	deviceID := base64.RawURLEncoding.EncodeToString(idBytes)
	key := make([]byte, ed25519.PublicKeySize)
	key[0] = 1
	capability := strings.Repeat("c", peerbroker.MinCredentialLen)
	hash := sha256.Sum256([]byte(capability))
	if err := runtime.grants.Add(pairing.Grant{
		DeviceID: deviceID, DeviceKey: key, DeviceName: "phone", Scope: pairing.ScopeWebV1,
		BasePath: "/", ReconnectCapabilityHash: hash[:], PairedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Enable(ctx); err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- runtime.renewNow(ctx) }()
	select {
	case <-firstRenew:
	case <-ctx.Done():
		t.Fatal("first renewal did not reach broker")
	}
	revokeDone := make(chan error, 1)
	go func() { revokeDone <- runtime.Revoke(ctx, deviceID) }()
	select {
	case <-secondRenew:
		t.Fatal("revocation published concurrently with stale renewal")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-revokeDone; err != nil {
		t.Fatal(err)
	}
	if got := renewCalls.Load(); got != 2 {
		t.Fatalf("renew calls = %d, want serialized stale then revocation snapshots", got)
	}
	if _, err := runtime.broker.ICE(ctx, runtime.hostID, peerbroker.PeerDevice, deviceID, capability); !errors.Is(err, peerbroker.ErrUnauthorized) {
		t.Fatalf("revoked capability was republished: %v", err)
	}
}

func TestRevokedHistoryDoesNotConsumeActiveDeviceQuota(t *testing.T) {
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	broker := httptest.NewServer(brokerHandler)
	defer broker.Close()
	defer brokerHandler.Close()
	runtime, err := New(Options{
		DataDir: t.TempDir(), Target: "quota", StoreInstanceID: "quota-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL, Handler: http.NotFoundHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	now := time.Now()
	for i := range maxDevices {
		id := make([]byte, pairing.DeviceIDSize)
		id[len(id)-1] = byte(i + 1)
		key := make([]byte, ed25519.PublicKeySize)
		key[len(key)-1] = byte(i + 1)
		hash := make([]byte, 32)
		hash[len(hash)-1] = byte(i + 1)
		deviceID := base64.RawURLEncoding.EncodeToString(id)
		if err := runtime.grants.Add(pairing.Grant{
			DeviceID: deviceID, DeviceKey: key, DeviceName: "old", Scope: pairing.ScopeWebV1,
			BasePath: "/", ReconnectCapabilityHash: hash, PairedAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
		if err := runtime.grants.Revoke(deviceID, now); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.NewInvitation(ctx, func(string) (string, error) { return "qr", nil }); err != nil {
		t.Fatalf("revoked history blocked replacement enrollment: %v", err)
	}
}

func TestResetRequiredRuntimeRebuildsBrokerDependencies(t *testing.T) {
	firstHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	firstBroker := httptest.NewServer(firstHandler)
	defer firstBroker.Close()
	defer firstHandler.Close()
	secondHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	secondBroker := httptest.NewServer(secondHandler)
	defer secondBroker.Close()
	defer secondHandler.Close()

	for _, test := range []struct {
		name, nextStore, nextBroker string
	}{
		{name: "store identity", nextStore: "store-two", nextBroker: firstBroker.URL},
		{name: "broker", nextStore: "store-one", nextBroker: secondBroker.URL},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataDir := t.TempDir()
			initial, err := New(Options{DataDir: dataDir, Target: "agent", StoreInstanceID: "store-one", BasePath: "/chat", Authenticated: true, BrokerURL: firstBroker.URL, Handler: http.NotFoundHandler()})
			if err != nil {
				t.Fatal(err)
			}
			if err := initial.Close(); err != nil {
				t.Fatal(err)
			}
			runtime, err := New(Options{DataDir: dataDir, Target: "agent", StoreInstanceID: test.nextStore, BasePath: "/chat", Authenticated: true, BrokerURL: test.nextBroker, Handler: http.NotFoundHandler()})
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			if status := runtime.Status(); status.State != StateUnavailable || !status.ResetRequired {
				t.Fatalf("pre-reset status = %+v", status)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := runtime.Reset(ctx); err != nil {
				t.Fatal(err)
			}
			if err := runtime.Enable(ctx); err != nil {
				t.Fatal(err)
			}
			if status := runtime.Status(); !status.BrokerConnected || status.ResetRequired {
				t.Fatalf("post-reset status = %+v", status)
			}
		})
	}
}

func TestNewServerBoundsHostLabelAndDegradesOptionalFailures(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	runtime, err := NewServer("agent", "store", "/chat", true, strings.Repeat("long title ", 20)+"\x00", "approval")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	label := safeHostLabel(strings.Repeat("long title ", 20)+"\x00", "agent")
	if len(label) > maxHostLabelBytes || strings.ContainsRune(label, '\x00') {
		t.Fatalf("host label was not bounded: %q", label)
	}

	unavailable, err := NewServer("agent", "", "/chat", true, "title", "approval")
	if err != nil {
		t.Fatal(err)
	}
	defer unavailable.Close()
	if status := unavailable.Status(); status.State != StateUnavailable {
		t.Fatalf("optional failure status = %+v", status)
	}
}

func TestOwnerAPIAcceptsHubSameOriginMetadata(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://node.internal/v1/linked-devices/stop", nil)
	request.Header.Set("Origin", "https://hub.example")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	if !sameOrigin(request) {
		t.Fatal("Hub same-origin mutation was rejected because the proxy replaced Host")
	}
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	if sameOrigin(request) {
		t.Fatal("cross-site mutation was accepted")
	}
}

func TestOwnerAPIRejectsDeviceAndCrossSiteMutation(t *testing.T) {
	runtime, err := New(Options{DataDir: t.TempDir(), Target: "agent", StoreInstanceID: "store", Authenticated: false})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	request := httptest.NewRequest(http.MethodGet, "/v1/linked-devices", nil)
	request = WithDevicePrincipal(request, DevicePrincipal{DeviceID: "device", Scope: ScopeWebV1})
	recorder := httptest.NewRecorder()
	runtime.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("device owner request = %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/linked-devices/stop", nil)
	request.Header.Set("Origin", "https://attacker.invalid")
	recorder = httptest.NewRecorder()
	runtime.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-site owner mutation = %d", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

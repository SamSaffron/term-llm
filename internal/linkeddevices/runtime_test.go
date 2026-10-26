package linkeddevices

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/linkeddevices/testdevice"
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

func TestServeHostDegradesUnavailableRuntime(t *testing.T) {
	host, err := NewServeHost(true, context.Background(), nil, "agent", "/", true, "title", "manual", &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	status := host.Status()
	if status.State != StateUnavailable || !strings.Contains(status.Reason, "no rendezvous service") {
		t.Fatalf("status = %+v", status)
	}
	var logs atomic.Int64
	host.StartAndLog(func(string, ...any) { logs.Add(1) })
	if logs.Load() != 0 {
		t.Fatal("unavailable optional runtime logged a startup failure")
	}
}

func TestDefaultICEPolicyRejectionMakesRuntimeUnavailableWithoutRetry(t *testing.T) {
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer brokerHandler.Close()
	var calls atomic.Int64
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		brokerHandler.ServeHTTP(w, request)
	}))
	defer broker.Close()

	cfg := &config.Config{LinkedDevices: config.LinkedDevicesConfig{
		BrokerURL:  broker.URL,
		ICEServers: config.DefaultLinkedDevicesICEServers(),
	}}
	serverOpts := ServerOptionsFromConfig(cfg)
	runtime, err := New(Options{
		DataDir: t.TempDir(), Target: "default-policy", StoreInstanceID: "default-policy-store", BasePath: "/",
		Authenticated: true, BrokerURL: serverOpts.BrokerURL, ICEPolicy: serverOpts.ICEPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	err = runtime.Enable(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid ice_policy") {
		t.Fatalf("enable error = %v, want rejected ICE policy", err)
	}
	status := runtime.Status()
	if status.State != StateUnavailable || !strings.Contains(status.Reason, "rendezvous rejected linked devices registration: bad_request: invalid ice_policy") {
		t.Fatalf("status = %+v, want clear unavailable rejection", status)
	}
	if !status.CanResume || !status.CanReset {
		t.Fatalf("rejected registration recovery flags = %+v", status)
	}
	runtime.mu.Lock()
	reconnecting := runtime.reconnecting
	runtime.mu.Unlock()
	if reconnecting {
		t.Fatal("permanent registration rejection started automatic retry")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("broker calls = %d, want one challenge and one rejected registration", got)
	}
}

func TestRejectedPairAnswerDeliveryRollsBackGrantWithoutPresence(t *testing.T) {
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	broker := httptest.NewServer(brokerHandler)
	defer broker.Close()
	defer brokerHandler.Close()
	runtime, err := New(Options{
		DataDir: t.TempDir(), Target: "delivery-error", StoreInstanceID: "delivery-error-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL, Handler: http.NotFoundHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	beforeSend, releaseSend := make(chan struct{}), make(chan struct{})
	presenceDropped := make(chan struct{}, 1)
	runtime.testBeforePairAnswerSend = func() { close(beforeSend); <-releaseSend }
	runtime.testDropInvitationPeer = func(*peerbroker.Frame) bool {
		select {
		case presenceDropped <- struct{}{}:
		default:
		}
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var invitationURL string
	if err := runtime.NewInvitation(ctx, func(raw string) (string, error) { invitationURL = raw; return "qr", nil }); err != nil {
		t.Fatal(err)
	}
	sas := make(chan string, 1)
	device, err := testdevice.New(testdevice.Config{DeviceName: "Original", StatePath: t.TempDir() + "/device.json", OnSAS: func(code string) { sas <- code }})
	if err != nil {
		t.Fatal(err)
	}
	pairResult := make(chan error, 1)
	go func() { _, pairErr := device.Pair(ctx, invitationURL); pairResult <- pairErr }()
	code := <-sas
	var pendingID string
	for pendingID == "" {
		status := runtime.Status()
		if len(status.PendingRequests) == 1 {
			pendingID = status.PendingRequests[0].ID
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("pair request did not become pending")
		case <-time.After(time.Millisecond):
		}
	}
	approveResult := make(chan error, 1)
	go func() { approveResult <- runtime.Approve(ctx, pendingID, code) }()
	select {
	case <-beforeSend:
	case <-ctx.Done():
		t.Fatal("approval did not reach answer delivery gap")
	}
	link, err := pairing.ParseLink(invitationURL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := peerbroker.NewClient(link.Broker, nil)
	if err != nil {
		t.Fatal(err)
	}
	hostHash := sha256.Sum256(link.HostKey)
	hostID := base64.RawURLEncoding.EncodeToString(hostHash[:])
	if _, err := client.DialInvitation(ctx, hostID, link.BrokerInvitationID(), link.InvitationCapability); err == nil {
		t.Fatal("duplicate invitation socket was accepted")
	}
	select {
	case <-presenceDropped:
	case <-ctx.Done():
		t.Fatal("duplicate invitation presence was not delayed")
	}
	close(releaseSend)
	if err := <-approveResult; err == nil || !strings.Contains(err.Error(), peerbroker.CodePeerOffline) {
		t.Fatalf("approval delivery error = %v", err)
	}
	if err := <-pairResult; err == nil {
		t.Fatal("original invitation received rejected pair answer")
	}
	if status := runtime.Status(); len(status.Devices) != 0 || len(status.PendingRequests) != 0 {
		t.Fatalf("rejected answer left grant or pending request: %+v", status)
	}
	grants, err := runtime.grants.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Fatalf("rejected answer persisted grants: %+v", grants)
	}
}

func TestPairAnswerDeliveryWaitEndsWithLifecycleOrBroker(t *testing.T) {
	for _, test := range []struct {
		name string
		end  func(*Runtime) error
	}{
		{name: "stop", end: func(runtime *Runtime) error { return runtime.Stop() }},
		{name: "reset", end: func(runtime *Runtime) error { return runtime.Reset(context.Background()) }},
		{name: "cancel invitation", end: func(runtime *Runtime) error { return runtime.CancelInvitation(context.Background()) }},
		{name: "broker disconnect", end: func(runtime *Runtime) error {
			runtime.mu.Lock()
			conn := runtime.brokerConn
			runtime.mu.Unlock()
			return conn.Close()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, oldGrants, approveResult := startDroppedPairAnswerDelivery(t)
			endResult := make(chan error, 1)
			go func() { endResult <- test.end(runtime) }()
			select {
			case err := <-approveResult:
				if err == nil {
					t.Fatal("approval succeeded without broker delivery acknowledgement")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("approval remained blocked after lifecycle or broker ended")
			}
			select {
			case err := <-endResult:
				if err != nil {
					t.Fatalf("end delivery wait: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("lifecycle operation remained blocked by approval")
			}
			grants, err := oldGrants.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(grants) != 0 {
				t.Fatalf("cancelled delivery persisted captured grant: %+v", grants)
			}
		})
	}
}

func startDroppedPairAnswerDelivery(t *testing.T) (*Runtime, *pairing.GrantStore, <-chan error) {
	t.Helper()
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(brokerHandler.Close)
	broker := httptest.NewServer(brokerHandler)
	t.Cleanup(broker.Close)
	runtime, err := New(Options{
		DataDir: t.TempDir(), Target: t.Name(), StoreInstanceID: t.Name() + "-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL, Handler: http.NotFoundHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	var invitationURL string
	if err := runtime.NewInvitation(ctx, func(raw string) (string, error) { invitationURL = raw; return "qr", nil }); err != nil {
		t.Fatal(err)
	}
	deliveryDropped := make(chan struct{}, 1)
	runtime.testDropPairAnswerDelivery = func(*peerbroker.Frame) bool {
		select {
		case deliveryDropped <- struct{}{}:
		default:
		}
		return true
	}
	sas := make(chan string, 1)
	device, err := testdevice.New(testdevice.Config{DeviceName: "Device", StatePath: t.TempDir() + "/device.json", OnSAS: func(code string) { sas <- code }})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = device.Pair(ctx, invitationURL) }()
	code := <-sas
	var pendingID string
	for pendingID == "" {
		status := runtime.Status()
		if len(status.PendingRequests) == 1 {
			pendingID = status.PendingRequests[0].ID
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("pair request did not become pending")
		case <-time.After(time.Millisecond):
		}
	}
	oldGrants := runtime.grants
	approveResult := make(chan error, 1)
	go func() { approveResult <- runtime.Approve(context.Background(), pendingID, code) }()
	select {
	case <-deliveryDropped:
	case <-ctx.Done():
		t.Fatal("pair answer did not reach broker delivery acknowledgement")
	}
	return runtime, oldGrants, approveResult
}

func TestOwnerCanStopAndResumeAfterBrokerPolicyFix(t *testing.T) {
	first, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	var active http.Handler = first
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { active.ServeHTTP(w, request) }))
	defer broker.Close()
	cfg := &config.Config{LinkedDevices: config.LinkedDevicesConfig{BrokerURL: broker.URL, ICEServers: config.DefaultLinkedDevicesICEServers()}}
	serverOpts := ServerOptionsFromConfig(cfg)
	runtime, err := New(Options{
		DataDir: t.TempDir(), Target: "policy-recovery", StoreInstanceID: "policy-recovery-store", BasePath: "/",
		Authenticated: true, BrokerURL: serverOpts.BrokerURL, ICEPolicy: serverOpts.ICEPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Enable(context.Background()); err == nil {
		t.Fatal("broker without configured TURN provider accepted default policy")
	}
	owner := httptest.NewServer(runtime.Handler())
	defer owner.Close()
	response, err := http.Post(owner.URL+"/v1/linked-devices/stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("stop after policy rejection = %d", response.StatusCode)
	}
	second, err := peerbroker.NewHandler(peerbroker.Config{Coturn: &peerbroker.CoturnConfig{
		URLs: []string{"turns:stun.term-llm.com:5349?transport=tcp"}, SharedSecret: strings.Repeat("s", peerbroker.MinCredentialLen),
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	active = second
	response, err = http.Post(owner.URL+"/v1/linked-devices/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("resume after broker fix = %d", response.StatusCode)
	}
	if status := runtime.Status(); !status.BrokerConnected || status.State == StateUnavailable {
		t.Fatalf("status after broker fix = %+v", status)
	}
}

func TestPermanentBrokerRejectionClassification(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		permanent bool
	}{
		{name: "policy", err: &peerbroker.APIError{StatusCode: http.StatusBadRequest, Code: peerbroker.CodeBadRequest, Message: "invalid ice_policy"}, permanent: true},
		{name: "admission", err: &peerbroker.APIError{StatusCode: http.StatusUnauthorized, Code: peerbroker.CodeUnauthorized}, permanent: true},
		{name: "expired lease", err: &peerbroker.APIError{StatusCode: http.StatusGone, Code: peerbroker.CodeExpired}, permanent: false},
		{name: "rate limited", err: &peerbroker.APIError{StatusCode: http.StatusTooManyRequests, Code: peerbroker.CodeRateLimited}, permanent: false},
		{name: "server failure", err: &peerbroker.APIError{StatusCode: http.StatusServiceUnavailable, Code: peerbroker.CodeInternal}, permanent: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reason, permanent := permanentBrokerRejection(test.err)
			if permanent != test.permanent {
				t.Fatalf("permanent = %v, want %v (reason %q)", permanent, test.permanent, reason)
			}
			if permanent && !strings.Contains(reason, "rendezvous rejected linked devices registration") {
				t.Fatalf("reason = %q", reason)
			}
		})
	}
}

func TestRuntimeRejectsInvalidAdmissionEnvironment(t *testing.T) {
	t.Setenv(AdmissionEnv, "short")
	_, err := New(Options{DataDir: t.TempDir(), Target: "env-admission", StoreInstanceID: "env-admission-store", Authenticated: true, BrokerURL: "http://127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "admission credential") {
		t.Fatalf("invalid admission environment error = %v", err)
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
	runtime, err := NewServer("agent", "store", "/chat", true, strings.Repeat("long title ", 20)+"\x00", "approval", ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	label := safeHostLabel(strings.Repeat("long title ", 20)+"\x00", "agent")
	if len(label) > maxHostLabelBytes || strings.ContainsRune(label, '\x00') {
		t.Fatalf("host label was not bounded: %q", label)
	}

	unavailable, err := NewServer("agent", "", "/chat", true, "title", "approval", ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer unavailable.Close()
	if status := unavailable.Status(); status.State != StateUnavailable || status.CanResume || status.CanReset {
		t.Fatalf("optional failure status = %+v", status)
	}
}

func TestServerOptionsRedactCredentials(t *testing.T) {
	secret := "private-static-turn-password"
	policy := peerbroker.NewHostICEConfig([]peerbroker.ConfiguredICEServer{{
		URLs: []string{"turn:relay.example.com:3478"}, CredentialSource: peerbroker.CredentialSourceStatic,
		Username: "device", Credential: secret,
	}})
	options := ServerOptions{BrokerURL: "https://broker.example.com", AdmissionCredential: secret, ICEPolicy: &policy}
	if text := fmt.Sprintf("%v %#v", options, options); strings.Contains(text, secret) {
		t.Fatalf("server options leaked credentials: %s", text)
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

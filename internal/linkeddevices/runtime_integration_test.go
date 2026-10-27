package linkeddevices_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/samsaffron/term-llm/internal/linkeddevices"
	"github.com/samsaffron/term-llm/internal/linkeddevices/testdevice"
	"github.com/samsaffron/term-llm/internal/pairing"
	"github.com/samsaffron/term-llm/internal/peer"
	"github.com/samsaffron/term-llm/internal/peerbroker"
)

func TestRuntimePairsConnectsAndRevokesRealDevice(t *testing.T) {
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	broker := httptest.NewServer(brokerHandler)
	defer broker.Close()

	// Pin transport to loopback like the peer bridge harness; host network/ICE
	// candidates must not make this protocol regression depend on external NICs.
	settings := &webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settings.SetInterfaceFilter(func(name string) bool { return name == "lo" || name == "lo0" })
	endpoint := peer.EndpointConfig{SettingEngine: settings}

	streamStarted, streamClosed := make(chan struct{}), make(chan struct{})
	peerPrincipal := make(chan linkeddevices.DevicePrincipal, 1)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/live/sessions/live_capability/audio/output" {
			principal, _ := linkeddevices.DeviceFromRequest(r)
			peerPrincipal <- principal
			if r.Header.Get("X-Term-LLM-Live-Audio-Capability") != "one-use-test-capability" {
				t.Error("peer HTTP profile stripped the Live audio capability header")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			close(streamStarted)
			<-r.Context().Done()
			close(streamClosed)
			return
		}
		if r.URL.Path == "/v1/sessions" {
			principal, _ := linkeddevices.DeviceFromRequest(r)
			peerPrincipal <- principal
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"s1"}]}`)
			return
		}
		http.NotFound(w, r)
	})
	dataDir := t.TempDir()
	runtime, err := linkeddevices.New(linkeddevices.Options{DataDir: dataDir, Target: "agent", StoreInstanceID: "store-one", BasePath: "/", Authenticated: true, AgentName: "agent", HostLabel: "Test agent", BrokerURL: broker.URL, Handler: inner, Endpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	owner := httptest.NewServer(runtime.Handler())
	defer owner.Close()
	response, err := http.Post(owner.URL+"/v1/linked-devices/invitations", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create invitation = %d: %s", response.StatusCode, data)
	}
	var invite linkeddevices.Status
	if err := json.NewDecoder(response.Body).Decode(&invite); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	// A late server Start racing the first owner action must not register a
	// second lease and invalidate the invitation socket.
	if err := runtime.Start(); err != nil {
		t.Fatal(err)
	}
	if invite.Invitation == nil || !strings.HasPrefix(invite.Invitation.QRDataURL, "data:image/png;base64,") {
		t.Fatalf("missing real invitation QR: %+v", invite)
	}
	if got := runtime.Status(); got.Invitation != nil {
		t.Fatal("status endpoint snapshot leaked invitation secrets")
	}

	sasCh := make(chan string, 1)
	deviceState := t.TempDir() + "/device.json"
	device, err := testdevice.New(testdevice.Config{DeviceName: "Synthetic test device", StatePath: deviceState, Endpoint: endpoint, OnSAS: func(code string) { sasCh <- code }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pairResult := make(chan error, 1)
	go func() { _, err := device.Pair(ctx, invite.Invitation.URL); pairResult <- err }()
	code := <-sasCh
	var pendingID string
	for pendingID == "" {
		select {
		case <-ctx.Done():
			t.Fatal("pairing request did not become pending")
		case <-time.After(10 * time.Millisecond):
			status := runtime.Status()
			if len(status.PendingRequests) == 1 {
				pendingID = status.PendingRequests[0].ID
			}
		}
	}
	response = postJSON(t, http.DefaultClient, owner.URL+"/v1/linked-devices/requests/"+pendingID+"/approve", map[string]string{"code": code})
	if response.StatusCode != http.StatusNoContent {
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("approve = %d: %s", response.StatusCode, data)
	}
	response.Body.Close()
	if err := <-pairResult; err != nil {
		t.Fatal(err)
	}

	session, err := device.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	response, err = session.HTTPClient().Get("http://peer/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"s1"`) {
		t.Fatalf("peer sessions = %d %s", response.StatusCode, body)
	}

	firstPrincipal := <-peerPrincipal
	if firstPrincipal.Lifetime == nil {
		t.Fatal("peer request did not carry a trusted connection lifetime")
	}

	// Server-owned enablement, identity and grants survive restart. The
	// invitation does not, and the device reconnects without another QR.
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	for {
		status := runtime.Status()
		if len(status.Devices) == 1 && status.Devices[0].Connection == "offline" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("disconnected device remained counted as live")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case <-firstPrincipal.Lifetime.Done():
	case <-ctx.Done():
		t.Fatal("disconnect did not cancel the trusted peer lifetime")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	owner.Close()
	runtime, err = linkeddevices.New(linkeddevices.Options{DataDir: dataDir, Target: "agent", StoreInstanceID: "store-one", BasePath: "/", Authenticated: true, AgentName: "agent", HostLabel: "Test agent", BrokerURL: broker.URL, Handler: inner, Endpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Start(); err != nil {
		t.Fatal(err)
	}
	owner = httptest.NewServer(runtime.Handler())
	defer owner.Close()
	device, err = testdevice.New(testdevice.Config{StatePath: deviceState, Endpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	session, err = device.Connect(ctx)
	if err != nil {
		t.Fatalf("reconnect after restart: %v", err)
	}
	defer session.Close()

	response, err = session.HTTPClient().Post("http://peer/v1/live/sessions/live_capability/%74ext", "application/json", strings.NewReader(`{"text":"alias"}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("encoded Live action alias = %d, want 403", response.StatusCode)
	}

	response, err = session.HTTPClient().Get("http://peer/v1/linked-devices")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("device owner API = %d, want 403", response.StatusCode)
	}

	pairing, ok := device.Pairing()
	if !ok {
		t.Fatal("device did not persist pairing")
	}
	streamRequest, _ := http.NewRequest(http.MethodGet, "http://peer/v1/live/sessions/live_capability/audio/output", nil)
	streamRequest.Header.Set("X-Term-LLM-Live-Audio-Capability", "one-use-test-capability")
	streamResponse, err := session.HTTPClient().Do(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	<-streamStarted
	principal := <-peerPrincipal
	if principal.DeviceID != pairing.DeviceID || principal.Lifetime == nil || principal.Lifetime.Err() != nil {
		t.Fatalf("peer principal has no active trusted lifetime: %+v", principal)
	}
	request, _ := http.NewRequest(http.MethodDelete, owner.URL+"/v1/linked-devices/devices/"+pairing.DeviceID, nil)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d", response.StatusCode)
	}
	select {
	case <-streamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("revocation did not cancel the active SSE stream within two seconds")
	}
	select {
	case <-principal.Lifetime.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("revocation did not cancel the trusted peer lifetime")
	}
	_ = streamResponse.Body.Close()
	if _, err := device.Connect(ctx); err == nil {
		t.Fatal("revoked device reconnected")
	}
}

func TestRuntimeDiagnosticsReportActualPionLifecycleAndRequest(t *testing.T) {
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer brokerHandler.Close()
	broker := httptest.NewServer(brokerHandler)
	defer broker.Close()

	clock := &runtimeDiagnosticClock{now: time.Now()}
	collector := newRuntimeDiagnosticCollector()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "models")
	})
	runtime, err := linkeddevices.New(linkeddevices.Options{
		DataDir: t.TempDir(), Target: "diagnostics", StoreInstanceID: "diagnostics-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL, Handler: handler,
		Now: clock.Next, Diagnostics: collector.Observe,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sas := make(chan string, 1)
	device, err := testdevice.New(testdevice.Config{
		StatePath: t.TempDir() + "/device.json", Now: clock.Next,
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
	response, err := session.HTTPClient().Get("http://peer/v1/models?token=must-not-appear")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || string(body) != "models" {
		t.Fatalf("response = %q, %v", body, readErr)
	}

	events := collector.waitFor(ctx, t, peer.DiagnosticRequestEnd)
	ordered := []peer.DiagnosticStage{
		peer.DiagnosticBrokerConnected, peer.DiagnosticIdentityVerified, peer.DiagnosticICEConnected,
		peer.DiagnosticChannelOpen, peer.DiagnosticRequestStart, peer.DiagnosticResponseHeaders, peer.DiagnosticRequestEnd,
	}
	last := -1
	for _, stage := range ordered {
		index := diagnosticStageIndex(events, stage)
		if index <= last {
			t.Fatalf("diagnostic stage %q index=%d after=%d; events=%+v", stage, index, last, events)
		}
		last = index
	}
	identity := diagnosticEventForStage(events, peer.DiagnosticIdentityVerified)
	ice := diagnosticEventForStage(events, peer.DiagnosticICEConnected)
	channel := diagnosticEventForStage(events, peer.DiagnosticChannelOpen)
	start := diagnosticEventForStage(events, peer.DiagnosticRequestStart)
	headers := diagnosticEventForStage(events, peer.DiagnosticResponseHeaders)
	end := diagnosticEventForStage(events, peer.DiagnosticRequestEnd)
	if identity.Generation == 0 || identity.Connection == 0 || ice.Generation != identity.Generation || ice.Connection != identity.Connection ||
		channel.Generation != identity.Generation || channel.Connection != identity.Connection || start.Connection != identity.Connection {
		t.Fatalf("diagnostic ownership mismatch: identity=%+v ice=%+v channel=%+v start=%+v", identity, ice, channel, start)
	}
	if ice.Candidate != peer.DiagnosticCandidateHost && ice.Candidate != peer.DiagnosticCandidatePrflx && ice.Candidate != peer.DiagnosticCandidateUnknown {
		t.Fatalf("loopback candidate class = %q, want host, prflx, or unknown", ice.Candidate)
	}
	if start.Method != peer.DiagnosticMethodGET || start.Route != peer.DiagnosticRouteModels || headers.Status != http.StatusCreated ||
		end.Status != http.StatusCreated || end.Bytes != uint64(len("models")) || end.Duration <= 0 || end.Request != start.Request {
		t.Fatalf("request diagnostics start=%+v headers=%+v end=%+v", start, headers, end)
	}
	if diagnosticTerminalCount(events, start.Connection, start.Request) != 1 {
		t.Fatalf("request terminal count != 1: %+v", events)
	}
}

func TestConcurrentRequestsFailAcrossReconnectWithoutMutationReplay(t *testing.T) {
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer brokerHandler.Close()
	broker := httptest.NewServer(brokerHandler)
	defer broker.Close()

	mutationStarted := make(chan struct{})
	readStarted := make(chan struct{})
	mutationCanceled := make(chan struct{})
	readCanceled := make(chan struct{})
	releaseOld := make(chan struct{})
	oldHandlersDone := make(chan struct{}, 2)
	lateMutationWrite := make(chan error, 1)
	var mutationDispatches atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
			mutationDispatches.Add(1)
			close(mutationStarted)
			<-r.Context().Done()
			close(mutationCanceled)
			<-releaseOld
			_, err := io.WriteString(w, "late mutation response")
			lateMutationWrite <- err
			oldHandlersDone <- struct{}{}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/status":
			close(readStarted)
			<-r.Context().Done()
			close(readCanceled)
			<-releaseOld
			oldHandlersDone <- struct{}{}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			_, _ = io.WriteString(w, "fresh-generation")
		default:
			http.NotFound(w, r)
		}
	})
	collector := newRuntimeDiagnosticCollector()
	runtime, err := linkeddevices.New(linkeddevices.Options{
		DataDir: t.TempDir(), Target: "failure-generation", StoreInstanceID: "failure-generation-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL, Handler: handler, Diagnostics: collector.Observe,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sas := make(chan string, 1)
	device, err := testdevice.New(testdevice.Config{StatePath: t.TempDir() + "/device.json", OnSAS: func(code string) { sas <- code }})
	if err != nil {
		t.Fatal(err)
	}
	pairRuntimeDevice(t, ctx, runtime, device, sas)
	oldSession, err := device.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer oldSession.Close()

	mutationResult := make(chan error, 1)
	go func() {
		request, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://peer/v1/responses", strings.NewReader("{}"))
		response, requestErr := oldSession.HTTPClient().Do(request)
		if response != nil {
			response.Body.Close()
		}
		mutationResult <- requestErr
	}()
	readResult := make(chan error, 1)
	go func() {
		response, requestErr := oldSession.HTTPClient().Get("http://peer/v1/sessions/status")
		if response != nil {
			response.Body.Close()
		}
		readResult <- requestErr
	}()
	waitTestSignal(t, ctx, mutationStarted, "mutation dispatch")
	waitTestSignal(t, ctx, readStarted, "read dispatch")
	if err := oldSession.Close(); err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]<-chan error{"mutation": mutationResult, "read": readResult} {
		select {
		case err := <-result:
			if err == nil {
				t.Fatalf("%s request completed successfully after connection loss", name)
			}
		case <-ctx.Done():
			t.Fatalf("%s request remained pending after connection loss", name)
		}
	}
	waitTestSignal(t, ctx, mutationCanceled, "mutation handler cancellation")
	waitTestSignal(t, ctx, readCanceled, "read handler cancellation")

	newSession, err := device.Connect(ctx)
	if err != nil {
		t.Fatalf("reconnect while old handlers drain: %v", err)
	}
	defer newSession.Close()
	assertFreshGenerationRead(t, newSession)
	close(releaseOld)
	waitTestSignal(t, ctx, oldHandlersDone, "old mutation handler drain")
	waitTestSignal(t, ctx, oldHandlersDone, "old read handler drain")
	if err := <-lateMutationWrite; err == nil {
		t.Fatal("old mutation handler published a late response after reconnect")
	}
	waitRuntimeStatus(t, ctx, runtime, func(status linkeddevices.Status) bool {
		return len(status.Devices) == 1 && status.Devices[0].Connection != "offline"
	})
	assertFreshGenerationRead(t, newSession)
	if got := mutationDispatches.Load(); got != 1 {
		t.Fatalf("uncertain mutation dispatched %d times, want exactly once", got)
	}
	events := collector.waitForTerminalCount(ctx, t, 4)
	var oldConnection, newConnection uint64
	for _, event := range events {
		if event.Stage != peer.DiagnosticRequestStart {
			continue
		}
		switch event.Route {
		case peer.DiagnosticRouteResponses, peer.DiagnosticRouteSessionStatus:
			if oldConnection == 0 {
				oldConnection = event.Connection
			} else if event.Connection != oldConnection {
				t.Fatalf("old concurrent requests crossed connections: %+v", events)
			}
		case peer.DiagnosticRouteModels:
			if newConnection == 0 {
				newConnection = event.Connection
			} else if event.Connection != newConnection {
				t.Fatalf("new reads crossed connections: %+v", events)
			}
		}
	}
	if oldConnection == 0 || newConnection == 0 || oldConnection == newConnection {
		t.Fatalf("diagnostic connection ownership old=%d new=%d events=%+v", oldConnection, newConnection, events)
	}
	for _, event := range events {
		if event.Stage == peer.DiagnosticRequestStart && diagnosticTerminalCount(events, event.Connection, event.Request) != 1 {
			t.Fatalf("request connection=%d request=%d terminal count != 1: %+v", event.Connection, event.Request, events)
		}
	}
}

func assertFreshGenerationRead(t *testing.T, session *testdevice.Session) {
	t.Helper()
	response, err := session.HTTPClient().Get("http://peer/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || string(body) != "fresh-generation" {
		t.Fatalf("fresh read status=%d body=%q err=%v", response.StatusCode, body, readErr)
	}
}

func waitTestSignal(t *testing.T, ctx context.Context, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestReplacementDuringAcceptedRenewalCannotReceivePairAnswerOrLeaveGrant(t *testing.T) {
	now := time.Now()
	provider := &trackingTURNProvider{urls: []string{"turns:managed.example:5349?transport=tcp"}, now: now}
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{TURNProvider: provider})
	if err != nil {
		t.Fatal(err)
	}
	controlled := newBlockedRenewBroker(brokerHandler)
	broker := httptest.NewServer(controlled)
	defer broker.Close()
	defer brokerHandler.Close()
	staticCredential := strings.Repeat("s", peerbroker.MinCredentialLen)
	policy := peerbroker.NewHostICEConfig([]peerbroker.ConfiguredICEServer{
		{URLs: []string{"stun:stun.example:3478"}},
		{URLs: []string{"turns:static.example:5349?transport=tcp"}, CredentialSource: peerbroker.CredentialSourceStatic, Username: "static-user", Credential: staticCredential},
		{URLs: provider.URLs(), CredentialSource: peerbroker.CredentialSourceBroker},
	})
	runtime, err := linkeddevices.New(linkeddevices.Options{
		DataDir: t.TempDir(), Target: "renew-duplicate", StoreInstanceID: "renew-duplicate-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL, ICEPolicy: &policy, Handler: http.NotFoundHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var invitationURL string
	if err := runtime.NewInvitation(ctx, func(raw string) (string, error) { invitationURL = raw; return "qr", nil }); err != nil {
		t.Fatal(err)
	}
	link, err := pairing.ParseLink(invitationURL)
	if err != nil {
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
	waitRuntimeStatus(t, ctx, runtime, func(status linkeddevices.Status) bool {
		if len(status.PendingRequests) == 1 {
			pendingID = status.PendingRequests[0].ID
		}
		return pendingID != ""
	})
	approveResult := make(chan error, 1)
	go func() { approveResult <- runtime.Approve(ctx, pendingID, code) }()
	select {
	case <-controlled.accepted:
	case <-ctx.Done():
		t.Fatal("approved reconnect capability hash did not reach broker renewal")
	}
	client, err := peerbroker.NewClient(link.Broker, nil)
	if err != nil {
		t.Fatal(err)
	}
	hostHash := sha256.Sum256(link.HostKey)
	hostID := base64.RawURLEncoding.EncodeToString(hostHash[:])
	if _, err := client.DialInvitation(ctx, hostID, link.BrokerInvitationID(), link.InvitationCapability); err == nil {
		t.Fatal("replacement invitation socket was accepted during renewal")
	}
	waitRuntimeStatus(t, ctx, runtime, func(status linkeddevices.Status) bool { return len(status.PendingRequests) == 0 })
	close(controlled.release)
	if err := <-approveResult; err == nil {
		t.Fatal("cancelled approval completed after replacement")
	}
	if err := <-pairResult; err == nil {
		t.Fatal("original pairing received a reconnect capability after replacement")
	}
	if status := runtime.Status(); len(status.Devices) != 0 {
		t.Fatalf("cancelled approval left a ghost grant: %+v", status.Devices)
	}
	if got := provider.calls.Load(); got != 0 {
		t.Fatalf("cancelled invitation reached broker TURN provider %d times", got)
	}
}

func TestSilentDuplicateInvitationSocketBurnsPendingApproval(t *testing.T) {
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	broker := httptest.NewServer(brokerHandler)
	defer broker.Close()
	defer brokerHandler.Close()
	runtime, err := linkeddevices.New(linkeddevices.Options{
		DataDir: t.TempDir(), Target: "silent-duplicate", StoreInstanceID: "silent-duplicate-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL, Handler: http.NotFoundHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	owner := httptest.NewServer(runtime.Handler())
	defer owner.Close()
	response, err := http.Post(owner.URL+"/v1/linked-devices/invitations", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var invitation linkeddevices.Status
	if err := json.NewDecoder(response.Body).Decode(&invitation); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if invitation.Invitation == nil {
		t.Fatal("invitation missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sas := make(chan string, 1)
	first, err := testdevice.New(testdevice.Config{DeviceName: "First", StatePath: t.TempDir() + "/first.json", OnSAS: func(code string) { sas <- code }})
	if err != nil {
		t.Fatal(err)
	}
	firstResult := make(chan error, 1)
	go func() { _, pairErr := first.Pair(ctx, invitation.Invitation.URL); firstResult <- pairErr }()
	var code, pendingID string
	select {
	case code = <-sas:
	case <-ctx.Done():
		t.Fatal("first device did not authenticate invitation")
	}
	waitRuntimeStatus(t, ctx, runtime, func(status linkeddevices.Status) bool {
		if len(status.PendingRequests) == 1 {
			pendingID = status.PendingRequests[0].ID
		}
		return pendingID != ""
	})
	link, err := pairing.ParseLink(invitation.Invitation.URL)
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
		t.Fatal("silent duplicate invitation socket was accepted")
	}
	waitRuntimeStatus(t, ctx, runtime, func(status linkeddevices.Status) bool {
		return len(status.PendingRequests) == 0 && len(status.Warnings) > 0
	})
	if err := runtime.Approve(ctx, pendingID, code); err == nil {
		t.Fatal("silent duplicate left the first pending request approvable")
	}
	select {
	case err := <-firstResult:
		if err == nil {
			t.Fatal("superseded first pairing unexpectedly completed")
		}
	case <-ctx.Done():
		t.Fatal("superseded first pairing did not terminate")
	}
}

func TestDuplicateInvitationThroughBrokerBurnsPendingApproval(t *testing.T) {
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	broker := httptest.NewServer(brokerHandler)
	defer broker.Close()
	defer brokerHandler.Close()
	runtime, err := linkeddevices.New(linkeddevices.Options{
		DataDir: t.TempDir(), Target: "duplicate", StoreInstanceID: "duplicate-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL, Handler: http.NotFoundHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	owner := httptest.NewServer(runtime.Handler())
	defer owner.Close()
	response, err := http.Post(owner.URL+"/v1/linked-devices/invitations", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var invitation linkeddevices.Status
	if err := json.NewDecoder(response.Body).Decode(&invitation); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if invitation.Invitation == nil {
		t.Fatal("invitation missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	firstSAS, secondSAS := make(chan string, 1), make(chan string, 1)
	first, _ := testdevice.New(testdevice.Config{DeviceName: "First", StatePath: t.TempDir() + "/first.json", OnSAS: func(code string) { firstSAS <- code }})
	second, _ := testdevice.New(testdevice.Config{DeviceName: "Second", StatePath: t.TempDir() + "/second.json", OnSAS: func(code string) { secondSAS <- code }})
	firstResult := make(chan error, 1)
	go func() { _, pairErr := first.Pair(ctx, invitation.Invitation.URL); firstResult <- pairErr }()
	select {
	case <-firstSAS:
	case <-ctx.Done():
		t.Fatal("first device did not authenticate invitation")
	}
	var pendingID string
	waitRuntimeStatus(t, ctx, runtime, func(status linkeddevices.Status) bool {
		if len(status.PendingRequests) == 1 {
			pendingID = status.PendingRequests[0].ID
		}
		return pendingID != ""
	})
	secondResult := make(chan error, 1)
	go func() { _, pairErr := second.Pair(ctx, invitation.Invitation.URL); secondResult <- pairErr }()
	select {
	case err := <-secondResult:
		if err == nil {
			t.Fatal("duplicate invitation pairing unexpectedly completed")
		}
	case <-ctx.Done():
		t.Fatal("duplicate invitation socket was not rejected")
	}
	select {
	case code := <-secondSAS:
		t.Fatalf("duplicate invitation reached pairing authority with SAS %q", code)
	default:
	}
	waitRuntimeStatus(t, ctx, runtime, func(status linkeddevices.Status) bool {
		return len(status.PendingRequests) == 0 && len(status.Warnings) > 0
	})
	if err := runtime.Approve(ctx, pendingID, "000000"); err == nil {
		t.Fatal("burned duplicate invitation remained approvable")
	}
	select {
	case err := <-firstResult:
		if err == nil {
			t.Fatal("superseded first pairing unexpectedly completed")
		}
	case <-ctx.Done():
		t.Fatal("superseded first pairing did not terminate")
	}
}

func TestStaleInvitationResponseCannotOverwriteResetGeneration(t *testing.T) {
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	controlled := newBlockedInvitationBroker(brokerHandler)
	broker := httptest.NewServer(controlled)
	defer broker.Close()
	defer brokerHandler.Close()
	runtime, err := linkeddevices.New(linkeddevices.Options{
		DataDir: t.TempDir(), Target: "stale-invitation", StoreInstanceID: "stale-invitation-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL, Handler: http.NotFoundHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	oldResult := make(chan error, 1)
	go func() { oldResult <- runtime.NewInvitation(ctx, func(string) (string, error) { return "old-qr", nil }) }()
	select {
	case <-controlled.accepted:
	case <-ctx.Done():
		t.Fatal("old invitation registration did not reach broker")
	}
	if err := runtime.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	var replacementURL string
	if err := runtime.NewInvitation(ctx, func(raw string) (string, error) { replacementURL = raw; return "new-qr", nil }); err != nil {
		t.Fatal(err)
	}
	sas := make(chan string, 1)
	device, err := testdevice.New(testdevice.Config{DeviceName: "Replacement", StatePath: t.TempDir() + "/replacement.json", OnSAS: func(code string) { sas <- code }})
	if err != nil {
		t.Fatal(err)
	}
	pairResult := make(chan error, 1)
	go func() { _, pairErr := device.Pair(ctx, replacementURL); pairResult <- pairErr }()
	code := <-sas
	var pendingID string
	waitRuntimeStatus(t, ctx, runtime, func(status linkeddevices.Status) bool {
		if len(status.PendingRequests) == 1 {
			pendingID = status.PendingRequests[0].ID
		}
		return pendingID != ""
	})
	close(controlled.release)
	if err := <-oldResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("stale invitation result = %v, want context canceled", err)
	}
	status := runtime.Status()
	if len(status.PendingRequests) != 1 || status.PendingRequests[0].ID != pendingID {
		t.Fatalf("stale invitation replaced current pending request: %+v", status)
	}
	if err := runtime.Approve(ctx, pendingID, code); err != nil {
		t.Fatal(err)
	}
	if err := <-pairResult; err != nil {
		t.Fatalf("replacement invitation did not pair: %v", err)
	}
}

func TestRegistrationCannotPublishAcrossResetResume(t *testing.T) {
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	controlled := newControlledReadyBroker(brokerHandler)
	broker := httptest.NewServer(controlled)
	defer broker.Close()
	defer brokerHandler.Close()
	defer close(controlled.finish)
	runtime, err := linkeddevices.New(linkeddevices.Options{
		DataDir: t.TempDir(), Target: "registration-generation", StoreInstanceID: "registration-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL, Handler: http.NotFoundHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	oldFingerprint := runtime.Status().HostFingerprint
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	firstEnable := make(chan error, 1)
	go func() { firstEnable <- runtime.Enable(ctx) }()
	select {
	case <-controlled.blocked:
	case <-ctx.Done():
		t.Fatal("initial registration did not reach controlled ready")
	}
	resetDone := make(chan error, 1)
	go func() { resetDone <- runtime.Reset(ctx) }()
	select {
	case <-controlled.canceled:
	case <-time.After(time.Second):
		close(controlled.release)
		t.Fatal("reset did not cancel the generation-owned ready wait")
	}
	resumeDone := make(chan error, 1)
	go func() { resumeDone <- runtime.Enable(ctx) }()
	close(controlled.release)
	if err := <-resetDone; err != nil {
		t.Fatal(err)
	}
	if err := <-resumeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-firstEnable; err == nil {
		t.Fatal("stale initial registration unexpectedly succeeded")
	}
	status := runtime.Status()
	if !status.BrokerConnected || status.HostFingerprint == oldFingerprint {
		t.Fatalf("post-reset status = %+v, want connected replacement identity", status)
	}

	owner := httptest.NewServer(runtime.Handler())
	defer owner.Close()
	response, err := http.Post(owner.URL+"/v1/linked-devices/invitations", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var invitation linkeddevices.Status
	if err := json.NewDecoder(response.Body).Decode(&invitation); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || invitation.Invitation == nil {
		t.Fatalf("post-reset invitation = %d %+v", response.StatusCode, invitation)
	}
	sas := make(chan string, 1)
	device, err := testdevice.New(testdevice.Config{DeviceName: "Replacement identity", StatePath: t.TempDir() + "/device.json", OnSAS: func(code string) { sas <- code }})
	if err != nil {
		t.Fatal(err)
	}
	paired := make(chan error, 1)
	go func() { _, pairErr := device.Pair(ctx, invitation.Invitation.URL); paired <- pairErr }()
	var code, pendingID string
	select {
	case code = <-sas:
	case <-ctx.Done():
		t.Fatal("post-reset pairing did not report SAS")
	}
	waitRuntimeStatus(t, ctx, runtime, func(status linkeddevices.Status) bool {
		if len(status.PendingRequests) == 1 {
			pendingID = status.PendingRequests[0].ID
		}
		return pendingID != ""
	})
	if err := runtime.Approve(ctx, pendingID, code); err != nil {
		t.Fatal(err)
	}
	if err := <-paired; err != nil {
		t.Fatalf("pairing through replacement identity: %v", err)
	}
}

type blockedInvitationBroker struct {
	next     http.Handler
	once     sync.Once
	accepted chan struct{}
	release  chan struct{}
}

func newBlockedInvitationBroker(next http.Handler) *blockedInvitationBroker {
	return &blockedInvitationBroker{next: next, accepted: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockedInvitationBroker) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	blocked := false
	if strings.HasSuffix(request.URL.Path, "/invitations") {
		b.once.Do(func() { blocked = true })
	}
	if !blocked {
		b.next.ServeHTTP(w, request)
		return
	}
	recorder := httptest.NewRecorder()
	b.next.ServeHTTP(recorder, request)
	close(b.accepted)
	<-b.release
	for key, values := range recorder.Header() {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

type trackingTURNProvider struct {
	urls  []string
	now   time.Time
	calls atomic.Int64
}

func (p *trackingTURNProvider) URLs() []string { return append([]string(nil), p.urls...) }
func (p *trackingTURNProvider) Mint(_ context.Context, subject string, urls []string) (peerbroker.ICECredentials, error) {
	p.calls.Add(1)
	return peerbroker.ICECredentials{
		ICEServers: []peerbroker.ICEServer{{URLs: append([]string(nil), urls...), Username: "managed:" + subject, Credential: "managed-credential"}},
		ExpiresAt:  p.now.Add(time.Hour),
	}, nil
}

type blockedRenewBroker struct {
	next     http.Handler
	once     sync.Once
	accepted chan struct{}
	release  chan struct{}
}

func newBlockedRenewBroker(next http.Handler) *blockedRenewBroker {
	return &blockedRenewBroker{next: next, accepted: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockedRenewBroker) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	blocked := false
	if strings.HasSuffix(request.URL.Path, "/renew") {
		b.once.Do(func() { blocked = true })
	}
	if !blocked {
		b.next.ServeHTTP(w, request)
		return
	}
	recorder := httptest.NewRecorder()
	b.next.ServeHTTP(recorder, request)
	close(b.accepted)
	<-b.release
	for key, values := range recorder.Header() {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

type controlledReadyBroker struct {
	next      http.Handler
	mu        sync.Mutex
	intercept bool
	blocked   chan struct{}
	canceled  chan struct{}
	release   chan struct{}
	finish    chan struct{}
}

func newControlledReadyBroker(next http.Handler) *controlledReadyBroker {
	return &controlledReadyBroker{
		next: next, blocked: make(chan struct{}), canceled: make(chan struct{}),
		release: make(chan struct{}), finish: make(chan struct{}),
	}
}

func (b *controlledReadyBroker) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	b.mu.Lock()
	intercept := !b.intercept && websocket.IsWebSocketUpgrade(request) && strings.HasSuffix(request.URL.Path, "/ws") && request.URL.RawQuery == ""
	if intercept {
		b.intercept = true
	}
	b.mu.Unlock()
	if !intercept {
		b.next.ServeHTTP(w, request)
		return
	}
	conn, err := (&websocket.Upgrader{Subprotocols: []string{peerbroker.WSSubprotocol}, CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, request, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	close(b.blocked)
	readDone := make(chan struct{})
	go func() {
		_, _, _ = conn.ReadMessage()
		close(readDone)
	}()
	select {
	case <-readDone:
		close(b.canceled)
	case <-b.release:
		if conn.WriteJSON(peerbroker.Frame{Type: peerbroker.FrameReady, Role: peerbroker.PeerHost}) == nil {
			select {
			case <-readDone:
			case <-b.finish:
			}
		}
	}
}

type replaceableBroker struct {
	mu      sync.RWMutex
	handler http.Handler
}

func (b *replaceableBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	handler.ServeHTTP(w, r)
}

func (b *replaceableBroker) replace(handler http.Handler) {
	b.mu.Lock()
	b.handler = handler
	b.mu.Unlock()
}

func TestReconnectCancelsInvitationThatBrokerLeaseCannotRetain(t *testing.T) {
	first, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	switcher := &replaceableBroker{handler: first}
	broker := httptest.NewServer(switcher)
	defer broker.Close()
	defer first.Close()
	runtime, err := linkeddevices.New(linkeddevices.Options{
		DataDir: t.TempDir(), Target: "reconnect-invite", StoreInstanceID: "reconnect-invite-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL, Handler: http.NotFoundHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := runtime.NewInvitation(ctx, func(string) (string, error) { return "qr", nil }); err != nil {
		t.Fatal(err)
	}
	if status := runtime.Status(); status.State != linkeddevices.StateInviting {
		t.Fatalf("invitation was not advertised before reconnect: %+v", status)
	}
	first.Close()
	second, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	switcher.replace(second)
	waitRuntimeStatus(t, ctx, runtime, func(status linkeddevices.Status) bool {
		return status.BrokerConnected && status.State != linkeddevices.StateInviting && slices.ContainsFunc(status.Warnings, func(warning string) bool {
			return strings.Contains(warning, "the open invitation was cancelled. Select Connect to create a new one.")
		})
	})
}

func TestRuntimeRecoversBrokerRestartAndStopResume(t *testing.T) {
	first, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	switcher := &replaceableBroker{handler: first}
	broker := httptest.NewServer(switcher)
	defer broker.Close()
	defer first.Close()

	collector := newRuntimeDiagnosticCollector()
	runtime, err := linkeddevices.New(linkeddevices.Options{
		DataDir: t.TempDir(), Target: "restart", StoreInstanceID: "restart-store", BasePath: "/",
		Authenticated: true, BrokerURL: broker.URL, Handler: http.NotFoundHandler(), Diagnostics: collector.Observe,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sas := make(chan string, 1)
	device, err := testdevice.New(testdevice.Config{
		DeviceName: "Restart device", StatePath: t.TempDir() + "/device.json",
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
	_ = session.Close()

	first.Close()
	waitRuntimeStatus(t, ctx, runtime, func(status linkeddevices.Status) bool { return !status.BrokerConnected })
	second, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	switcher.replace(second)
	waitRuntimeStatus(t, ctx, runtime, func(status linkeddevices.Status) bool { return status.BrokerConnected })
	events := collector.waitForCount(ctx, t, peer.DiagnosticBrokerConnected, 2)
	if diagnosticStageIndex(events, peer.DiagnosticBrokerDisconnected) < 0 || diagnosticStageIndex(events, peer.DiagnosticRecoveryAttempt) < 0 {
		t.Fatalf("broker recovery diagnostics = %+v", events)
	}
	connected := 0
	for _, event := range events {
		if event.Stage == peer.DiagnosticBrokerConnected {
			connected++
		}
	}
	if connected < 2 {
		t.Fatalf("broker connected events = %d, want initial and recovered; events=%+v", connected, events)
	}
	session, err = device.Connect(ctx)
	if err != nil {
		t.Fatalf("connect after broker restart: %v", err)
	}
	_ = session.Close()

	if err := runtime.Stop(); err != nil {
		t.Fatal(err)
	}
	if status := runtime.Status(); status.State != linkeddevices.StateStopped {
		t.Fatalf("stopped status = %+v", status)
	}
	resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer resumeCancel()
	if err := runtime.Enable(resumeCtx); err != nil {
		t.Fatal(err)
	}
	waitRuntimeStatus(t, resumeCtx, runtime, func(status linkeddevices.Status) bool { return status.BrokerConnected })
	session, err = device.Connect(resumeCtx)
	if err != nil {
		t.Fatalf("connect after resume: %v", err)
	}
	_ = session.Close()
}

type runtimeDiagnosticClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *runtimeDiagnosticClock) Next() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Millisecond)
	return c.now
}

type runtimeDiagnosticCollector struct {
	mu     sync.Mutex
	events []peer.DiagnosticEvent
	notify chan struct{}
}

func newRuntimeDiagnosticCollector() *runtimeDiagnosticCollector {
	return &runtimeDiagnosticCollector{notify: make(chan struct{}, 64)}
}

func (c *runtimeDiagnosticCollector) Observe(event peer.DiagnosticEvent) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (c *runtimeDiagnosticCollector) snapshot() []peer.DiagnosticEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]peer.DiagnosticEvent(nil), c.events...)
}

func (c *runtimeDiagnosticCollector) waitFor(ctx context.Context, t *testing.T, stage peer.DiagnosticStage) []peer.DiagnosticEvent {
	t.Helper()
	for {
		events := c.snapshot()
		if diagnosticStageIndex(events, stage) >= 0 {
			return events
		}
		select {
		case <-c.notify:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for diagnostic stage %q; events=%+v", stage, events)
		}
	}
}

func (c *runtimeDiagnosticCollector) waitForCount(ctx context.Context, t *testing.T, stage peer.DiagnosticStage, want int) []peer.DiagnosticEvent {
	t.Helper()
	for {
		events := c.snapshot()
		count := 0
		for _, event := range events {
			if event.Stage == stage {
				count++
			}
		}
		if count >= want {
			return events
		}
		select {
		case <-c.notify:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %d diagnostic stages %q; events=%+v", want, stage, events)
		}
	}
}

func (c *runtimeDiagnosticCollector) waitForTerminalCount(ctx context.Context, t *testing.T, want int) []peer.DiagnosticEvent {
	t.Helper()
	for {
		events := c.snapshot()
		count := 0
		for _, event := range events {
			if event.Stage == peer.DiagnosticRequestEnd || event.Stage == peer.DiagnosticRequestCancel || event.Stage == peer.DiagnosticRequestFailure {
				count++
			}
		}
		if count >= want {
			return events
		}
		select {
		case <-c.notify:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %d terminal diagnostics; events=%+v", want, events)
		}
	}
}

func diagnosticStageIndex(events []peer.DiagnosticEvent, stage peer.DiagnosticStage) int {
	for index, event := range events {
		if event.Stage == stage {
			return index
		}
	}
	return -1
}

func diagnosticEventForStage(events []peer.DiagnosticEvent, stage peer.DiagnosticStage) peer.DiagnosticEvent {
	index := diagnosticStageIndex(events, stage)
	if index < 0 {
		return peer.DiagnosticEvent{}
	}
	return events[index]
}

func diagnosticTerminalCount(events []peer.DiagnosticEvent, connection, request uint64) int {
	count := 0
	for _, event := range events {
		if event.Connection == connection && event.Request == request &&
			(event.Stage == peer.DiagnosticRequestEnd || event.Stage == peer.DiagnosticRequestCancel || event.Stage == peer.DiagnosticRequestFailure) {
			count++
		}
	}
	return count
}

func pairRuntimeDevice(t *testing.T, ctx context.Context, runtime *linkeddevices.Runtime, device *testdevice.Device, sas <-chan string) {
	t.Helper()
	owner := httptest.NewServer(runtime.Handler())
	defer owner.Close()
	response, err := http.Post(owner.URL+"/v1/linked-devices/invitations", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var invite linkeddevices.Status
	if err := json.NewDecoder(response.Body).Decode(&invite); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if invite.Invitation == nil {
		t.Fatal("invitation missing")
	}
	paired := make(chan error, 1)
	go func() { _, pairErr := device.Pair(ctx, invite.Invitation.URL); paired <- pairErr }()
	var code string
	select {
	case code = <-sas:
	case <-ctx.Done():
		t.Fatal("SAS was not reported")
	}
	var pending string
	waitRuntimeStatus(t, ctx, runtime, func(status linkeddevices.Status) bool {
		if len(status.PendingRequests) == 1 {
			pending = status.PendingRequests[0].ID
		}
		return pending != ""
	})
	if err := runtime.Approve(ctx, pending, code); err != nil {
		t.Fatal(err)
	}
	if err := <-paired; err != nil {
		t.Fatal(err)
	}
}

func waitRuntimeStatus(t *testing.T, ctx context.Context, runtime *linkeddevices.Runtime, ready func(linkeddevices.Status) bool) {
	t.Helper()
	for {
		if ready(runtime.Status()) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for linked devices status")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func postJSON(t *testing.T, client *http.Client, url string, value any) *http.Response {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

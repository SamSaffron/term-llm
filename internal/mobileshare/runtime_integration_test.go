package mobileshare_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/mobileshare"
	"github.com/samsaffron/term-llm/internal/mobileshare/testdevice"
	"github.com/samsaffron/term-llm/internal/peerbroker"
)

func TestRuntimePairsConnectsAndRevokesRealDevice(t *testing.T) {
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	broker := httptest.NewServer(brokerHandler)
	defer broker.Close()

	streamStarted, streamClosed := make(chan struct{}), make(chan struct{})
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
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
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"s1"}]}`)
			return
		}
		http.NotFound(w, r)
	})
	dataDir := t.TempDir()
	runtime, err := mobileshare.New(mobileshare.Options{DataDir: dataDir, Target: "agent", StoreInstanceID: "store-one", BasePath: "/", Authenticated: true, AgentName: "agent", HostLabel: "Test agent", BrokerURL: broker.URL, Handler: inner})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	owner := httptest.NewServer(runtime.Handler())
	defer owner.Close()
	response, err := http.Post(owner.URL+"/v1/mobile-sharing/invitations", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create invitation = %d: %s", response.StatusCode, data)
	}
	var invite mobileshare.Status
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
	device, err := testdevice.New(testdevice.Config{DeviceName: "Synthetic test device", StatePath: deviceState, OnSAS: func(code string) { sasCh <- code }})
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
	response = postJSON(t, http.DefaultClient, owner.URL+"/v1/mobile-sharing/requests/"+pendingID+"/approve", map[string]string{"code": code})
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

	// Server-owned enablement, identity and grants survive restart. The
	// invitation does not, and the device reconnects without another QR.
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	owner.Close()
	runtime, err = mobileshare.New(mobileshare.Options{DataDir: dataDir, Target: "agent", StoreInstanceID: "store-one", BasePath: "/", Authenticated: true, AgentName: "agent", HostLabel: "Test agent", BrokerURL: broker.URL, Handler: inner})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Start(); err != nil {
		t.Fatal(err)
	}
	owner = httptest.NewServer(runtime.Handler())
	defer owner.Close()
	device, err = testdevice.New(testdevice.Config{StatePath: deviceState})
	if err != nil {
		t.Fatal(err)
	}
	session, err = device.Connect(ctx)
	if err != nil {
		t.Fatalf("reconnect after restart: %v", err)
	}
	defer session.Close()

	response, err = session.HTTPClient().Get("http://peer/v1/mobile-sharing")
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
	streamResponse, err := session.HTTPClient().Get("http://peer/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	<-streamStarted
	request, _ := http.NewRequest(http.MethodDelete, owner.URL+"/v1/mobile-sharing/devices/"+pairing.DeviceID, nil)
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
	_ = streamResponse.Body.Close()
	if _, err := device.Connect(ctx); err == nil {
		t.Fatal("revoked device reconnected")
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

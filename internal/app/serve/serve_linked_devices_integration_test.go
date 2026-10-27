package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/samsaffron/term-llm/internal/linkeddevices"
	"github.com/samsaffron/term-llm/internal/linkeddevices/testdevice"
	"github.com/samsaffron/term-llm/internal/peer"
	"github.com/samsaffron/term-llm/internal/peerbroker"
)

func TestLinkedDevicesDispatchesMountedServeHandler(t *testing.T) {
	for _, basePath := range []string{"/", "/chat", "/deep/mount"} {
		t.Run(basePath, func(t *testing.T) {
			client, closeFixture := mountedPeerServeFixture(t, basePath)
			defer closeFixture()

			root := peerWirePath(basePath, "/")
			assertPeerResponse(t, client, http.MethodGet, root, nil, http.StatusOK, "text/html", "TERM_LLM_UI_PREFIX")
			if basePath != "/" {
				assertPeerResponse(t, client, http.MethodGet, basePath, nil, http.StatusOK, "text/html", basePath)
			}
			asset := "/" + uiBuildAsset(t, "dist/app.js")
			assertPeerResponse(t, client, http.MethodGet, peerWirePath(basePath, asset), nil, http.StatusOK, "javascript", "")
			assertPeerResponse(t, client, http.MethodGet, peerWirePath(basePath, "/v1/sessions"), nil, http.StatusOK, "application/json", "sessions")
			assertPeerResponse(t, client, http.MethodGet, peerWirePath(basePath, "/v1/capabilities"), nil, http.StatusOK, "application/json", `"linked_devices"`)

			body := bytes.NewBufferString(`{"input":"mounted request","stream":true}`)
			response := assertPeerResponse(t, client, http.MethodPost, peerWirePath(basePath, "/v1/responses"), body, http.StatusOK, "text/event-stream", "data:")
			if strings.Count(response, "event: response.output_text.delta") < 2 ||
				!strings.Contains(response, `"delta":"mounted "`) || !strings.Contains(response, `"delta":"response"`) ||
				!strings.Contains(response, "event: response.completed") {
				t.Fatalf("streamed response did not contain two deltas and durable completion: %s", response)
			}

			for _, denied := range []struct{ method, path string }{
				{http.MethodGet, "/v1/linked-devices"},
				{http.MethodGet, "/v1/sharing/capabilities"},
				{http.MethodPost, "/uploads"},
				{http.MethodPost, "/v1/push/subscribe"},
				{http.MethodGet, "/admin/widgets/status"},
			} {
				request, err := http.NewRequest(denied.method, "http://peer"+peerWirePath(basePath, denied.path), nil)
				if err != nil {
					t.Fatal(err)
				}
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != http.StatusForbidden || response.Header.Get(peer.PeerErrorHeader) != peer.PeerErrorScopeDenied {
					t.Errorf("%s %s = %d peer-error=%q, want scoped 403", denied.method, denied.path, response.StatusCode, response.Header.Get(peer.PeerErrorHeader))
				}
			}

			if basePath != "/" {
				encodedBase := fmt.Sprintf("/%%%02X%s", basePath[1], basePath[2:])
				for _, bypass := range []string{
					basePath + "ty/v1/sessions",
					encodedBase + "/v1/sessions",
					basePath + "/../v1/sessions",
					basePath + "/%2e%2e/v1/sessions",
					basePath + "%2fv1/sessions",
				} {
					response, err := client.Get("http://peer" + bypass)
					if err != nil {
						continue
					}
					response.Body.Close()
					if response.StatusCode == http.StatusOK {
						t.Errorf("bypass %q unexpectedly reached mounted serve handler", bypass)
					}
				}
			}

			stream, err := client.Get("http://peer" + peerWirePath(basePath, "/v1/events"))
			if err != nil {
				t.Fatal(err)
			}
			if stream.StatusCode != http.StatusOK || !strings.Contains(stream.Header.Get("Content-Type"), "text/event-stream") {
				t.Fatalf("event stream = %d %q", stream.StatusCode, stream.Header.Get("Content-Type"))
			}
			drained := make(chan struct{})
			go func() { closeFixture(); close(drained) }()
			select {
			case <-drained:
			case <-time.After(2 * time.Second):
				t.Fatal("mobile runtime shutdown did not cancel and drain passive SSE")
			}
			_ = stream.Body.Close()
		})
	}
}

func mountedPeerServeFixture(t *testing.T, basePath string) (*http.Client, func()) {
	t.Helper()
	brokerHandler, err := peerbroker.NewHandler(peerbroker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	broker := httptest.NewServer(brokerHandler)

	// Keep this protocol fixture local and deterministic, independent of host
	// network interfaces and external ICE connectivity.
	settings := &webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settings.SetInterfaceFilter(func(name string) bool { return name == "lo" || name == "lo0" })
	endpoint := peer.EndpointConfig{SettingEngine: settings}

	runtime, err := linkeddevices.New(linkeddevices.Options{
		DataDir: t.TempDir(), Target: "mounted-test", StoreInstanceID: "mounted-store",
		BasePath: basePath, Authenticated: true, AgentName: "test", HostLabel: "Mounted test", BrokerURL: broker.URL, Endpoint: endpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := newTestServeServer("mounted response")
	server.cfg = serveServerConfig{ui: true, basePath: basePath, requireAuth: true, token: "owner-token"}
	server.store = newSessionDirectoryTestStore(t)
	server.linkedDevices = &linkeddevices.ServeHost{Runtime: runtime}
	_ = server.httpHandler()

	owner := httptest.NewServer(runtime.Handler())
	response, err := http.Post(owner.URL+"/v1/linked-devices/invitations", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var status linkeddevices.Status
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || status.Invitation == nil {
		t.Fatalf("create invitation = %d %+v", response.StatusCode, status)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	sas := make(chan string, 1)
	device, err := testdevice.New(testdevice.Config{
		DeviceName: "Mounted peer", StatePath: t.TempDir() + "/device.json", Endpoint: endpoint,
		OnSAS: func(code string) { sas <- code },
	})
	if err != nil {
		t.Fatal(err)
	}
	paired := make(chan error, 1)
	go func() { _, pairErr := device.Pair(ctx, status.Invitation.URL); paired <- pairErr }()
	var code string
	select {
	case code = <-sas:
	case <-ctx.Done():
		t.Fatal("synthetic device did not report SAS")
	}
	var pending string
	for pending == "" {
		select {
		case <-ctx.Done():
			t.Fatal("pairing request did not arrive")
		case <-time.After(10 * time.Millisecond):
			current := runtime.Status()
			if len(current.PendingRequests) == 1 {
				pending = current.PendingRequests[0].ID
			}
		}
	}
	if err := runtime.Approve(ctx, pending, code); err != nil {
		t.Fatal(err)
	}
	if err := <-paired; err != nil {
		t.Fatal(err)
	}
	session, err := device.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := sync.OnceFunc(func() {
		owner.Close()
		_ = runtime.Close()
		cancel()
		_ = session.Close()
		server.sessionMgr.Close()
		broker.Close()
		brokerHandler.Close()
	})
	return session.HTTPClient(), cleanup
}

func peerWirePath(basePath, path string) string {
	if basePath == "/" {
		return path
	}
	if path == "/" {
		return basePath + "/"
	}
	return basePath + path
}

func assertPeerResponse(t *testing.T, client *http.Client, method, path string, body io.Reader, status int, contentType, contains string) string {
	t.Helper()
	request, err := http.NewRequest(method, "http://peer"+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status || !strings.Contains(response.Header.Get("Content-Type"), contentType) || contains != "" && !strings.Contains(string(data), contains) {
		t.Fatalf("%s %s = %d content-type=%q body=%q", method, path, response.StatusCode, response.Header.Get("Content-Type"), data)
	}
	return string(data)
}

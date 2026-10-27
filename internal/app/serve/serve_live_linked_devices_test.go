package serve

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/linkeddevices"
	"github.com/samsaffron/term-llm/internal/live"
)

type linkedLiveTestProvider struct {
	session  live.Session
	readyErr error
	starts   int
	start    func(context.Context) (live.Session, error)
}

func (*linkedLiveTestProvider) Name() string                  { return "fake" }
func (p *linkedLiveTestProvider) Ready(context.Context) error { return p.readyErr }
func (p *linkedLiveTestProvider) Start(ctx context.Context, _ string, _ live.SessionOptions) (live.Session, error) {
	p.starts++
	if p.start != nil {
		return p.start(ctx)
	}
	return p.session, nil
}

func newLinkedLiveTestServer(t *testing.T, providerName string) (*serveServer, *stubPCMLiveSession, *linkedLiveTestProvider) {
	t.Helper()
	srv := newTestServeServer()
	srv.cfg.ui = true
	srv.cfg.requireAuth = true
	srv.cfg.token = "test-owner-token"
	srv.cfg.basePath = "/chat"
	srv.shutdownCh = make(chan struct{})
	srv.cfgRef = &config.Config{Live: config.LiveConfig{Enabled: true, Provider: providerName}}
	pcm := newStubPCMLiveSession()
	provider := &linkedLiveTestProvider{session: pcm}
	if providerName != config.LiveProviderGemini {
		// Expose only the real WebRTC session interface, not fake PCM support.
		provider.session = struct{ live.Session }{pcm}
	}
	srv.liveProviderFactory = func(config.LiveConfig) (live.Provider, error) { return provider, nil }
	t.Cleanup(func() {
		srv.closeLiveSessions(context.Background())
		srv.sessionMgr.Close()
	})
	return srv, pcm, provider
}

func linkedLiveRequest(method, path, body, device string, lifetime context.Context) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	return linkeddevices.WithDevicePrincipal(r, linkeddevices.DevicePrincipal{DeviceID: device, Scope: linkeddevices.ScopeWebV1, Lifetime: lifetime})
}

func TestLinkedLiveStartRejectsDeviceAuthorityAndUnsupportedTransport(t *testing.T) {
	cases := []struct {
		name, provider, body string
		lifetime             context.Context
		want                 int
	}{
		{"client delegation", config.LiveProviderGemini, `{"audio_transport":"http_pcm","delegation_mode":"client"}`, context.Background(), http.StatusBadRequest},
		{"context", config.LiveProviderGemini, `{"audio_transport":"http_pcm","delegation_context":"phone tools"}`, context.Background(), http.StatusBadRequest},
		{"blank context", config.LiveProviderGemini, `{"audio_transport":"http_pcm","delegation_context":" "}`, context.Background(), http.StatusBadRequest},
		{"client tools", config.LiveProviderGemini, `{"audio_transport":"http_pcm","client_tools":[{"type":"function","name":"device_shell","parameters":{"type":"object"}}]}`, context.Background(), http.StatusBadRequest},
		{"invalid tools", config.LiveProviderGemini, `{"audio_transport":"http_pcm","client_tools":[null]}`, context.Background(), http.StatusBadRequest},
		{"websocket", config.LiveProviderGemini, `{"audio_transport":"websocket_pcm"}`, context.Background(), http.StatusBadRequest},
		{"implicit websocket", config.LiveProviderGemini, `{}`, context.Background(), http.StatusBadRequest},
		{"wrong Gemini transport", config.LiveProviderGemini, `{"audio_transport":"webrtc"}`, context.Background(), http.StatusBadRequest},
		{"wrong WebRTC transport", config.LiveProviderOpenAI, `{"audio_transport":"http_pcm"}`, context.Background(), http.StatusBadRequest},
		{"missing lifetime", config.LiveProviderGemini, `{"audio_transport":"http_pcm"}`, nil, http.StatusForbidden},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			srv, _, provider := newLinkedLiveTestServer(t, test.provider)
			response := httptest.NewRecorder()
			srv.handleLiveSessions(response, linkedLiveRequest(http.MethodPost, "/v1/live/sessions", test.body, "device-a", test.lifetime))
			if response.Code != test.want || provider.starts != 0 || len(srv.liveSessions) != 0 {
				t.Fatalf("start = %d %s, provider starts = %d", response.Code, response.Body.String(), provider.starts)
			}
		})
	}
}

func TestLinkedLiveWebRTCStartPreservesProviderPath(t *testing.T) {
	for _, transport := range []string{"", "webrtc"} {
		t.Run(transport, func(t *testing.T) {
			srv, pcm, provider := newLinkedLiveTestServer(t, config.LiveProviderOpenAI)
			lifetime, revoke := context.WithCancel(context.Background())
			defer revoke()
			body, _ := json.Marshal(liveStartRequest{SessionID: "linked-rtc", SDP: browserOffer, AudioTransport: transport})
			response := httptest.NewRecorder()
			srv.handleLiveSessions(response, linkedLiveRequest(http.MethodPost, "/v1/live/sessions", string(body), "device-a", lifetime))
			var started map[string]any
			_ = json.Unmarshal(response.Body.Bytes(), &started)
			if response.Code != http.StatusOK || started["transport"] != "webrtc" || started["delegation_mode"] != "server" || provider.starts != 1 {
				t.Fatalf("start = %d %s", response.Code, response.Body.String())
			}
			revoke()
			select {
			case <-pcm.closed:
			case <-time.After(2 * time.Second):
				t.Fatal("peer revocation did not stop WebRTC provider")
			}
		})
	}
}

func TestLinkedLiveCapabilityReflectsReadinessAndTransport(t *testing.T) {
	for _, providerName := range []string{config.LiveProviderGemini, config.LiveProviderOpenAI} {
		for _, state := range []string{"ready", "disabled", "unavailable"} {
			t.Run(providerName+"/"+state, func(t *testing.T) {
				srv, _, provider := newLinkedLiveTestServer(t, providerName)
				if state == "disabled" {
					srv.cfgRef.Live.Enabled = false
				}
				if state == "unavailable" {
					provider.readyErr = errors.New("fake unavailable")
				}
				owner := srv.liveCapability(context.Background())
				response := httptest.NewRecorder()
				srv.handleCapabilities(response, linkedLiveRequest(http.MethodGet, "/v1/capabilities", "", "device-a", context.Background()))
				var payload map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				caps := payload["live"].(map[string]any)
				linked := payload["linked_devices"].(map[string]any)
				if caps["enabled"] != (state == "ready") || linked["voice"] != caps["enabled"] || caps["transport"] != owner["transport"] || caps["provider"] != owner["provider"] || !reflect.DeepEqual(caps["delegation_modes"], []any{"server"}) {
					t.Fatalf("linked = %v, live = %v, owner = %v", linked, caps, owner)
				}
			})
		}
	}
}

func TestLinkedLiveHandlerBoundariesDenyOwnerAndOtherDeviceCalls(t *testing.T) {
	srv, _, _ := newLinkedLiveTestServer(t, config.LiveProviderGemini)
	for _, owner := range []string{"", "device-b"} {
		record := newLiveSession("live_"+owner+"call", "chat-"+owner)
		if owner != "" {
			record.linkedOwner = linkeddevices.NewLiveOwner(linkedLiveRequest(http.MethodPost, "/v1/live/sessions", "", owner, context.Background()))
		}
		if err := srv.registerLiveSession(record); err != nil {
			t.Fatal(err)
		}
		for path, method := range map[string]string{
			"": http.MethodDelete, "/events": http.MethodGet, "/audio/output": http.MethodGet,
			"/audio/input": http.MethodPost, "/diagnostics": http.MethodPost,
			"/text": http.MethodPost, "/session": http.MethodPost,
		} {
			response := httptest.NewRecorder()
			srv.httpHandler().ServeHTTP(response, linkedLiveRequest(method, "/chat/v1/live/sessions/"+record.id+path, `{}`, "device-a", context.Background()))
			if response.Code != http.StatusNotFound {
				t.Fatalf("other owner %q %s = %d %s", owner, path, response.Code, response.Body.String())
			}
		}
		if _, ok := srv.lookupLiveSession(record.id); !ok {
			t.Fatal("foreign device stopped the call")
		}
		// Ordinary owner authentication and control remain unchanged.
		r := httptest.NewRequest(http.MethodDelete, "/chat/v1/live/sessions/"+record.id, nil)
		r.Header.Set("Authorization", "Bearer test-owner-token")
		response := httptest.NewRecorder()
		srv.httpHandler().ServeHTTP(response, r)
		if response.Code != http.StatusOK {
			t.Fatalf("owner stop = %d %s", response.Code, response.Body.String())
		}
	}
}

func TestLinkedLiveHandlerBoundariesDenyToolsWebSocketAndAliases(t *testing.T) {
	srv, _, _ := newLinkedLiveTestServer(t, config.LiveProviderGemini)
	for _, path := range []string{
		"/live_own/audio", "/live_own/client_tools", "/live_own/delegations/d1/result", "/live_own/tool_calls/t1/result",
		"/live_own/text/", "/live_own/extra/text", "/live_own/events/", "/live_own/", "/live_own/audio/output/extra",
		"/live_bad.id/text", "/live_%6fwn/text",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			response := httptest.NewRecorder()
			srv.httpHandler().ServeHTTP(response, linkedLiveRequest(method, "/chat/v1/live/sessions"+path, `{}`, "device-a", context.Background()))
			if response.Code != http.StatusForbidden {
				t.Errorf("%s %s = %d %s", method, path, response.Code, response.Body.String())
			}
		}
	}
}

func TestLinkedLiveHTTPPCMLifecycle(t *testing.T) {
	for _, end := range []string{"stop", "peer-revocation", "output-disconnect"} {
		t.Run(end, func(t *testing.T) {
			srv, pcm, _ := newLinkedLiveTestServer(t, config.LiveProviderGemini)
			lifetime, revoke := context.WithCancel(context.Background())
			defer revoke()
			handler := srv.httpHandler()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler.ServeHTTP(w, linkeddevices.WithDevicePrincipal(r, linkeddevices.DevicePrincipal{DeviceID: "device-a", Scope: linkeddevices.ScopeWebV1, Lifetime: lifetime}))
			}))
			defer ts.Close()
			client := ts.Client()
			client.Timeout = 5 * time.Second
			start, err := client.Post(ts.URL+"/chat/v1/live/sessions", "application/json", strings.NewReader(`{"session_id":"linked-pcm","audio_transport":"http_pcm","client_tools":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			var started struct {
				ID              string `json:"live_id"`
				Transport       string `json:"transport"`
				AudioCapability string `json:"audio_capability"`
				DelegationMode  string `json:"delegation_mode"`
			}
			err = json.NewDecoder(start.Body).Decode(&started)
			start.Body.Close()
			if err != nil || start.StatusCode != http.StatusOK || started.Transport != "http_pcm" || started.AudioCapability == "" || started.DelegationMode != "server" {
				t.Fatalf("start = %d %+v, err=%v", start.StatusCode, started, err)
			}
			base := ts.URL + "/chat/v1/live/sessions/" + started.ID
			post := func(path, body string, want int) {
				t.Helper()
				response, err := client.Post(base+path, "application/octet-stream", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				if response.StatusCode != want {
					data, _ := io.ReadAll(response.Body)
					t.Fatalf("POST %s = %d %s", path, response.StatusCode, data)
				}
			}
			post("/audio/input", "\x01\x00", http.StatusConflict)
			// Query parameters do not substitute for the one-use header capability.
			bad, err := client.Get(base + "/audio/output?token=" + started.AudioCapability)
			if err != nil {
				t.Fatal(err)
			}
			bad.Body.Close()
			if bad.StatusCode != http.StatusUnauthorized {
				t.Fatalf("query capability status = %d", bad.StatusCode)
			}
			r, _ := http.NewRequest(http.MethodGet, base+"/audio/output", nil)
			r.Header.Set("X-Term-LLM-Live-Audio-Capability", started.AudioCapability)
			output, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer output.Body.Close()
			if output.StatusCode != http.StatusOK {
				t.Fatalf("output = %d", output.StatusCode)
			}
			scanner := bufio.NewScanner(output.Body)
			event, _, ok := readSSEEvent(t, scanner)
			if !ok || event != "ready" {
				t.Fatalf("first audio event = %q", event)
			}
			second, err := client.Do(r.Clone(context.Background()))
			if err != nil {
				t.Fatal(err)
			}
			second.Body.Close()
			if second.StatusCode != http.StatusUnauthorized {
				t.Fatalf("reused audio token = %d", second.StatusCode)
			}
			post("/audio/input", "\x01\x00\x02\x00", http.StatusNoContent)
			select {
			case input := <-pcm.received:
				if string(input) != "\x01\x00\x02\x00" {
					t.Fatalf("provider PCM = %v", input)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("provider did not receive PCM")
			}
			pcm.frames <- live.PCMFrame{Audio: []byte{3, 0, 4, 0}}
			event, data, ok := readSSEEvent(t, scanner)
			if !ok || event != "audio" {
				t.Fatalf("audio event = %q %s", event, data)
			}
			var frame struct {
				Audio string `json:"audio"`
			}
			if err := json.Unmarshal([]byte(data), &frame); err != nil {
				t.Fatal(err)
			}
			decoded, err := base64.StdEncoding.DecodeString(frame.Audio)
			if err != nil || string(decoded) != "\x03\x00\x04\x00" {
				t.Fatalf("output PCM = %v, err = %v", decoded, err)
			}
			events, err := client.Get(base + "/events")
			if err != nil {
				t.Fatal(err)
			}
			defer events.Body.Close()
			if events.StatusCode != http.StatusOK {
				t.Fatalf("events = %d", events.StatusCode)
			}
			record, _ := srv.lookupLiveSession(started.ID)
			record.appendEvent(liveEventTranscript, map[string]any{"text": "linked transcript"})
			eventScanner := bufio.NewScanner(events.Body)
			event, _, ok = readSSEEvent(t, eventScanner)
			if !ok || event != liveEventTranscript {
				t.Fatalf("session event = %q", event)
			}
			post("/text", `{"text":"hello"}`, http.StatusOK)
			if end == "peer-revocation" {
				revoke()
			} else if end == "output-disconnect" {
				output.Body.Close()
			} else {
				stop, _ := http.NewRequest(http.MethodDelete, base, nil)
				response, err := client.Do(stop)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != http.StatusOK {
					t.Fatalf("stop = %d", response.StatusCode)
				}
			}
			select {
			case <-pcm.closed:
			case <-time.After(2 * time.Second):
				t.Fatal("call end did not close provider")
			}
			if _, ok := srv.lookupLiveSession(started.ID); ok {
				t.Fatal("call still registered after stop")
			}
		})
	}
}

func TestLinkedLiveRevocationCancelsProviderStartup(t *testing.T) {
	srv, _, provider := newLinkedLiveTestServer(t, config.LiveProviderGemini)
	lifetime, revoke := context.WithCancel(context.Background())
	defer revoke()
	started := make(chan struct{})
	provider.start = func(ctx context.Context) (live.Session, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		srv.handleLiveSessions(response, linkedLiveRequest(http.MethodPost, "/v1/live/sessions", `{"audio_transport":"http_pcm"}`, "device-a", lifetime))
		finished <- response
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider startup did not begin")
	}
	revoke()
	select {
	case response := <-finished:
		if response.Code != http.StatusBadGateway || len(srv.liveSessions) != 0 {
			t.Fatalf("cancelled startup = %d %s, sessions=%d", response.Code, response.Body.String(), len(srv.liveSessions))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revocation did not promptly cancel provider startup")
	}
}

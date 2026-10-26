package serve

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/live"
	"github.com/samsaffron/term-llm/internal/restart"
)

func drainingProducerCoordinator(t *testing.T) *restart.Coordinator {
	t.Helper()
	old := restart.Default
	c := &restart.Coordinator{InterruptAfter: time.Hour}
	restart.Default = c
	t.Cleanup(func() { restart.Default = old })
	_, release, err := c.Root(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	stop, err := c.Bind(t.Context(), func(context.Context) error {
		t.Error("replacement ran while the test still owned a Root")
		return errors.New("unexpected replacement")
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	c.Request()
	if !c.Draining() {
		t.Fatal("coordinator did not begin draining")
	}
	return c
}

type producerGateBody struct{ reads int }

func (b *producerGateBody) Read([]byte) (int, error) {
	b.reads++
	return 0, io.EOF
}
func (*producerGateBody) Close() error { return nil }

func TestServeProducerGatesRejectDuringDrain(t *testing.T) {
	c := drainingProducerCoordinator(t)
	for _, tc := range []struct {
		name, method, path, body string
		handler                  func(*serveServer, http.ResponseWriter, *http.Request)
	}{
		{"chat", "POST", "/v1/chat/completions", `{"messages":[{"role":"user","content":"hello"}]}`, (*serveServer).handleChatCompletions},
		{"chat stream", "POST", "/v1/chat/completions", `{"stream":true,"messages":[{"role":"user","content":"hello"}]}`, (*serveServer).handleChatCompletions},
		{"messages", "POST", "/v1/messages", `{"messages":[{"role":"user","content":"hello"}]}`, (*serveServer).handleAnthropicMessages},
		{"messages stream", "POST", "/v1/messages", `{"stream":true,"messages":[{"role":"user","content":"hello"}]}`, (*serveServer).handleAnthropicMessages},
		{"responses nonstream", "POST", "/v1/responses", `{"input":"hello","stream":false}`, (*serveServer).handleResponses},
		{"Gemini live start", "POST", "/v1/live/sessions", `{"session_id":"gate-chat"}`, (*serveServer).handleLiveSessions},
		{"audio attach", "GET", "/v1/live/sessions/live_gate/audio", "", (*serveServer).handleLiveSessionAudioRoute},
		{"worktree create", "POST", "/v1/worktrees", `{"name":"gate"}`, (*serveServer).handleWorktrees},
		{"worktree clean", "POST", "/v1/worktrees", `{"name":"gate","clean":true}`, (*serveServer).handleWorktrees},
		{"transcribe", "POST", "/v1/audio/transcriptions", "", (*serveServer).handleTranscribe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServeServer()
			t.Cleanup(srv.sessionMgr.Close)
			srv.cfg.ui = true
			srv.cfgRef = &config.Config{Live: config.LiveConfig{Enabled: true, Provider: config.LiveProviderGemini}}
			srv.runtimeFactory = func(context.Context, serveRuntimeRequest) (*serveRuntime, error) {
				t.Error("runtime created during drain")
				return nil, errors.New("unexpected runtime")
			}
			srv.liveProviderFactory = func(config.LiveConfig) (live.Provider, error) {
				t.Error("live provider created during drain")
				return nil, errors.New("unexpected provider")
			}
			srv.worktreeRootFn = func() (string, error) {
				t.Error("worktree preparation started during drain")
				return "", errors.New("unexpected worktree")
			}
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			// Model a control request admitted by the transport during drain.
			ctx, release, err := c.Activity(req.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			req = req.WithContext(ctx)
			if tc.name == "audio attach" {
				record := newLiveSession("live_gate", "gate-chat")
				record.pcmSession = newStubPCMLiveSession()
				if err := srv.registerLiveSession(record); err != nil {
					t.Fatal(err)
				}
				token, err := record.issueAudioToken(time.Now(), time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				req.SetPathValue("liveID", record.id)
				req.URL.RawQuery = "token=" + token
				req.Header.Set("Sec-Fetch-Site", "same-origin")
				t.Cleanup(func() {
					if _, ok := record.claimAudio(token, time.Now()); !ok {
						t.Error("draining attach consumed the audio capability")
					}
					record.releaseAudio()
				})
			}
			if tc.name == "transcribe" {
				body := &producerGateBody{}
				req.Body = body
				req.Header.Set("Content-Type", "multipart/form-data; boundary=gate")
				t.Cleanup(func() {
					if body.reads != 0 || req.MultipartForm != nil {
						t.Error("transcription parsed the upload before admission")
					}
				})
			}
			rec := httptest.NewRecorder()
			tc.handler(srv, rec, req)
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"server_reloading"`) || rec.Header().Get("Retry-After") != "1" {
				t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
			}
			if tc.name == "messages" && !strings.Contains(rec.Body.String(), `"type":"error"`) {
				t.Fatalf("lost Anthropic error envelope: %s", rec.Body.String())
			}
		})
	}
}

func TestProducerGatePreservesTransportOwnership(t *testing.T) {
	old := restart.Default
	c := &restart.Coordinator{}
	restart.Default = c
	defer func() { restart.Default = old }()
	caller, cancel := context.WithCancel(t.Context())
	defer cancel()
	var owned context.Context
	(&restart.HTTPTransport{Coordinator: c}).Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowServeProducer(w, r) {
			t.Fatal("producer rejected while ready")
		}
		owned = r.Context()
		cancel()
		if !errors.Is(owned.Err(), context.Canceled) {
			t.Fatalf("caller cancellation lost: %v", owned.Err())
		}
		_, cleanup, err := c.Enter(owned)
		if err != nil {
			t.Fatalf("cancelled handler lost cleanup ownership: %v", err)
		}
		cleanup()
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil).WithContext(caller))
	if _, cleanup, err := c.Enter(owned); !errors.Is(err, restart.ErrReleased) {
		if cleanup != nil {
			cleanup()
		}
		t.Fatalf("returned handler still owns work: %v", err)
	}
}

func TestHubDelegationProducerGateRejectsDuringDrain(t *testing.T) {
	drainingProducerCoordinator(t)
	srv, target := newDelegationHub(t)
	rec, _ := createDelegation(t, srv, "alpha", "alpha-token", map[string]any{
		"target_node": "beta", "prompt": "do not create work", "model": "haiku",
	})
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), `"server_reloading"`) {
		t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	records, err := srv.delegations.List()
	if err != nil || len(records) != 0 {
		t.Fatalf("delegation ledger changed during drain: records=%v err=%v", records, err)
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if len(target.jobs) != 0 || len(target.jobRuns) != 0 {
		t.Fatalf("target received new work: jobs=%v runs=%v", target.jobs, target.jobRuns)
	}
}

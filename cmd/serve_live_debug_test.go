package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/live"
)

func TestLiveDebugEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, value                 string
		flag, rawFlag, enabled, raw bool
	}{
		{name: "default"},
		{name: "numeric", value: "1", enabled: true},
		{name: "boolean", value: "true", enabled: true},
		{name: "on", value: "on", enabled: true},
		{name: "metadata", value: "metadata", enabled: true},
		{name: "raw", value: "raw", enabled: true, raw: true},
		{name: "normalized", value: " RAW ", enabled: true, raw: true},
		{name: "zero", value: "0"},
		{name: "false", value: "false"},
		{name: "off", value: "off"},
		{name: "invalid", value: "verbose"},
		{name: "debug flag", value: "off", flag: true, enabled: true},
		{name: "raw flag", value: "metadata", rawFlag: true, enabled: true, raw: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM_LLM_LIVE_DEBUG", tc.value)
			s := &serveServer{cfg: serveServerConfig{debug: tc.flag, debugRaw: tc.rawFlag}}
			opts := s.liveSessionOptions(context.Background(), "test", live.Capabilities{})
			if opts.Debug != tc.enabled || opts.DebugRaw != tc.raw {
				t.Fatalf("debug/raw=%t/%t, want %t/%t", opts.Debug, opts.DebugRaw, tc.enabled, tc.raw)
			}
			if s.cfg.debug != tc.flag || s.cfg.debugRaw != tc.rawFlag {
				t.Fatal("live environment changed global debugging")
			}
			rr := httptest.NewRecorder()
			s.handleLiveSessionDiagnostics(rr, httptest.NewRequest(http.MethodGet, "/diagnostics", nil), "missing")
			want := http.StatusNotFound
			if tc.enabled {
				want = http.StatusMethodNotAllowed
			}
			if rr.Code != want {
				t.Fatalf("diagnostic gate status=%d want=%d", rr.Code, want)
			}
		})
	}
}

func TestLiveDebugEnvironmentEnablesBrowserDiagnostics(t *testing.T) {
	t.Setenv("TERM_LLM_LIVE_DEBUG", "raw")
	s := newTestServeServer()
	s.cfg.ui = true
	s.shutdownCh = make(chan struct{})
	s.cfgRef = &config.Config{Live: config.LiveConfig{Enabled: true, Provider: config.LiveProviderGemini}}
	provider := &stubPCMProvider{session: newStubPCMLiveSession(), options: make(chan live.SessionOptions, 1)}
	s.liveProviderFactory = func(config.LiveConfig) (live.Provider, error) { return provider, nil }
	t.Cleanup(func() { s.closeLiveSessions(context.Background()) })
	rr := httptest.NewRecorder()
	s.handleLiveSessions(rr, httptest.NewRequest(http.MethodPost, "/v1/live/sessions", strings.NewReader(`{"session_id":"env-debug","audio_transport":"http_pcm"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("start status=%d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Diagnostics bool `json:"diagnostics"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Diagnostics {
		t.Fatal("environment did not enable browser reports")
	}
	opts := <-provider.options
	if !opts.Debug || !opts.DebugRaw {
		t.Fatalf("provider diagnostics=%t/%t", opts.Debug, opts.DebugRaw)
	}
}

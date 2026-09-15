package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/samsaffron/term-llm/internal/credentials"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/livecall"
)

const liveTestSDP = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\n"

func TestLiveCallsHTTP(t *testing.T) {
	valid, _ := json.Marshal(livecall.Offer{SDP: liveTestSDP})
	cases := []struct {
		name, method, body, contentType, auth string
		enabled, requireAuth                  bool
		status                                int
	}{
		{"unauthenticated", "POST", string(valid), "application/json", "", true, true, 401},
		{"disabled", "POST", string(valid), "application/json", "Bearer test", false, true, 503},
		{"no auth mode", "POST", string(valid), "application/json", "", true, false, 503},
		{"method", "GET", "", "", "Bearer test", true, true, 405},
		{"content type", "POST", string(valid), "text/plain", "Bearer test", true, true, 415},
		{"oversize", "POST", strings.Repeat("x", livecall.MaxBody+1), "application/json", "Bearer test", true, true, 413},
		{"unknown", "POST", `{"sdp":"v=0","model":"other"}`, "application/json", "Bearer test", true, true, 400},
		{"invalid offer", "POST", `{"sdp":"not sdp"}`, "application/json", "Bearer test", true, true, 400},
		{"unavailable executable", "POST", string(valid), "application/json", "Bearer test", true, true, 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &serveServer{cfg: serveServerConfig{requireAuth: tc.requireAuth, token: "test"}}
			if tc.enabled {
				s.cfg.liveCodexPath = "/no/such/codex"
			}
			req := httptest.NewRequest(tc.method, "/v1/live/calls", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("Authorization", tc.auth)
			rr := httptest.NewRecorder()
			s.auth(s.cors(s.handleLiveCalls))(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status %d want %d: %s", rr.Code, tc.status, rr.Body.String())
			}
			if s.liveCalls != nil {
				s.liveCalls.Close()
			}
		})
	}
}
func TestLiveOfferStrictJSON(t *testing.T) {
	valid, _ := json.Marshal(livecall.Offer{SDP: liveTestSDP, InitialItems: []livecall.Item{{Role: "user", Text: "hello"}}})
	cases := [][]byte{bytes.Replace(valid, []byte(`"sdp"`), []byte(`"SDP"`), 1), []byte(`null`), []byte(`[]`), []byte(`{"sdp":null}`), []byte(`{"sdp":"a","sdp":"b"}`), append(bytes.Clone(valid), []byte(` {}`)...), bytes.Replace(valid, []byte(`"role":"user"`), []byte(`"role":"user","role":"assistant"`), 1), bytes.Replace(valid, []byte(`"text":"hello"`), []byte(`"text":"hello","tool":"shell"`), 1), append([]byte{'"'}, 0xff, '"')}
	for _, body := range cases {
		var offer livecall.Offer
		if decodeLiveOffer(body, &offer) == nil {
			t.Errorf("accepted %q", body)
		}
	}
	var offer livecall.Offer
	if err := decodeLiveOffer(valid, &offer); err != nil {
		t.Fatal(err)
	}
}
func TestLiveCallsRegistered(t *testing.T) {
	s := newTestServeServer()
	s.cfg.requireAuth = true
	s.cfg.token = "test"
	req := httptest.NewRequest(http.MethodPost, s.cfg.basePath+"/v1/live/calls", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test")
	rr := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(rr, req)
	if rr.Code != 503 {
		t.Fatalf("route status %d: %s", rr.Code, rr.Body.String())
	}
}

func TestLiveChatGPTTokenSource(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := liveChatGPTTokens(context.Background(), false); !errors.Is(err, livecall.ErrUnavailable) {
		t.Fatalf("missing credentials: %v", err)
	}
	creds := &credentials.ChatGPTCredentials{AccessToken: "existing-token", AccountID: "existing-account", RefreshToken: "must-not-copy", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if err := credentials.SaveChatGPTCredentials(creds); err != nil {
		t.Fatal(err)
	}
	tokens, err := liveChatGPTTokens(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != creds.AccessToken || tokens.AccountID != creds.AccountID {
		t.Fatal("did not use configured native OAuth")
	}
	encoded, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(creds.RefreshToken)) {
		t.Fatal("refresh secret exposed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := liveChatGPTTokens(ctx, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

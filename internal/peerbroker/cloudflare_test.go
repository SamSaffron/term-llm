package peerbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testKeyID = "abcdef0123456789"
	testToken = "cf-api-token-SECRET"
)

const cfDocResponse = `{"iceServers":[{"urls":["stun:stun.cloudflare.com:3478"]},
{"urls":["turn:turn.cloudflare.com:3478?transport=udp","turns:turn.cloudflare.com:443?transport=tcp"],
"username":"user123","credential":"cred456"}]}`

func newCFMinter(t *testing.T, h http.HandlerFunc) *CloudflareTURNMinter {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	m, err := NewCloudflareTURNMinter(CloudflareTURNConfig{KeyID: testKeyID, APIToken: testToken, BaseURL: srv.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCloudflareMintSuccess(t *testing.T) {
	m := newCFMinter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/turn/keys/"+testKeyID+"/credentials/generate-ice-servers" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["ttl"] != float64(600) || len(body) != 1 {
			t.Errorf("body = %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, cfDocResponse)
	})
	creds, err := m.MintICEServers(context.Background(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds.ICEServers) != 2 || creds.ICEServers[1].Username != "user123" || creds.ICEServers[1].Credential != "cred456" {
		t.Fatalf("creds = %+v", creds)
	}
	if d := time.Until(creds.ExpiresAt); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("expires in %s", d)
	}
}

func TestCloudflareLegacyObjectShape(t *testing.T) {
	m := newCFMinter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"iceServers":{"urls":["turn:turn.cloudflare.com:3478?transport=udp"],"username":"u","credential":"c"}}`)
	})
	creds, err := m.MintICEServers(context.Background(), 5*time.Minute)
	if err != nil || len(creds.ICEServers) != 1 {
		t.Fatalf("creds=%+v err=%v", creds, err)
	}
}

func TestCloudflareTTLBounds(t *testing.T) {
	called := false
	m := newCFMinter(t, func(http.ResponseWriter, *http.Request) { called = true })
	for _, ttl := range []time.Duration{0, 4 * time.Minute, 31 * time.Minute, 24 * time.Hour} {
		if _, err := m.MintICEServers(context.Background(), ttl); err == nil {
			t.Errorf("ttl %s accepted", ttl)
		}
	}
	if called {
		t.Fatal("upstream called for invalid TTL")
	}
}

func TestCloudflareErrorsRedacted(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"http error echoing token": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"error":"bad token %s for key %s"}`, testToken, testKeyID)
		},
		"redirect not followed": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://evil.example/steal", http.StatusTemporaryRedirect)
		},
		"oversized": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"iceServers":[`+strings.Repeat(" ", cfMaxResponseBytes)+`]}`)
		},
		"malformed": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"iceServers":[]}`)
		},
		"turn without creds": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"iceServers":[{"urls":["turn:x:3478"]}]}`)
		},
		"bad scheme": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"iceServers":[{"urls":["http://x"],"username":"u","credential":"c"}]}`)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			m := newCFMinter(t, h)
			_, err := m.MintICEServers(context.Background(), 10*time.Minute)
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), testKeyID) {
				t.Fatalf("error leaks secret: %v", err)
			}
		})
	}
}

func TestCloudflareTimeout(t *testing.T) {
	// Cleanups run LIFO: unblock the handler before srv.Close waits for it.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })
	m, err := NewCloudflareTURNMinter(CloudflareTURNConfig{KeyID: testKeyID, APIToken: testToken, BaseURL: srv.URL, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.MintICEServers(context.Background(), 10*time.Minute)
	if err == nil || strings.Contains(err.Error(), testKeyID) {
		t.Fatalf("got %v", err)
	}
}

func TestCloudflareConfigValidation(t *testing.T) {
	bad := []CloudflareTURNConfig{
		{KeyID: "", APIToken: "x"},
		{KeyID: "../etc", APIToken: "x"},
		{KeyID: "abc", APIToken: ""},
		{KeyID: "abc", APIToken: "a b"},
		{KeyID: "abc", APIToken: "x", BaseURL: "http://rtc.example"},
	}
	for _, cfg := range bad {
		if _, err := NewCloudflareTURNMinter(cfg); err == nil {
			t.Errorf("accepted %+v", cfg)
		}
	}
	m, err := NewCloudflareTURNMinter(CloudflareTURNConfig{KeyID: "abc", APIToken: testToken})
	if err != nil {
		t.Fatal(err)
	}
	if m.endpoint != "https://rtc.live.cloudflare.com/v1/turn/keys/abc/credentials/generate-ice-servers" {
		t.Fatalf("endpoint = %s", m.endpoint)
	}
	if s := fmt.Sprintf("%v %+v %#v %s", m, m, m, m); strings.Contains(s, testToken) {
		t.Fatalf("formatting leaks token: %s", s)
	}
}

func TestCloudflareMinterWithBroker(t *testing.T) {
	m := newCFMinter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, cfDocResponse)
	})
	_, c, _ := newTestBroker(t, Config{TURNMinter: m})
	s := mustCreate(t, c)
	creds, err := c.MintTURN(context.Background(), s.ID, s.DeviceToken)
	if err != nil || len(creds.ICEServers) != 2 {
		t.Fatalf("creds=%+v err=%v", creds, err)
	}
}

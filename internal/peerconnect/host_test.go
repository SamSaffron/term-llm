package peerconnect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/pairing"
	"github.com/samsaffron/term-llm/internal/peerbroker"
)

func TestHostPairingTTLLimits(t *testing.T) {
	token := strings.Repeat("t", 43)
	broker, err := peerbroker.NewHandler(peerbroker.Config{CreateToken: token, SessionTTL: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(broker)
	defer server.Close()
	defer broker.Close()
	base := func() Config {
		return Config{BrokerURL: server.URL, RegistrationToken: token, StateDir: filepath.Join(t.TempDir(), "peer"), Handler: http.NotFoundHandler(), Approver: pairing.ApproverFunc(func(context.Context, pairing.PairingPrompt) (bool, error) { return false, nil }), OnPairingLink: func(pairing.Link) error { return nil }}
	}
	for _, ttl := range []time.Duration{-time.Second, 30 * time.Second, 31 * time.Minute} {
		cfg := base()
		cfg.PairingTTL = ttl
		if _, err := NewHost(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "pairing TTL") {
			t.Fatalf("accepted TTL %s: %v", ttl, err)
		}
	}
	cfg := base()
	cfg.PairingTTL = 6 * time.Minute
	cfg.RendezvousTTL = 5 * time.Minute
	if _, err := NewHost(t.Context(), cfg); err == nil {
		t.Fatal("pairing outlives broker rendezvous")
	}
	for _, ttl := range []time.Duration{0, time.Minute, 20 * time.Minute, 30 * time.Minute} {
		cfg := base()
		cfg.PairingTTL = ttl
		before := time.Now()
		h, err := NewHost(t.Context(), cfg)
		if err != nil {
			t.Fatalf("valid TTL %s: %v", ttl, err)
		}
		want := ttl
		if want == 0 {
			want = 5 * time.Minute
		}
		got := h.link.ExpiresAt.Sub(before)
		if got < want-2*time.Second || got > want+2*time.Second {
			t.Errorf("TTL %s became %s", want, got)
		}
		if h.link.ExpiresAt.After(h.session.ExpiresAt) {
			t.Error("link outlives actual broker")
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

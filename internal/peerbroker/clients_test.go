package peerbroker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type forwardedTransport struct {
	base http.RoundTripper
	ip   string
}

func (t forwardedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Real-IP", t.ip)
	return t.base.RoundTrip(r)
}

func loopbackProxies() []netip.Prefix {
	return []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
}

func newForwardedClient(t *testing.T, srv *httptest.Server, ip string) *Client {
	t.Helper()
	httpClient := *srv.Client()
	httpClient.Transport = forwardedTransport{base: srv.Client().Transport, ip: ip}
	client, err := NewClient(srv.URL, &httpClient)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func postChallenge(t *testing.T, srv *httptest.Server, ip string) int {
	t.Helper()
	encoded := base64.RawURLEncoding.EncodeToString(mustKey(t).Public().(ed25519.PublicKey))
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v2/challenges", bytes.NewBufferString(`{"public_key":"`+encoded+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if ip != "" {
		req.Header.Set("X-Real-IP", ip)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestClientPolicyRateLimitsOneNetworkWithoutStarvingOthers(t *testing.T) {
	_, _, srv := newBrokerClient(t, Config{Clients: &ClientPolicy{RequestRate: 0.001, RequestBurst: 3, TrustedProxies: loopbackProxies()}})
	for i := range 3 {
		if status := postChallenge(t, srv, "198.51.100.7"); status != http.StatusCreated {
			t.Fatalf("request %d status = %d", i, status)
		}
	}
	if status := postChallenge(t, srv, "198.51.100.7"); status != http.StatusTooManyRequests {
		t.Fatalf("over-rate status = %d, want 429", status)
	}
	if status := postChallenge(t, srv, "203.0.113.9"); status != http.StatusCreated {
		t.Fatalf("independent client status = %d, want 201", status)
	}
}

func TestClientPolicyIgnoresForwardedHeaderFromUntrustedPeer(t *testing.T) {
	_, _, srv := newBrokerClient(t, Config{Clients: &ClientPolicy{RequestRate: 0.001, RequestBurst: 2}})
	for _, ip := range []string{"198.51.100.1", "198.51.100.2"} {
		if status := postChallenge(t, srv, ip); status != http.StatusCreated {
			t.Fatalf("status = %d", status)
		}
	}
	if status := postChallenge(t, srv, "198.51.100.3"); status != http.StatusTooManyRequests {
		t.Fatalf("spoofed X-Real-IP escaped the socket-address bucket: status %d", status)
	}
}

func TestClientPolicyBoundsChallengesAndHostLeasesPerNetwork(t *testing.T) {
	_, _, srv := newBrokerClient(t, Config{Clients: &ClientPolicy{MaxChallenges: 2, MaxHosts: 1, TrustedProxies: loopbackProxies()}})
	for range 2 {
		if status := postChallenge(t, srv, "198.51.100.7"); status != http.StatusCreated {
			t.Fatalf("challenge status = %d", status)
		}
	}
	if status := postChallenge(t, srv, "198.51.100.7"); status != http.StatusTooManyRequests {
		t.Fatalf("outstanding challenge cap status = %d, want 429", status)
	}

	a := newForwardedClient(t, srv, "203.0.113.1")
	first, second := mustKey(t), mustKey(t)
	mustRegister(t, a, first, "", nil)
	if _, err := a.RegisterHost(context.Background(), second, "", nil); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second host lease from one network error = %v, want quota", err)
	}
	// Re-registering an identity this network already holds is not growth,
	// and the consumed challenge was returned to the network's budget.
	mustRegister(t, a, first, "", nil)
	b := newForwardedClient(t, srv, "203.0.113.2")
	mustRegister(t, b, second, "", nil)
}

func TestClientPolicyKeysIPv6ByPrefix64(t *testing.T) {
	_, _, srv := newBrokerClient(t, Config{Clients: &ClientPolicy{RequestRate: 0.001, RequestBurst: 1, TrustedProxies: loopbackProxies()}})
	if status := postChallenge(t, srv, "2001:db8:1:2::1"); status != http.StatusCreated {
		t.Fatalf("status = %d", status)
	}
	if status := postChallenge(t, srv, "2001:db8:1:2:ffff::9"); status != http.StatusTooManyRequests {
		t.Fatalf("same /64 status = %d, want 429", status)
	}
	if status := postChallenge(t, srv, "2001:db8:1:3::1"); status != http.StatusCreated {
		t.Fatalf("different /64 status = %d", status)
	}
}

func TestClientPolicyBoundsSocketsPerNetwork(t *testing.T) {
	h, client, _ := newBrokerClient(t, Config{Clients: &ClientPolicy{MaxConnections: 1}})
	key := mustKey(t)
	deviceCap := strings.Repeat("d", MinCredentialLen)
	registration := mustRegister(t, client, key, "", []DeviceCapabilityHash{{DeviceID: "phone", Hash: HashCapability(deviceCap)}})
	hostConn, err := client.DialHost(context.Background(), registration.HostID, registration.LeaseCapability)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.DialDevice(context.Background(), registration.HostID, "phone", deviceCap); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second socket from one network error = %v, want quota", err)
	}
	_ = hostConn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		device, err := client.DialDevice(context.Background(), registration.HostID, "phone", deviceCap)
		if err == nil {
			_ = device.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket slot was not released after close: %v (%v)", err, h)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMaxConnectionLifetimeClosesWithRetryableCode(t *testing.T) {
	_, client, _ := newBrokerClient(t, Config{MaxConnectionLifetime: 50 * time.Millisecond})
	registration := mustRegister(t, client, mustKey(t), "", nil)
	conn, err := client.DialHost(context.Background(), registration.HostID, registration.LeaseCapability)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, err := conn.Recv()
		if err == nil {
			continue
		}
		var closeErr *CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != WSCloseBrokerClosed {
			t.Fatalf("lifetime close = %v, want %d", err, WSCloseBrokerClosed)
		}
		return
	}
}

func TestPublicServeOptionsValidation(t *testing.T) {
	base := ServeOptions{Host: "127.0.0.1", Port: 8091, MaxHosts: 10, Public: true}
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "--trusted-proxy") {
		t.Fatalf("public mode without proxy error = %v", err)
	}
	withProxy := base
	withProxy.TrustedProxies = []string{"127.0.0.1"}
	if err := withProxy.Validate(); err != nil {
		t.Fatal(err)
	}
	withAdmission := withProxy
	withAdmission.AdmissionFile = "/run/secrets/admission"
	if err := withAdmission.Validate(); err == nil {
		t.Fatal("public mode accepted a private admission credential")
	}
	badProxy := withProxy
	badProxy.TrustedProxies = []string{"not-an-ip"}
	if err := badProxy.Validate(); err == nil {
		t.Fatal("invalid trusted proxy accepted")
	}
	cfg := withProxy.publicConfig(Config{})
	if cfg.Clients == nil || cfg.MaxConnectionLifetime != DefaultPublicConnectionLifetime || cfg.AdmissionCredential != "" || cfg.TURNProvider != nil || cfg.Coturn != nil {
		t.Fatalf("public config = %v", cfg)
	}
}

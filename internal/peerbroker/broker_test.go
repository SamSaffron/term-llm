package peerbroker

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newBrokerClient(t *testing.T, cfg Config) (*Handler, *Client, *httptest.Server) {
	t.Helper()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	client, err := NewClient(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(); srv.Close() })
	return h, client, srv
}

func mustKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := GenerateHostKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}
func mustRegister(t *testing.T, client *Client, key ed25519.PrivateKey, admission string, devices []DeviceCapabilityHash) *Registration {
	t.Helper()
	registration, err := client.RegisterHost(context.Background(), key, admission, devices)
	if err != nil {
		t.Fatal(err)
	}
	return registration
}

func TestRegistrationAdmissionSignatureAndHashReplacement(t *testing.T) {
	admission := strings.Repeat("a", MinCredentialLen)
	h, client, _ := newBrokerClient(t, Config{AdmissionCredential: admission})
	key := mustKey(t)
	if _, err := client.RegisterHost(context.Background(), key, "wrong", nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong admission error = %v", err)
	}
	firstCap, secondCap := "device-secret-one", "device-secret-two"
	registration := mustRegister(t, client, key, admission, []DeviceCapabilityHash{{DeviceID: "phone", Hash: HashCapability(firstCap)}})
	if strings.Contains(fmt.Sprintf("%v %#v", registration, registration), registration.LeaseCapability) {
		t.Fatal("registration formatting leaked lease capability")
	}

	h.mu.Lock()
	stored := h.hosts[registration.HostID]
	if stored == nil || !capabilityEqual(stored.devices["phone"].capHash, firstCap) {
		t.Fatal("device capability hash not installed")
	}
	h.mu.Unlock()
	if _, err := client.Renew(context.Background(), registration.HostID, registration.LeaseCapability, []DeviceCapabilityHash{{DeviceID: "tablet", Hash: HashCapability(secondCap)}}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	_, oldExists := stored.devices["phone"]
	newDevice := stored.devices["tablet"]
	h.mu.Unlock()
	if oldExists || newDevice == nil || !capabilityEqual(newDevice.capHash, secondCap) {
		t.Fatal("renew did not atomically replace device hashes")
	}
}

func TestSignedRegistrationCoversDeviceHashesAndChallengeIsOneUse(t *testing.T) {
	_, client, _ := newBrokerClient(t, Config{})
	key := mustKey(t)
	publicKey := key.Public().(ed25519.PublicKey)
	encodedKey := base64.RawURLEncoding.EncodeToString(publicKey)
	var challenge Challenge
	if err := client.do(context.Background(), http.MethodPost, "/v2/challenges", "", challengeRequest{PublicKey: encodedKey}, http.StatusCreated, &challenge); err != nil {
		t.Fatal(err)
	}
	signedDevices := []DeviceCapabilityHash{{DeviceID: "phone", Hash: HashCapability("signed-secret")}}
	tamperedDevices := []DeviceCapabilityHash{{DeviceID: "phone", Hash: HashCapability("attacker-secret")}}
	signature := ed25519.Sign(key, registrationPayload(publicKey, challenge.ID, challenge.Nonce, signedDevices))
	request := registerRequest{PublicKey: encodedKey, ChallengeID: challenge.ID, Signature: base64.RawURLEncoding.EncodeToString(signature), Devices: tamperedDevices}
	var out Registration
	if err := client.do(context.Background(), http.MethodPost, "/v2/hosts", "", request, http.StatusCreated, &out); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("tampered request error = %v", err)
	}
	request.Devices = signedDevices
	if err := client.do(context.Background(), http.MethodPost, "/v2/hosts", "", request, http.StatusCreated, &out); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replayed challenge error = %v", err)
	}
}

func TestHostGeneratedInvitationStoredHashedAndOneUse(t *testing.T) {
	h, client, _ := newBrokerClient(t, Config{})
	registration := mustRegister(t, client, mustKey(t), "", nil)
	capability := "host-generated-invitation-secret"
	expires := time.Now().Add(time.Minute)
	slot, err := client.RegisterInvitation(context.Background(), registration.HostID, registration.LeaseCapability, "invite_1", HashCapability(capability), expires)
	if err != nil {
		t.Fatal(err)
	}
	if slot.InvitationID != "invite_1" {
		t.Fatalf("slot = %+v", slot)
	}
	h.mu.Lock()
	stored := h.hosts[registration.HostID].invitations["invite_1"]
	h.mu.Unlock()
	if stored == nil || !capabilityEqual(stored.capHash, capability) {
		t.Fatal("invitation hash not stored")
	}
	if strings.Contains(fmt.Sprintf("%+v", stored), capability) {
		t.Fatal("stored invitation leaked plaintext")
	}

	conn, err := client.DialInvitation(context.Background(), registration.HostID, "invite_1", capability)
	if err != nil {
		t.Fatal(err)
	}
	if conn.Ready.Role != PeerInvitation {
		t.Fatalf("ready = %+v", conn.Ready)
	}
	_ = conn.Close()
	if _, err := client.DialInvitation(context.Background(), registration.HostID, "invite_1", capability); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("invitation replay error = %v", err)
	}
}

func TestICEStaticAndCoturnREST(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	secret := strings.Repeat("s", MinCredentialLen)
	cfg, err := (Config{Now: func() time.Time { return now }, STUNURLs: []string{"stun:relay.example:3478"}, Coturn: &CoturnConfig{URLs: []string{"turn:relay.example:3478"}, SharedSecret: secret, TTL: 5 * time.Minute, Username: "mobile"}}).withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg}
	creds := h.mintICE("host", "phone")
	if len(creds.ICEServers) != 2 || creds.ICEServers[0].URLs[0] != "stun:relay.example:3478" {
		t.Fatalf("ICE servers = %+v", creds.ICEServers)
	}
	turn := creds.ICEServers[1]
	mac := hmac.New(sha1.New, []byte(secret))
	_, _ = mac.Write([]byte(turn.Username))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if turn.Credential != want || !strings.Contains(turn.Username, ":mobile:phone") {
		t.Fatalf("TURN credential = %+v", turn)
	}
}

func TestNoStoreAndTransportPolicy(t *testing.T) {
	_, _, srv := newBrokerClient(t, Config{})
	resp, err := http.Get(srv.URL + "/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if _, err := NewClient("http://example.com", nil); err == nil {
		t.Fatal("non-loopback HTTP accepted")
	}
	if err := (ServeOptions{Host: "0.0.0.0", Port: 8091, MaxHosts: 1}).Validate(); err == nil {
		t.Fatal("non-loopback listener without TLS accepted")
	}
	if err := (ServeOptions{Host: "127.0.0.1", Port: 8091, MaxHosts: 1}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSecretRedaction(t *testing.T) {
	secret := strings.Repeat("z", MinCredentialLen)
	cfg := Config{AdmissionCredential: secret, Coturn: &CoturnConfig{URLs: []string{"turn:relay.example"}, SharedSecret: secret}}
	if text := fmt.Sprintf("%v %#v", cfg, cfg); strings.Contains(text, secret) {
		t.Fatalf("config leaked secret: %s", text)
	}
	ice := ICECredentials{ICEServers: []ICEServer{{URLs: []string{"turn:relay.example"}, Username: "user", Credential: secret}}}
	if text := fmt.Sprintf("%v %#v", ice, ice); strings.Contains(text, secret) {
		t.Fatalf("ICE formatting leaked credential: %s", text)
	}
	if got := HashCapability("capability"); got == "capability" || len(got) != 43 {
		t.Fatalf("HashCapability = %q", got)
	}
}

func rawJSON(value any) json.RawMessage { data, _ := json.Marshal(value); return data }

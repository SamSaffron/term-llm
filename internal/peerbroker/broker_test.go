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
	signature := ed25519.Sign(key, registrationPayload(publicKey, challenge.ID, challenge.Nonce, signedDevices, nil))
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
	replacement, err := client.DialInvitation(context.Background(), registration.HostID, "invite_1", capability)
	if err != nil {
		t.Fatalf("authenticated duplicate invitation socket: %v", err)
	}
	_ = replacement.Close()
	if _, err := conn.Recv(); err == nil {
		t.Fatal("superseded invitation socket remained open")
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
	creds, minted, err := h.mintICELegacy(context.Background(), "host", PeerDevice, "phone")
	if err != nil || !minted {
		t.Fatalf("mint legacy ICE: minted=%v err=%v", minted, err)
	}
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

func assertBrokerCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != code {
		t.Fatalf("error = %v, want broker code %q", err, code)
	}
}

func TestPeerEndpointsDistinguishOfflineExpiryAndRevocation(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	h, client, _ := newBrokerClient(t, Config{LeaseTTL: time.Minute, Now: func() time.Time { return now }})
	key := mustKey(t)
	hostID := hostIDForKey(key.Public().(ed25519.PublicKey))
	deviceID := "phone_1"
	deviceCap := strings.Repeat("d", MinCredentialLen)

	_, err := client.ICE(context.Background(), hostID, PeerDevice, deviceID, deviceCap)
	assertBrokerCode(t, err, CodeNotFound)
	_, err = client.DialDevice(context.Background(), hostID, deviceID, deviceCap)
	assertBrokerCode(t, err, CodeNotFound)

	registration := mustRegister(t, client, key, "", []DeviceCapabilityHash{{DeviceID: deviceID, Hash: HashCapability(deviceCap)}})
	wrong := strings.Repeat("w", MinCredentialLen)
	_, err = client.ICE(context.Background(), hostID, PeerDevice, deviceID, wrong)
	assertBrokerCode(t, err, CodeUnauthorized)
	_, err = client.DialDevice(context.Background(), hostID, deviceID, wrong)
	assertBrokerCode(t, err, CodeUnauthorized)

	if _, err := client.Renew(context.Background(), hostID, registration.LeaseCapability, nil); err != nil {
		t.Fatal(err)
	}
	_, err = client.ICE(context.Background(), hostID, PeerDevice, deviceID, deviceCap)
	assertBrokerCode(t, err, CodeUnauthorized)
	_, err = client.DialDevice(context.Background(), hostID, deviceID, deviceCap)
	assertBrokerCode(t, err, CodeUnauthorized)

	registration = mustRegister(t, client, key, "", []DeviceCapabilityHash{{DeviceID: deviceID, Hash: HashCapability(deviceCap)}})
	now = now.Add(2 * time.Minute)
	_, err = client.ICE(context.Background(), registration.HostID, PeerDevice, deviceID, deviceCap)
	assertBrokerCode(t, err, CodeExpired)
	_, err = client.DialDevice(context.Background(), registration.HostID, deviceID, deviceCap)
	assertBrokerCode(t, err, CodeExpired)

	h.mu.Lock()
	_, retained := h.expiredHosts[registration.HostID]
	h.mu.Unlock()
	if !retained {
		t.Fatal("expired lease was not retained long enough to distinguish it from an unknown host")
	}
}

func TestICEQuotaIsPerPrincipalResetByRenewalAndSkipsStaticOnly(t *testing.T) {
	secret := strings.Repeat("s", MinCredentialLen)
	_, client, _ := newBrokerClient(t, Config{
		MaxICEMintsPerLease: 2,
		Coturn:              &CoturnConfig{URLs: []string{"turn:relay.example:3478"}, SharedSecret: secret, TTL: time.Hour},
	})
	deviceOne, deviceTwo := strings.Repeat("1", MinCredentialLen), strings.Repeat("2", MinCredentialLen)
	devices := []DeviceCapabilityHash{
		{DeviceID: "one", Hash: HashCapability(deviceOne)},
		{DeviceID: "two", Hash: HashCapability(deviceTwo)},
	}
	registration := mustRegister(t, client, mustKey(t), "", devices)
	for range 2 {
		if _, err := client.ICE(context.Background(), registration.HostID, PeerDevice, "one", deviceOne); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.ICE(context.Background(), registration.HostID, PeerDevice, "one", deviceOne); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("per-device quota error = %v", err)
	}
	if _, err := client.ICE(context.Background(), registration.HostID, PeerDevice, "two", deviceTwo); err != nil {
		t.Fatalf("one device starved another: %v", err)
	}
	if _, err := client.ICE(context.Background(), registration.HostID, PeerHost, "", registration.LeaseCapability); err != nil {
		t.Fatalf("device mints starved host: %v", err)
	}
	if _, err := client.Renew(context.Background(), registration.HostID, registration.LeaseCapability, devices); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ICE(context.Background(), registration.HostID, PeerDevice, "one", deviceOne); err != nil {
		t.Fatalf("renewal did not reset quota: %v", err)
	}

	_, staticClient, _ := newBrokerClient(t, Config{MaxICEMintsPerLease: 1})
	staticRegistration := mustRegister(t, staticClient, mustKey(t), "", devices[:1])
	for range 4 {
		if _, err := staticClient.ICE(context.Background(), staticRegistration.HostID, PeerDevice, "one", deviceOne); err != nil {
			t.Fatalf("static-only ICE was unnecessarily quota limited: %v", err)
		}
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

type fakeTURNCredentialProvider struct {
	urls  []string
	now   time.Time
	calls int
}

func (p *fakeTURNCredentialProvider) URLs() []string { return append([]string(nil), p.urls...) }
func (p *fakeTURNCredentialProvider) Mint(_ context.Context, subject string, urls []string) (ICECredentials, error) {
	p.calls++
	return ICECredentials{
		ICEServers: []ICEServer{{URLs: append([]string(nil), urls...), Username: "managed:" + subject, Credential: "managed-credential"}},
		ExpiresAt:  p.now.Add(time.Hour),
	}, nil
}

func TestInvitationICEIsSTUNOnlyWithoutTURNMintOrQuota(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		name := "legacy"
		if explicit {
			name = "explicit"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Unix(2_000_000_000, 0)
			provider := &fakeTURNCredentialProvider{urls: []string{"turns:managed.example:5349?transport=tcp"}, now: now}
			_, client, _ := newBrokerClient(t, Config{
				STUNURLs:            []string{"stun:global.example:3478"},
				TURNProvider:        provider,
				MaxICEMintsPerLease: 1,
				Now:                 func() time.Time { return now },
			})
			key := mustKey(t)
			var registration *Registration
			var err error
			if explicit {
				policy := NewHostICEConfig([]ConfiguredICEServer{
					{URLs: []string{"stun:policy.example:3478"}},
					{URLs: provider.URLs(), CredentialSource: CredentialSourceBroker},
				})
				registration, err = client.RegisterHostWithPolicy(context.Background(), key, "", nil, &policy)
			} else {
				registration, err = client.RegisterHost(context.Background(), key, "", nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			invitationCapability := strings.Repeat("i", MinCredentialLen)
			if _, err := client.RegisterInvitation(context.Background(), registration.HostID, registration.LeaseCapability, "invite", HashCapability(invitationCapability), now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}

			for range 3 {
				ice, err := client.ICE(context.Background(), registration.HostID, PeerInvitation, "invite", invitationCapability)
				if err != nil {
					t.Fatalf("invitation ICE: %v", err)
				}
				if len(ice.ICEServers) != 1 || len(ice.ICEServers[0].URLs) != 1 || !strings.HasPrefix(ice.ICEServers[0].URLs[0], "stun:") || ice.ICEServers[0].Username != "" || ice.ICEServers[0].Credential != "" || !ice.ExpiresAt.IsZero() {
					t.Fatalf("invitation ICE = %+v, want STUN only", ice)
				}
			}
			if provider.calls != 0 {
				t.Fatalf("invitation called TURN provider %d times", provider.calls)
			}
			if _, err := client.ICE(context.Background(), registration.HostID, PeerHost, "", registration.LeaseCapability); err != nil {
				t.Fatalf("invitation consumed host mint quota: %v", err)
			}
			if provider.calls != 1 {
				t.Fatalf("host TURN provider calls = %d, want 1", provider.calls)
			}
			if _, err := client.ICE(context.Background(), registration.HostID, PeerHost, "", registration.LeaseCapability); !errors.Is(err, ErrRateLimited) {
				t.Fatalf("host mint quota error = %v", err)
			}
		})
	}
}

func TestTURNProviderBoundaryMintsBrokerPolicy(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	provider := &fakeTURNCredentialProvider{urls: []string{"turns:managed.example:5349?transport=tcp"}, now: now}
	_, client, _ := newBrokerClient(t, Config{TURNProvider: provider, Now: func() time.Time { return now }})
	policy := NewHostICEConfig([]ConfiguredICEServer{{URLs: provider.URLs(), CredentialSource: CredentialSourceBroker}})
	registration, err := client.RegisterHostWithPolicy(context.Background(), mustKey(t), "", nil, &policy)
	if err != nil {
		t.Fatal(err)
	}
	ice, err := client.ICE(context.Background(), registration.HostID, PeerHost, "", registration.LeaseCapability)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 || len(ice.ICEServers) != 1 || ice.ICEServers[0].Username != "managed:"+registration.HostID || !ice.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("managed TURN response = %+v calls=%d", ice, provider.calls)
	}
}

type blockingTURNCredentialProvider struct {
	urls    []string
	now     time.Time
	started chan struct{}
	release chan struct{}
}

func (p *blockingTURNCredentialProvider) URLs() []string { return append([]string(nil), p.urls...) }
func (p *blockingTURNCredentialProvider) Mint(ctx context.Context, subject string, urls []string) (ICECredentials, error) {
	close(p.started)
	select {
	case <-p.release:
	case <-ctx.Done():
		return ICECredentials{}, ctx.Err()
	}
	return ICECredentials{
		ICEServers: []ICEServer{{URLs: append([]string(nil), urls...), Username: "managed:" + subject, Credential: "managed-credential"}},
		ExpiresAt:  p.now.Add(time.Hour),
	}, nil
}

func TestTURNProviderRevalidatesDeviceAuthorityAfterMint(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	provider := &blockingTURNCredentialProvider{
		urls: []string{"turns:managed.example:5349?transport=tcp"}, now: now,
		started: make(chan struct{}), release: make(chan struct{}),
	}
	_, client, _ := newBrokerClient(t, Config{TURNProvider: provider, Now: func() time.Time { return now }})
	deviceCapability := strings.Repeat("d", MinCredentialLen)
	devices := []DeviceCapabilityHash{{DeviceID: "device", Hash: HashCapability(deviceCapability)}}
	policy := NewHostICEConfig([]ConfiguredICEServer{{URLs: provider.URLs(), CredentialSource: CredentialSourceBroker}})
	registration, err := client.RegisterHostWithPolicy(context.Background(), mustKey(t), "", devices, &policy)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, iceErr := client.ICE(context.Background(), registration.HostID, PeerDevice, "device", deviceCapability)
		result <- iceErr
	}()
	<-provider.started
	if _, err := client.Renew(context.Background(), registration.HostID, registration.LeaseCapability, nil); err != nil {
		t.Fatal(err)
	}
	close(provider.release)
	if err := <-result; !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ICE completed after device authority was revoked: %v", err)
	}
}

func rawJSON(value any) json.RawMessage { data, _ := json.Marshal(value); return data }

func TestICEPolicySignatureBinding(t *testing.T) {
	coturnSecret := strings.Repeat("c", MinCredentialLen)
	cfg := Config{
		Coturn: &CoturnConfig{
			URLs:         []string{"turns:relay.example:5349?transport=tcp"},
			SharedSecret: coturnSecret,
			TTL:          time.Hour,
		},
	}
	_, client, _ := newBrokerClient(t, cfg)
	key := mustKey(t)
	publicKey := key.Public().(ed25519.PublicKey)
	encodedKey := base64.RawURLEncoding.EncodeToString(publicKey)

	var challenge Challenge
	if err := client.do(context.Background(), http.MethodPost, "/v2/challenges", "", challengeRequest{PublicKey: encodedKey}, http.StatusCreated, &challenge); err != nil {
		t.Fatal(err)
	}

	// Policy that will be signed
	signedPolicy := []wireICEServer{
		{URLs: []string{"stun:stun.example:3478"}},
		{URLs: []string{"turns:relay.example:5349?transport=tcp"}, CredentialSource: "broker"},
	}

	// Different policy to tamper with
	tamperedPolicy := []wireICEServer{
		{URLs: []string{"stun:evil.example:3478"}},
	}

	signature := ed25519.Sign(key, registrationPayload(publicKey, challenge.ID, challenge.Nonce, nil, &signedPolicy))
	request := registerRequest{
		PublicKey:   encodedKey,
		ChallengeID: challenge.ID,
		Signature:   base64.RawURLEncoding.EncodeToString(signature),
		ICEPolicy:   &tamperedPolicy,
	}

	var out Registration
	// Should fail because signature doesn't match tampered policy
	if err := client.do(context.Background(), http.MethodPost, "/v2/hosts", "", request, http.StatusCreated, &out); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("tampered policy error = %v, want unauthorized", err)
	}

	// Get a new challenge for the valid registration
	var challenge2 Challenge
	if err := client.do(context.Background(), http.MethodPost, "/v2/challenges", "", challengeRequest{PublicKey: encodedKey}, http.StatusCreated, &challenge2); err != nil {
		t.Fatal(err)
	}

	// Should succeed with matching policy
	signature2 := ed25519.Sign(key, registrationPayload(publicKey, challenge2.ID, challenge2.Nonce, nil, &signedPolicy))
	request2 := registerRequest{
		PublicKey:   encodedKey,
		ChallengeID: challenge2.ID,
		Signature:   base64.RawURLEncoding.EncodeToString(signature2),
		ICEPolicy:   &signedPolicy,
	}
	if err := client.do(context.Background(), http.MethodPost, "/v2/hosts", "", request2, http.StatusCreated, &out); err != nil {
		t.Fatalf("valid policy registration failed: %v", err)
	}
}

func TestICEPolicyExplicitEmptyDisablesLegacyDefaults(t *testing.T) {
	secret := strings.Repeat("s", MinCredentialLen)
	_, client, _ := newBrokerClient(t, Config{
		STUNURLs: []string{"stun:legacy.example:3478"},
		Coturn:   &CoturnConfig{URLs: []string{"turn:legacy.example:3478"}, SharedSecret: secret, TTL: time.Hour},
	})
	policy := NewHostICEConfig(nil)
	registration, err := client.RegisterHostWithPolicy(context.Background(), mustKey(t), "", nil, &policy)
	if err != nil {
		t.Fatal(err)
	}
	ice, err := client.ICE(context.Background(), registration.HostID, PeerHost, "", registration.LeaseCapability)
	if err != nil {
		t.Fatal(err)
	}
	if len(ice.ICEServers) != 0 || !ice.ExpiresAt.IsZero() {
		t.Fatalf("explicit host-only ICE policy fell back to broker defaults: %+v", ice)
	}
}

func TestICEPolicyArbitraryRelayRejection(t *testing.T) {
	coturnSecret := strings.Repeat("c", MinCredentialLen)
	cfg := Config{
		Coturn: &CoturnConfig{
			URLs:         []string{"turns:relay.example:5349?transport=tcp"},
			SharedSecret: coturnSecret,
			TTL:          time.Hour,
		},
	}
	_, client, _ := newBrokerClient(t, cfg)
	key := mustKey(t)

	// Try to register with a broker TURN URL that doesn't match coturn config
	arbitraryPolicy := []wireICEServer{
		{URLs: []string{"turns:evil.example:5349?transport=tcp"}, CredentialSource: "broker"},
	}

	publicKey := key.Public().(ed25519.PublicKey)
	encodedKey := base64.RawURLEncoding.EncodeToString(publicKey)

	var challenge Challenge
	if err := client.do(context.Background(), http.MethodPost, "/v2/challenges", "", challengeRequest{PublicKey: encodedKey}, http.StatusCreated, &challenge); err != nil {
		t.Fatal(err)
	}

	signature := ed25519.Sign(key, registrationPayload(publicKey, challenge.ID, challenge.Nonce, nil, &arbitraryPolicy))
	request := registerRequest{
		PublicKey:   encodedKey,
		ChallengeID: challenge.ID,
		Signature:   base64.RawURLEncoding.EncodeToString(signature),
		ICEPolicy:   &arbitraryPolicy,
	}

	var out Registration
	err := client.do(context.Background(), http.MethodPost, "/v2/hosts", "", request, http.StatusCreated, &out)
	if err == nil {
		t.Fatal("arbitrary broker TURN URL was accepted")
	}
	if !strings.Contains(err.Error(), "not in configured credential provider") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestICEPolicyStaticDeliveryToHostAndDevice(t *testing.T) {
	staticUsername := "static-user"
	staticPassword := "static-pass-" + strings.Repeat("x", MinCredentialLen)
	cfg := Config{}
	h, client, _ := newBrokerClient(t, cfg)

	key := mustKey(t)
	publicKey := key.Public().(ed25519.PublicKey)
	encodedKey := base64.RawURLEncoding.EncodeToString(publicKey)

	var challenge Challenge
	if err := client.do(context.Background(), http.MethodPost, "/v2/challenges", "", challengeRequest{PublicKey: encodedKey}, http.StatusCreated, &challenge); err != nil {
		t.Fatal(err)
	}

	policy := []wireICEServer{
		{URLs: []string{"stun:stun.example:3478"}},
		{URLs: []string{"turns:relay.example:5349"}, CredentialSource: "static", Username: staticUsername, Credential: staticPassword},
	}

	deviceCap := strings.Repeat("d", MinCredentialLen)
	devices := []DeviceCapabilityHash{{DeviceID: "phone", Hash: HashCapability(deviceCap)}}

	signature := ed25519.Sign(key, registrationPayload(publicKey, challenge.ID, challenge.Nonce, devices, &policy))
	request := registerRequest{
		PublicKey:   encodedKey,
		ChallengeID: challenge.ID,
		Signature:   base64.RawURLEncoding.EncodeToString(signature),
		Devices:     devices,
		ICEPolicy:   &policy,
	}

	var registration Registration
	if err := client.do(context.Background(), http.MethodPost, "/v2/hosts", "", request, http.StatusCreated, &registration); err != nil {
		t.Fatalf("registration with static policy failed: %v", err)
	}

	// Verify host receives static credentials
	hostICE, err := client.ICE(context.Background(), registration.HostID, PeerHost, "", registration.LeaseCapability)
	if err != nil {
		t.Fatalf("host ICE failed: %v", err)
	}
	if len(hostICE.ICEServers) != 2 {
		t.Fatalf("host ICE servers = %d, want 2", len(hostICE.ICEServers))
	}
	if hostICE.ICEServers[1].Username != staticUsername || hostICE.ICEServers[1].Credential != staticPassword {
		t.Fatalf("host did not receive static credentials: %+v", hostICE.ICEServers[1])
	}

	// Verify approved device receives static credentials
	deviceICE, err := client.ICE(context.Background(), registration.HostID, PeerDevice, "phone", deviceCap)
	if err != nil {
		t.Fatalf("device ICE failed: %v", err)
	}
	if len(deviceICE.ICEServers) != 2 {
		t.Fatalf("device ICE servers = %d, want 2", len(deviceICE.ICEServers))
	}
	if deviceICE.ICEServers[1].Username != staticUsername || deviceICE.ICEServers[1].Credential != staticPassword {
		t.Fatalf("device did not receive static credentials: %+v", deviceICE.ICEServers[1])
	}
	if _, err := client.Renew(context.Background(), registration.HostID, registration.LeaseCapability, devices); err != nil {
		t.Fatal(err)
	}
	afterRenewal, err := client.ICE(context.Background(), registration.HostID, PeerDevice, "phone", deviceCap)
	if err != nil || len(afterRenewal.ICEServers) != 2 || afterRenewal.ICEServers[1].Credential != staticPassword {
		t.Fatalf("renewal did not retain validated ICE policy: servers=%v err=%v", afterRenewal, err)
	}

	// Verify policy is stored and redacted
	h.mu.Lock()
	stored := h.hosts[registration.HostID]
	h.mu.Unlock()
	if stored == nil || stored.icePolicy.Empty() {
		t.Fatal("policy not stored in host lease")
	}
	policyStr := stored.icePolicy.String()
	if strings.Contains(policyStr, staticPassword) {
		t.Fatalf("policy String() leaked credential: %s", policyStr)
	}
	if !strings.Contains(policyStr, "[REDACTED]") {
		t.Fatalf("policy String() did not redact credential: %s", policyStr)
	}
}

func TestICEPolicyInvitationFilteringOfStatic(t *testing.T) {
	staticPassword := "static-pass-" + strings.Repeat("x", MinCredentialLen)
	cfg := Config{}
	_, client, _ := newBrokerClient(t, cfg)

	key := mustKey(t)
	publicKey := key.Public().(ed25519.PublicKey)
	encodedKey := base64.RawURLEncoding.EncodeToString(publicKey)

	var challenge Challenge
	if err := client.do(context.Background(), http.MethodPost, "/v2/challenges", "", challengeRequest{PublicKey: encodedKey}, http.StatusCreated, &challenge); err != nil {
		t.Fatal(err)
	}

	policy := []wireICEServer{
		{URLs: []string{"stun:stun.example:3478"}},
		{URLs: []string{"turns:relay.example:5349"}, CredentialSource: "static", Username: "user", Credential: staticPassword},
	}

	signature := ed25519.Sign(key, registrationPayload(publicKey, challenge.ID, challenge.Nonce, nil, &policy))
	request := registerRequest{
		PublicKey:   encodedKey,
		ChallengeID: challenge.ID,
		Signature:   base64.RawURLEncoding.EncodeToString(signature),
		ICEPolicy:   &policy,
	}

	var registration Registration
	if err := client.do(context.Background(), http.MethodPost, "/v2/hosts", "", request, http.StatusCreated, &registration); err != nil {
		t.Fatalf("registration failed: %v", err)
	}

	// Create an invitation
	inviteCap := strings.Repeat("i", MinCredentialLen)
	expires := time.Now().Add(time.Minute)
	_, err := client.RegisterInvitation(context.Background(), registration.HostID, registration.LeaseCapability, "invite1", HashCapability(inviteCap), expires)
	if err != nil {
		t.Fatalf("invitation creation failed: %v", err)
	}

	// Verify invitation does NOT receive static credentials
	inviteICE, err := client.ICE(context.Background(), registration.HostID, PeerInvitation, "invite1", inviteCap)
	if err != nil {
		t.Fatalf("invitation ICE failed: %v", err)
	}

	// Should only have STUN, not the static TURN
	if len(inviteICE.ICEServers) != 1 {
		t.Fatalf("invitation ICE servers = %d, want 1 (STUN only)", len(inviteICE.ICEServers))
	}
	if inviteICE.ICEServers[0].URLs[0] != "stun:stun.example:3478" {
		t.Fatalf("invitation ICE unexpected server: %+v", inviteICE.ICEServers[0])
	}

	// Verify no server has credentials
	for i, srv := range inviteICE.ICEServers {
		if srv.Username != "" || srv.Credential != "" {
			t.Fatalf("invitation ICE server[%d] leaked credentials: %+v", i, srv)
		}
	}
}

func TestICEPolicyBrokerMintedCredentials(t *testing.T) {
	coturnSecret := strings.Repeat("c", MinCredentialLen)
	now := time.Unix(2_000_000_000, 0)
	cfg := Config{
		Now: func() time.Time { return now },
		Coturn: &CoturnConfig{
			URLs:         []string{"turns:relay.example:5349?transport=tcp"},
			SharedSecret: coturnSecret,
			TTL:          time.Hour,
			Username:     "mobile",
		},
	}
	_, client, _ := newBrokerClient(t, cfg)

	key := mustKey(t)
	publicKey := key.Public().(ed25519.PublicKey)
	encodedKey := base64.RawURLEncoding.EncodeToString(publicKey)

	var challenge Challenge
	if err := client.do(context.Background(), http.MethodPost, "/v2/challenges", "", challengeRequest{PublicKey: encodedKey}, http.StatusCreated, &challenge); err != nil {
		t.Fatal(err)
	}

	policy := []wireICEServer{
		{URLs: []string{"stun:stun.example:3478"}},
		{URLs: []string{"turns:relay.example:5349?transport=tcp"}, CredentialSource: "broker"},
	}

	deviceCap := strings.Repeat("d", MinCredentialLen)
	devices := []DeviceCapabilityHash{{DeviceID: "phone", Hash: HashCapability(deviceCap)}}

	signature := ed25519.Sign(key, registrationPayload(publicKey, challenge.ID, challenge.Nonce, devices, &policy))
	request := registerRequest{
		PublicKey:   encodedKey,
		ChallengeID: challenge.ID,
		Signature:   base64.RawURLEncoding.EncodeToString(signature),
		Devices:     devices,
		ICEPolicy:   &policy,
	}

	var registration Registration
	if err := client.do(context.Background(), http.MethodPost, "/v2/hosts", "", request, http.StatusCreated, &registration); err != nil {
		t.Fatalf("registration failed: %v", err)
	}

	// Verify host receives broker-minted credentials
	hostICE, err := client.ICE(context.Background(), registration.HostID, PeerHost, "", registration.LeaseCapability)
	if err != nil {
		t.Fatalf("host ICE failed: %v", err)
	}
	if len(hostICE.ICEServers) != 2 {
		t.Fatalf("host ICE servers = %d, want 2", len(hostICE.ICEServers))
	}

	turn := hostICE.ICEServers[1]
	if !strings.Contains(turn.Username, ":mobile:") || turn.Credential == "" {
		t.Fatalf("broker TURN credentials not minted: %+v", turn)
	}

	// Verify credentials are valid
	mac := hmac.New(sha1.New, []byte(coturnSecret))
	_, _ = mac.Write([]byte(turn.Username))
	expectedCred := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if turn.Credential != expectedCred {
		t.Fatalf("broker credential invalid: got %q, want %q", turn.Credential, expectedCred)
	}

	// Verify expiry is set
	if hostICE.ExpiresAt.IsZero() {
		t.Fatal("broker minted credentials have no expiry")
	}
}

func TestICEPolicyQuotaOnlyForMinted(t *testing.T) {
	coturnSecret := strings.Repeat("c", MinCredentialLen)
	staticPassword := "static-pass-" + strings.Repeat("x", MinCredentialLen)

	// Broker with low quota
	brokerCfg := Config{
		MaxICEMintsPerLease: 2,
		Coturn: &CoturnConfig{
			URLs:         []string{"turns:relay.example:5349?transport=tcp"},
			SharedSecret: coturnSecret,
			TTL:          time.Hour,
		},
	}
	_, brokerClient, _ := newBrokerClient(t, brokerCfg)

	// Register host with broker policy
	brokerKey := mustKey(t)
	brokerPublicKey := brokerKey.Public().(ed25519.PublicKey)
	encodedBrokerKey := base64.RawURLEncoding.EncodeToString(brokerPublicKey)

	var brokerChallenge Challenge
	if err := brokerClient.do(context.Background(), http.MethodPost, "/v2/challenges", "", challengeRequest{PublicKey: encodedBrokerKey}, http.StatusCreated, &brokerChallenge); err != nil {
		t.Fatal(err)
	}

	brokerPolicy := []wireICEServer{
		{URLs: []string{"turns:relay.example:5349?transport=tcp"}, CredentialSource: "broker"},
	}

	brokerSignature := ed25519.Sign(brokerKey, registrationPayload(brokerPublicKey, brokerChallenge.ID, brokerChallenge.Nonce, nil, &brokerPolicy))
	brokerRequest := registerRequest{
		PublicKey:   encodedBrokerKey,
		ChallengeID: brokerChallenge.ID,
		Signature:   base64.RawURLEncoding.EncodeToString(brokerSignature),
		ICEPolicy:   &brokerPolicy,
	}

	var brokerRegistration Registration
	if err := brokerClient.do(context.Background(), http.MethodPost, "/v2/hosts", "", brokerRequest, http.StatusCreated, &brokerRegistration); err != nil {
		t.Fatalf("broker registration failed: %v", err)
	}

	// Exhaust quota
	for i := 0; i < 2; i++ {
		if _, err := brokerClient.ICE(context.Background(), brokerRegistration.HostID, PeerHost, "", brokerRegistration.LeaseCapability); err != nil {
			t.Fatalf("ICE request %d failed: %v", i, err)
		}
	}

	// Next request should fail with quota exceeded
	_, err := brokerClient.ICE(context.Background(), brokerRegistration.HostID, PeerHost, "", brokerRegistration.LeaseCapability)
	assertBrokerCode(t, err, CodeQuotaExceeded)

	// Now test static-only policy does NOT consume quota
	staticCfg := Config{MaxICEMintsPerLease: 2}
	_, staticClient, _ := newBrokerClient(t, staticCfg)

	staticKey := mustKey(t)
	staticPublicKey := staticKey.Public().(ed25519.PublicKey)
	encodedStaticKey := base64.RawURLEncoding.EncodeToString(staticPublicKey)

	var staticChallenge Challenge
	if err := staticClient.do(context.Background(), http.MethodPost, "/v2/challenges", "", challengeRequest{PublicKey: encodedStaticKey}, http.StatusCreated, &staticChallenge); err != nil {
		t.Fatal(err)
	}

	staticPolicy := []wireICEServer{
		{URLs: []string{"stun:stun.example:3478"}},
		{URLs: []string{"turns:relay.example:5349"}, CredentialSource: "static", Username: "user", Credential: staticPassword},
	}

	staticSignature := ed25519.Sign(staticKey, registrationPayload(staticPublicKey, staticChallenge.ID, staticChallenge.Nonce, nil, &staticPolicy))
	staticRequest := registerRequest{
		PublicKey:   encodedStaticKey,
		ChallengeID: staticChallenge.ID,
		Signature:   base64.RawURLEncoding.EncodeToString(staticSignature),
		ICEPolicy:   &staticPolicy,
	}

	var staticRegistration Registration
	if err := staticClient.do(context.Background(), http.MethodPost, "/v2/hosts", "", staticRequest, http.StatusCreated, &staticRegistration); err != nil {
		t.Fatalf("static registration failed: %v", err)
	}

	// Should be able to make many requests without hitting quota
	for i := 0; i < 10; i++ {
		if _, err := staticClient.ICE(context.Background(), staticRegistration.HostID, PeerHost, "", staticRegistration.LeaseCapability); err != nil {
			t.Fatalf("static ICE request %d unexpectedly failed: %v", i, err)
		}
	}
}

func TestICEPolicyValidation(t *testing.T) {
	coturnSecret := strings.Repeat("c", MinCredentialLen)
	cfg := Config{
		Coturn: &CoturnConfig{
			URLs:         []string{"turns:relay.example:5349?transport=tcp"},
			SharedSecret: coturnSecret,
			TTL:          time.Hour,
		},
	}

	tests := []struct {
		name    string
		policy  []wireICEServer
		wantErr string
	}{
		{
			name:    "empty URLs",
			policy:  []wireICEServer{{URLs: []string{}}},
			wantErr: "no URLs",
		},
		{
			name: "mixed STUN and TURN",
			policy: []wireICEServer{
				{URLs: []string{"stun:stun.example:3478", "turn:relay.example:3478"}},
			},
			wantErr: "mixed STUN and TURN",
		},
		{
			name: "STUN with credentials",
			policy: []wireICEServer{
				{URLs: []string{"stun:stun.example:3478"}, Username: "user"},
			},
			wantErr: "must not have username",
		},
		{
			name: "broker without TURN",
			policy: []wireICEServer{
				{URLs: []string{"stun:stun.example:3478"}, CredentialSource: "broker"},
			},
			wantErr: "requires TURN",
		},
		{
			name: "broker with credentials",
			policy: []wireICEServer{
				{URLs: []string{"turns:relay.example:5349?transport=tcp"}, CredentialSource: "broker", Username: "user"},
			},
			wantErr: "must not have username",
		},
		{
			name: "static without credential",
			policy: []wireICEServer{
				{URLs: []string{"turns:relay.example:5349"}, CredentialSource: "static", Username: "user"},
			},
			wantErr: "requires username and credential",
		},
		{
			name: "too many servers",
			policy: func() []wireICEServer {
				servers := make([]wireICEServer, 9)
				for i := range servers {
					servers[i] = wireICEServer{URLs: []string{"stun:stun.example:3478"}}
				}
				return servers
			}(),
			wantErr: "too many ICE server entries",
		},
		{
			name: "invalid scheme",
			policy: []wireICEServer{
				{URLs: []string{"http://example.com"}},
			},
			wantErr: "invalid scheme",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coturnURLs := cfg.Coturn.URLs
			_, err := validateHostICEPolicy(tt.policy, coturnURLs)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestICEPolicyLegacyCompatibility(t *testing.T) {
	coturnSecret := strings.Repeat("c", MinCredentialLen)
	cfg := Config{
		STUNURLs: []string{"stun:global.example:3478"},
		Coturn: &CoturnConfig{
			URLs:         []string{"turns:global.example:5349?transport=tcp"},
			SharedSecret: coturnSecret,
			TTL:          time.Hour,
		},
	}
	_, client, _ := newBrokerClient(t, cfg)

	// Register without ICEPolicy (legacy)
	key := mustKey(t)
	registration := mustRegister(t, client, key, "", nil)

	// Should receive global STUN/coturn
	hostICE, err := client.ICE(context.Background(), registration.HostID, PeerHost, "", registration.LeaseCapability)
	if err != nil {
		t.Fatalf("legacy host ICE failed: %v", err)
	}

	if len(hostICE.ICEServers) != 2 {
		t.Fatalf("legacy ICE servers = %d, want 2", len(hostICE.ICEServers))
	}
	if hostICE.ICEServers[0].URLs[0] != "stun:global.example:3478" {
		t.Fatalf("legacy STUN incorrect: %+v", hostICE.ICEServers[0])
	}
	if hostICE.ICEServers[1].URLs[0] != "turns:global.example:5349?transport=tcp" {
		t.Fatalf("legacy TURN incorrect: %+v", hostICE.ICEServers[1])
	}
}

func TestHostICEConfigRedaction(t *testing.T) {
	secret := "my-secret-password-" + strings.Repeat("x", MinCredentialLen)
	servers := []ConfiguredICEServer{
		{URLs: []string{"stun:stun.example:3478"}},
		{URLs: []string{"turns:relay.example:5349"}, CredentialSource: CredentialSourceStatic, Username: "user", Credential: secret},
	}

	cfg := NewHostICEConfig(servers)
	str := cfg.String()
	goStr := cfg.GoString()

	if strings.Contains(str, secret) {
		t.Fatalf("String() leaked credential: %s", str)
	}
	if strings.Contains(goStr, secret) {
		t.Fatalf("GoString() leaked credential: %s", goStr)
	}
	if !strings.Contains(str, "[REDACTED]") {
		t.Fatalf("String() did not redact credential: %s", str)
	}

	// Verify ConfiguredICEServer redaction
	srv := ConfiguredICEServer{URLs: []string{"turns:relay.example:5349"}, CredentialSource: CredentialSourceStatic, Username: "user", Credential: secret}
	srvStr := srv.String()
	if strings.Contains(srvStr, secret) {
		t.Fatalf("ConfiguredICEServer String() leaked credential: %s", srvStr)
	}
}

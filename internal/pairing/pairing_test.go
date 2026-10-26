package pairing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testClock struct{ nowTime time.Time }

func (c *testClock) now() time.Time { return c.nowTime }

func newTestHost(t *testing.T) (*Host, *Identity, *testClock) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	hostID, err := LoadOrCreateIdentity(filepath.Join(dir, "host.key"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenGrantStore(filepath.Join(dir, "grants.json"))
	if err != nil {
		t.Fatal(err)
	}
	device, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{nowTime: time.Unix(1_800_000_000, 0)}
	host, err := NewHost(hostID, store, HostOptions{Broker: "https://broker.example", HostLabel: "desk", BasePath: "/chat", Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	return host, device, clock
}
func invitationID() string     { return B64(bytes.Repeat([]byte{0x42}, InvitationIDSize)).String() }
func testSDP(fp string) string { return "v=0\r\na=fingerprint:sha-256 " + fp + "\r\n" }

const fingerprintA = "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
const fingerprintB = "11:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"

func TestPairingV2AndTransport(t *testing.T) {
	host, device, clock := newTestHost(t)
	link, err := host.NewPairingForInvitation(invitationID(), "cap_1", clock.nowTime.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseLink(link.String())
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := StartPairing(parsed, device, "phone", clock.nowTime)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := host.VerifyPairRequest(jsonRoundTrip(t, attempt.Request()).(*PairRequest))
	if err != nil {
		t.Fatal(err)
	}
	if pending.SAS != attempt.SAS() {
		t.Fatalf("SAS mismatch %q != %q", pending.SAS, attempt.SAS())
	}
	if err := host.Approve(context.Background(), pending, ApproverFunc(func(context.Context, PairingPrompt) (bool, error) { return true, nil })); err != nil {
		t.Fatal(err)
	}
	answer, grant, err := host.CompletePairing(pending)
	if err != nil {
		t.Fatal(err)
	}
	paired, err := attempt.VerifyAnswer(jsonRoundTrip(t, answer).(*PairAnswer))
	if err != nil {
		t.Fatal(err)
	}
	if grant.Scope != ScopeWebV1 || grant.BasePath != "/chat" || !grant.PairedAt.Equal(clock.nowTime.UTC()) || grant.RevokedAt != nil {
		t.Fatalf("bad grant: %+v", grant)
	}
	if answer.Scope != ScopeWebV1 || answer.HostLabel != "desk" || len(paired.ReconnectCapability) != SecretSize {
		t.Fatalf("bad answer: %+v", answer)
	}
	hash := sha256.Sum256(paired.ReconnectCapability)
	if !bytes.Equal(grant.ReconnectCapabilityHash, hash[:]) {
		t.Fatal("raw reconnect capability was not hashed in grant")
	}
	state, err := os.ReadFile(host.Grants().Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(state, []byte(B64(paired.ReconnectCapability).String())) {
		t.Fatal("grant state contains raw reconnect capability")
	}
	if bytes.Contains(state, []byte("expires")) || !bytes.Contains(state, []byte(`"v": 1`)) {
		t.Fatalf("grant state must be non-expiring state schema v1: %s", state)
	}
	hello, err := NewTransportHello(device, paired.HostKey, paired.DeviceID, paired.ReconnectCapability)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := host.NewTransportChallenge(jsonRoundTrip(t, hello).(*TransportHello))
	if err != nil {
		t.Fatal(err)
	}
	transport, err := StartTransport(challenge, paired.HostKey, device, paired.DeviceID, testSDP(fingerprintA), clock.nowTime)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := host.VerifyTransport(jsonRoundTrip(t, transport.Request()).(*TransportRequest))
	if err != nil {
		t.Fatal(err)
	}
	transportAnswer, err := host.AnswerTransport(verified, testSDP(fingerprintB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.VerifyAnswer(jsonRoundTrip(t, transportAnswer).(*TransportAnswer)); err != nil {
		t.Fatal(err)
	}
	stored, err := host.Grants().Get(grant.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastSeenAt == nil {
		t.Fatal("transport did not update last seen")
	}
	if err := host.Grants().Revoke(grant.DeviceID, clock.nowTime); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Grants().Authorize(grant.DeviceID, device.PublicKey()); !errors.Is(err, ErrRevoked) {
		t.Fatalf("authorize after revoke: %v", err)
	}
	if _, err := host.NewTransportChallenge(hello); !errors.Is(err, ErrRevoked) {
		t.Fatalf("challenge after revoke: %v", err)
	}
}

func TestMACBeforeDuplicateAndDuplicateBurn(t *testing.T) {
	host, device, clock := newTestHost(t)
	link, _ := host.NewPairingForInvitation(invitationID(), "", clock.nowTime.Add(time.Minute))
	attempt, _ := StartPairing(link, device, "phone", clock.nowTime)
	pending, err := host.VerifyPairRequest(attempt.Request())
	if err != nil {
		t.Fatal(err)
	}
	bad := *attempt.Request()
	bad.MAC = bytes.Repeat([]byte{0}, MACSize)
	if _, err := host.VerifyPairRequest(&bad); !errors.Is(err, ErrBadMAC) {
		t.Fatalf("bad MAC duplicate disclosed state: %v", err)
	}
	if _, err := host.VerifyPairRequest(attempt.Request()); !errors.Is(err, ErrReplay) {
		t.Fatalf("authenticated duplicate: %v", err)
	}
	if err := host.Approve(context.Background(), pending, ApproverFunc(func(context.Context, PairingPrompt) (bool, error) { return true, nil })); !errors.Is(err, ErrUnknownInvitation) {
		t.Fatalf("duplicate did not burn invitation: %v", err)
	}
}

func TestHostRejectsBasePathsThatRequireEscaping(t *testing.T) {
	host, _, _ := newTestHost(t)
	for _, path := range []string{"/my agent", "/café", "/literal%20escape"} {
		if _, err := NewHost(host.id, host.grants, HostOptions{Broker: "https://broker.example", BasePath: path}); !errors.Is(err, ErrInvalid) {
			t.Errorf("NewHost base path %q = %v, want invalid", path, err)
		}
	}
}

func TestStrictPairLink(t *testing.T) {
	host, _, clock := newTestHost(t)
	link, err := host.NewPairingForInvitation(invitationID(), "invite_cap", clock.nowTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	raw := link.String()
	if !strings.HasPrefix(raw, "termllm://pair?v=2&b=") || !strings.Contains(raw, "&h=") || !strings.Contains(raw, "&i=") || !strings.Contains(raw, "&e=") || !strings.Contains(raw, "&n=") || !strings.Contains(raw, "#s=") || !strings.Contains(raw, "&c=") {
		t.Fatalf("wrong link contract: %s", raw)
	}
	if strings.Contains(link.Redacted(), "#") || strings.Contains(link.Redacted(), "invite_cap") {
		t.Fatalf("redaction leak: %s", link.Redacted())
	}
	for name, bad := range map[string]string{
		"old key":         strings.Replace(raw, "&h=", "&hk=", 1),
		"secret query":    strings.Replace(raw, "&e=", "&s="+B64(link.PairingSecret).String()+"&e=", 1),
		"duplicate":       strings.Replace(raw, "&i=", "&h="+B64(link.HostKey).String()+"&i=", 1),
		"unknown":         raw + "&x=y",
		"wrong authority": strings.Replace(raw, "termllm://pair", "termllm://v2", 1),
		"path":            strings.Replace(raw, "pair?", "pair/x?", 1),
	} {
		if _, err := ParseLink(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func jsonRoundTrip(t *testing.T, value any) any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeMessage(data)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

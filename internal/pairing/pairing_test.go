package pairing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fpA = "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
	fpB = "11:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
)

func sdp(fp string) string {
	return "v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\na=fingerprint:sha-256 " + fp + "\r\n"
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type fixture struct {
	t      *testing.T
	clk    *clock
	host   *Host
	device *Identity
	dir    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	// t.TempDir is 0755; state must live in a private (0700) leaf directory.
	dir := filepath.Join(t.TempDir(), "state")
	hid, err := LoadOrCreateIdentity(filepath.Join(dir, "host.key"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenGrantStore(filepath.Join(dir, "grants.json"))
	if err != nil {
		t.Fatal(err)
	}
	dev, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	h, err := NewHost(hid, store, HostOptions{Broker: "https://broker.example", Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, clk: clk, host: h, device: dev, dir: dir}
}

// mailboxTTL mirrors peerbroker.MaxSessionTTL.
const mailboxTTL = 30 * time.Minute

// newPairing creates a pairing bound to a fresh broker-style mailbox that
// lives for mailboxTTL.
func (f *fixture) newPairing() (Link, error) {
	sid, err := randomBytes(SessionIDSize)
	if err != nil {
		return Link{}, err
	}
	return f.host.NewPairingForSession(B64(sid).String(), "", f.clk.now().Add(mailboxTTL))
}

var yes = ApproverFunc(func(context.Context, PairingPrompt) (bool, error) { return true, nil })
var no = ApproverFunc(func(context.Context, PairingPrompt) (bool, error) { return false, nil })

// roundTrip passes a message through JSON as the broker would.
func roundTrip[T any](t *testing.T, msg *T) *T {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeMessage(data)
	if err != nil {
		t.Fatal(err)
	}
	out, ok := got.(*T)
	if !ok {
		t.Fatalf("decoded %T", got)
	}
	return out
}

func (f *fixture) pair() (*DevicePairing, Grant) {
	f.t.Helper()
	link, err := f.newPairing()
	if err != nil {
		f.t.Fatal(err)
	}
	parsed, err := ParseLink(link.String())
	if err != nil {
		f.t.Fatal(err)
	}
	att, err := StartPairing(parsed, f.device, "Sam's phone", sdp(fpA), f.clk.now())
	if err != nil {
		f.t.Fatal(err)
	}
	p, err := f.host.VerifyPairRequest(roundTrip(f.t, att.Request()))
	if err != nil {
		f.t.Fatal(err)
	}
	if p.OfferFingerprint != "sha-256 "+fpA {
		f.t.Fatalf("offer fingerprint %q", p.OfferFingerprint)
	}
	if err := f.host.Approve(context.Background(), p, yes); err != nil {
		f.t.Fatal(err)
	}
	ans, g, err := f.host.CompletePairing(p, sdp(fpB))
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := att.VerifyAnswer(roundTrip(f.t, ans))
	if err != nil {
		f.t.Fatal(err)
	}
	return res, g
}

func TestPairAndReconnect(t *testing.T) {
	f := newFixture(t)
	res, g := f.pair()
	if res.AnswerSDP != sdp(fpB) || B64(res.GrantID).String() != g.ID {
		t.Fatalf("unexpected pairing result %+v / %+v", res, g)
	}
	ch, err := f.host.NewChallenge(roundTrip(t, NewReconnectHello(f.device, res.GrantID)), res.BrokerSessionID)
	if err != nil {
		t.Fatal(err)
	}
	ra, err := StartReconnect(roundTrip(t, ch), res.HostKey, f.device, res.GrantID, sdp(fpA), f.clk.now())
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.host.VerifyReconnect(roundTrip(t, ra.Request()))
	if err != nil {
		t.Fatal(err)
	}
	ans, err := f.host.AnswerReconnect(v, sdp(fpB))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ra.VerifyAnswer(roundTrip(t, ans)); err != nil || got != sdp(fpB) {
		t.Fatalf("reconnect answer: %q %v", got, err)
	}
	// The challenge is single-use.
	if _, err := f.host.VerifyReconnect(ra.Request()); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("challenge replay: %v", err)
	}
}

func TestPairRequestTampering(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PairRequest)
		want   error
	}{
		{"offer fingerprint replaced", func(r *PairRequest) { r.OfferSDP = sdp(fpB) }, ErrBadMAC},
		{"offer sdp appended", func(r *PairRequest) { r.OfferSDP += "a=candidate:evil\r\n" }, ErrBadMAC},
		{"device key replaced", func(r *PairRequest) { r.DeviceKey = bytes.Repeat([]byte{7}, 32) }, ErrBadMAC},
		{"device name replaced", func(r *PairRequest) { r.DeviceName = "evil" }, ErrBadMAC},
		{"device nonce replaced", func(r *PairRequest) { r.DeviceNonce = bytes.Repeat([]byte{1}, 32) }, ErrBadMAC},
		{"host nonce replaced", func(r *PairRequest) { r.HostNonce = bytes.Repeat([]byte{1}, 32) }, ErrInvalid},
		{"session unknown", func(r *PairRequest) { r.SessionID = bytes.Repeat([]byte{1}, 16) }, ErrUnknownSession},
		{"signature broken", func(r *PairRequest) { r.Sig[0] ^= 1 }, ErrBadSignature},
		{"conflicting fingerprints", func(r *PairRequest) { r.OfferSDP += "a=fingerprint:sha-256 " + fpB + "\r\n" }, ErrInvalid},
		{"control chars in name", func(r *PairRequest) { r.DeviceName = "a\x1b[2Jb" }, ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			link, _ := f.newPairing()
			att, err := StartPairing(link, f.device, "phone", sdp(fpA), f.clk.now())
			if err != nil {
				t.Fatal(err)
			}
			r := att.Request()
			r.Sig = bytes.Clone(r.Sig)
			tc.mutate(r)
			if _, err := f.host.VerifyPairRequest(r); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			// A forged request must not burn the legitimate one.
			if tc.want != ErrUnknownSession {
				if _, err := f.host.VerifyPairRequest(att.Request()); err != nil {
					t.Fatalf("legitimate request after forgery: %v", err)
				}
			}
		})
	}
}

func TestMITMReSignedWithoutSecretFails(t *testing.T) {
	f := newFixture(t)
	link, _ := f.newPairing()
	att, _ := StartPairing(link, f.device, "phone", sdp(fpA), f.clk.now())
	// Attacker replaces SDP and identity, re-signs with own key, but lacks the secret.
	evil, _ := GenerateIdentity()
	wrong := link
	wrong.Secret = bytes.Repeat([]byte{9}, SecretSize)
	forged, _ := StartPairing(wrong, evil, "phone", sdp(fpB), f.clk.now())
	r := forged.Request()
	r.DeviceNonce = att.Request().DeviceNonce
	if _, err := f.host.VerifyPairRequest(r); !errors.Is(err, ErrBadMAC) {
		t.Fatalf("got %v", err)
	}
}

func TestPairAnswerTampering(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PairAnswer)
		want   error
	}{
		{"answer fingerprint replaced", func(a *PairAnswer) { a.AnswerSDP = sdp(fpA) }, ErrBadMAC},
		{"grant id replaced", func(a *PairAnswer) { a.GrantID = bytes.Repeat([]byte{1}, 16) }, ErrBadMAC},
		{"host key replaced", func(a *PairAnswer) { a.HostKey = bytes.Repeat([]byte{1}, 32) }, ErrHostKeyMismatch},
		{"request hash replaced", func(a *PairAnswer) { a.RequestHash = bytes.Repeat([]byte{1}, 32) }, ErrInvalid},
		{"signature broken", func(a *PairAnswer) { a.Sig[5] ^= 1 }, ErrBadSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			link, _ := f.newPairing()
			att, _ := StartPairing(link, f.device, "phone", sdp(fpA), f.clk.now())
			p, err := f.host.VerifyPairRequest(att.Request())
			if err != nil {
				t.Fatal(err)
			}
			if err := f.host.Approve(context.Background(), p, yes); err != nil {
				t.Fatal(err)
			}
			ans, _, err := f.host.CompletePairing(p, sdp(fpB))
			if err != nil {
				t.Fatal(err)
			}
			ans.Sig = bytes.Clone(ans.Sig)
			tc.mutate(ans)
			if _, err := att.VerifyAnswer(ans); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPairingReplayExpiryApproval(t *testing.T) {
	t.Run("replay after completion", func(t *testing.T) {
		f := newFixture(t)
		link, _ := f.newPairing()
		att, _ := StartPairing(link, f.device, "phone", sdp(fpA), f.clk.now())
		p, _ := f.host.VerifyPairRequest(att.Request())
		_ = f.host.Approve(context.Background(), p, yes)
		if _, _, err := f.host.CompletePairing(p, sdp(fpB)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.host.VerifyPairRequest(att.Request()); !errors.Is(err, ErrUnknownSession) {
			t.Fatalf("replay: %v", err)
		}
		if _, _, err := f.host.CompletePairing(p, sdp(fpB)); !errors.Is(err, ErrUnknownSession) {
			t.Fatalf("double complete: %v", err)
		}
	})
	t.Run("second request while pending", func(t *testing.T) {
		f := newFixture(t)
		link, _ := f.newPairing()
		att, _ := StartPairing(link, f.device, "phone", sdp(fpA), f.clk.now())
		if _, err := f.host.VerifyPairRequest(att.Request()); err != nil {
			t.Fatal(err)
		}
		if _, err := f.host.VerifyPairRequest(att.Request()); !errors.Is(err, ErrReplay) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("unapproved", func(t *testing.T) {
		f := newFixture(t)
		link, _ := f.newPairing()
		att, _ := StartPairing(link, f.device, "phone", sdp(fpA), f.clk.now())
		p, _ := f.host.VerifyPairRequest(att.Request())
		if _, _, err := f.host.CompletePairing(p, sdp(fpB)); !errors.Is(err, ErrNotApproved) {
			t.Fatalf("got %v", err)
		}
		if gs, _ := f.host.Grants().List(); len(gs) != 0 {
			t.Fatalf("grant persisted without approval: %+v", gs)
		}
	})
	t.Run("denied burns session", func(t *testing.T) {
		f := newFixture(t)
		link, _ := f.newPairing()
		att, _ := StartPairing(link, f.device, "phone", sdp(fpA), f.clk.now())
		p, _ := f.host.VerifyPairRequest(att.Request())
		if err := f.host.Approve(context.Background(), p, no); !errors.Is(err, ErrDenied) {
			t.Fatalf("got %v", err)
		}
		if _, _, err := f.host.CompletePairing(p, sdp(fpB)); !errors.Is(err, ErrUnknownSession) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("expired link", func(t *testing.T) {
		f := newFixture(t)
		link, _ := f.newPairing()
		att, _ := StartPairing(link, f.device, "phone", sdp(fpA), f.clk.now())
		f.clk.t = f.clk.t.Add(6 * time.Minute)
		if _, err := f.host.VerifyPairRequest(att.Request()); !errors.Is(err, ErrExpired) {
			t.Fatalf("got %v", err)
		}
		if _, err := StartPairing(link, f.device, "phone", sdp(fpA), f.clk.now()); !errors.Is(err, ErrExpired) {
			t.Fatalf("device accepted expired link: %v", err)
		}
	})
	t.Run("approval timeout", func(t *testing.T) {
		f := newFixture(t)
		link, _ := f.newPairing()
		att, _ := StartPairing(link, f.device, "phone", sdp(fpA), f.clk.now())
		p, _ := f.host.VerifyPairRequest(att.Request())
		slow := ApproverFunc(func(context.Context, PairingPrompt) (bool, error) {
			f.clk.t = f.clk.t.Add(10 * time.Minute)
			return true, nil
		})
		if err := f.host.Approve(context.Background(), p, slow); !errors.Is(err, ErrExpired) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestReconnectRejections(t *testing.T) {
	setup := func(t *testing.T) (*fixture, *DevicePairing, *ReconnectAttempt) {
		f := newFixture(t)
		res, _ := f.pair()
		ch, err := f.host.NewChallenge(NewReconnectHello(f.device, res.GrantID), res.BrokerSessionID)
		if err != nil {
			t.Fatal(err)
		}
		ra, err := StartReconnect(ch, res.HostKey, f.device, res.GrantID, sdp(fpA), f.clk.now())
		if err != nil {
			t.Fatal(err)
		}
		return f, res, ra
	}
	t.Run("revoked", func(t *testing.T) {
		f, res, ra := setup(t)
		if err := f.host.Grants().Revoke(B64(res.GrantID).String(), f.clk.now()); err != nil {
			t.Fatal(err)
		}
		if _, err := f.host.VerifyReconnect(ra.Request()); !errors.Is(err, ErrRevoked) {
			t.Fatalf("got %v", err)
		}
		if _, err := f.host.NewChallenge(NewReconnectHello(f.device, res.GrantID), res.BrokerSessionID); !errors.Is(err, ErrRevoked) {
			t.Fatalf("challenge for revoked grant: %v", err)
		}
	})
	t.Run("revoked between verify and answer", func(t *testing.T) {
		f, res, ra := setup(t)
		v, err := f.host.VerifyReconnect(ra.Request())
		if err != nil {
			t.Fatal(err)
		}
		_ = f.host.Grants().Revoke(B64(res.GrantID).String(), f.clk.now())
		if _, err := f.host.AnswerReconnect(v, sdp(fpB)); !errors.Is(err, ErrRevoked) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("grant expired", func(t *testing.T) {
		f, _, ra := setup(t)
		f.clk.t = f.clk.t.Add(25 * time.Hour)
		if _, err := f.host.VerifyReconnect(ra.Request()); !errors.Is(err, ErrExpired) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("challenge expired", func(t *testing.T) {
		f, _, ra := setup(t)
		f.clk.t = f.clk.t.Add(2 * time.Minute)
		if _, err := f.host.VerifyReconnect(ra.Request()); !errors.Is(err, ErrExpired) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("offer replaced", func(t *testing.T) {
		f, _, ra := setup(t)
		r := ra.Request()
		r.OfferSDP = sdp(fpB)
		if _, err := f.host.VerifyReconnect(r); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("got %v", err)
		}
		// Failed attempts still consume the challenge.
		if _, err := f.host.VerifyReconnect(ra.Request()); !errors.Is(err, ErrUnknownSession) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("other device key", func(t *testing.T) {
		f, res, _ := setup(t)
		evil, _ := GenerateIdentity()
		if _, err := f.host.NewChallenge(NewReconnectHello(evil, res.GrantID), res.BrokerSessionID); !errors.Is(err, ErrGrantNotFound) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("answer tampered", func(t *testing.T) {
		f, _, ra := setup(t)
		v, _ := f.host.VerifyReconnect(ra.Request())
		ans, _ := f.host.AnswerReconnect(v, sdp(fpB))
		ans.AnswerSDP = sdp(fpA)
		if _, err := ra.VerifyAnswer(ans); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("challenge from other host", func(t *testing.T) {
		f, res, _ := setup(t)
		other, err := NewHost(mustIdentity(t), f.host.Grants(), HostOptions{Now: f.clk.now})
		if err != nil {
			t.Fatal(err)
		}
		ch, err := other.NewChallenge(NewReconnectHello(f.device, res.GrantID), res.BrokerSessionID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := StartReconnect(ch, res.HostKey, f.device, res.GrantID, sdp(fpA), f.clk.now()); !errors.Is(err, ErrHostKeyMismatch) {
			t.Fatalf("got %v", err)
		}
	})
}

func mustIdentity(t *testing.T) *Identity {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestConcurrentCompleteConsumesOnce(t *testing.T) {
	f := newFixture(t)
	link, _ := f.newPairing()
	att, _ := StartPairing(link, f.device, "phone", sdp(fpA), f.clk.now())
	p, _ := f.host.VerifyPairRequest(att.Request())
	_ = f.host.Approve(context.Background(), p, yes)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := f.host.CompletePairing(p, sdp(fpB)); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	gs, _ := f.host.Grants().List()
	if ok != 1 || len(gs) != 1 {
		t.Fatalf("completions=%d grants=%d", ok, len(gs))
	}
}

func TestStorePersistenceAndPermissions(t *testing.T) {
	f := newFixture(t)
	_, g := f.pair()
	// Persistence across reopen.
	store, err := OpenGrantStore(filepath.Join(f.dir, "grants.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(g.ID)
	if err != nil || got.DeviceName != "Sam's phone" || !bytes.Equal(got.DeviceKey, f.device.PublicKey()) {
		t.Fatalf("reloaded grant %+v %v", got, err)
	}
	if err := store.Revoke(g.ID, f.clk.now()); err != nil {
		t.Fatal(err)
	}
	again, _ := OpenGrantStore(filepath.Join(f.dir, "grants.json"))
	if r, _ := again.Get(g.ID); r.Check(f.clk.now()) != ErrRevoked {
		t.Fatalf("revocation not persisted: %+v", r)
	}
	// Identity is stable.
	id1, _ := LoadOrCreateIdentity(filepath.Join(f.dir, "host.key"))
	if !bytes.Equal(id1.PublicKey(), f.host.Identity().PublicKey()) {
		t.Fatal("host identity changed across reload")
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, name := range []string{"grants.json", "host.key"} {
		p := filepath.Join(f.dir, name)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", name, info.Mode().Perm())
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := OpenGrantStore(filepath.Join(f.dir, "grants.json")); err == nil || !strings.Contains(err.Error(), "too open") {
		t.Fatalf("loose grant file accepted: %v", err)
	}
	if _, err := LoadOrCreateIdentity(filepath.Join(f.dir, "host.key")); err == nil {
		t.Fatal("loose identity file accepted")
	}
	nested := filepath.Join(f.dir, "new", "sub", "id.key")
	if _, err := LoadOrCreateIdentity(nested); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(filepath.Dir(nested)); info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", info.Mode().Perm())
	}
}

func TestTerminalApprover(t *testing.T) {
	prompt := PairingPrompt{DeviceName: "phone", DeviceFingerprint: "abcd", ExpiresAt: time.Now().Add(time.Minute)}
	for in, want := range map[string]bool{"yes\n": true, "YES\n": true, "y\n": false, "\n": false, "": false} {
		var out bytes.Buffer
		ok, err := TerminalApprover{In: strings.NewReader(in), Out: &out}.ApprovePairing(context.Background(), prompt)
		if err != nil || ok != want {
			t.Fatalf("input %q: %v %v", in, ok, err)
		}
		if !strings.Contains(out.String(), "abcd") {
			t.Fatalf("prompt lacks fingerprint: %q", out.String())
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	if ok, err := (TerminalApprover{In: r, Out: &bytes.Buffer{}}).ApprovePairing(ctx, prompt); ok || err == nil {
		t.Fatal("cancelled approval accepted")
	}
}

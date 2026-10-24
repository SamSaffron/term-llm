package pairing

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func brokerSID(t *testing.T) string {
	t.Helper()
	b := make([]byte, SessionIDSize)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	// Same encoding as peerbroker's randomToken.
	return base64.RawURLEncoding.EncodeToString(b)
}

// noncanonical sets an unused low bit in the final character of an unpadded
// base64url encoding of 32 bytes (whose last character carries 2 zero bits).
func noncanonical(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	i := strings.IndexByte(alphabet, s[len(s)-1])
	return s[:len(s)-1] + string(alphabet[i|1])
}

const testCap = "Zm9vYmFyLWRldmljZS1jYXBhYmlsaXR5LXRlc3Q"

func TestLinkSecretsOnlyInFragment(t *testing.T) {
	f := newFixture(t)
	link, err := f.host.NewPairingForSession(brokerSID(t), testCap, f.clk.now().Add(mailboxTTL))
	if err != nil {
		t.Fatal(err)
	}
	s := link.String()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	ps := B64(link.Secret).String()
	if strings.Contains(u.RawQuery, ps) || strings.Contains(u.RawQuery, testCap) ||
		u.Query().Has("ps") || u.Query().Has("cap") {
		t.Fatalf("secret material in query: %q", u.RawQuery)
	}
	if u.Fragment != "ps="+ps+"&cap="+testCap {
		t.Fatalf("fragment %q", u.Fragment)
	}
	red := link.Redacted()
	if strings.Contains(red, ps) || strings.Contains(red, testCap) || strings.Contains(red, "#") {
		t.Fatalf("redacted link leaks: %q", red)
	}
	got, err := ParseLink(s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Secret, link.Secret) || got.Capability != testCap || !got.ExpiresAt.Equal(link.ExpiresAt) ||
		got.Broker != link.Broker || !bytes.Equal(got.HostKey, link.HostKey) || got.BrokerSessionID() != link.BrokerSessionID() {
		t.Fatalf("round trip %+v", got)
	}
}

func TestLinkStrictParsing(t *testing.T) {
	f := newFixture(t)
	link, err := f.host.NewPairingForSession(brokerSID(t), testCap, f.clk.now().Add(mailboxTTL))
	if err != nil {
		t.Fatal(err)
	}
	s := link.String()
	q, frag, _ := strings.Cut(s, "#")
	ps := B64(link.Secret).String()
	cases := map[string]string{
		"psk in query":         strings.Replace(q, "&exp=", "&ps="+ps+"&exp=", 1) + "#" + frag,
		"psk only in query":    q + "&ps=" + ps,
		"cap in query":         strings.Replace(q, "&exp=", "&cap="+testCap+"&exp=", 1) + "#ps=" + ps,
		"no fragment":          q,
		"empty fragment":       q + "#",
		"unknown query param":  q + "&x=1#" + frag,
		"unknown frag param":   s + "&x=1",
		"duplicate sid":        strings.Replace(q, "&hk=", "&sid="+link.BrokerSessionID()+"&hk=", 1) + "#" + frag,
		"duplicate ps":         s + "&ps=" + ps,
		"duplicate cap":        s + "&cap=" + testCap,
		"empty value":          strings.Replace(s, "&cap="+testCap, "&cap=", 1),
		"bare key":             s + "&cap",
		"empty segment":        strings.Replace(s, "#ps=", "#&ps=", 1),
		"missing hn":           strings.Replace(s, "&hn="+B64(link.HostNonce).String(), "", 1),
		"version 2":            strings.Replace(s, "://v1", "://v2", 1),
		"wrong scheme":         strings.Replace(s, "term-llm-pair", "https", 1),
		"path":                 strings.Replace(s, "://v1?", "://v1/x?", 1),
		"userinfo":             strings.Replace(s, "://v1", "://u@v1", 1),
		"short ps":             strings.Replace(s, "#ps=", "#ps=AA", 1),
		"noncanonical b64":     strings.Replace(s, "#ps="+ps, "#ps="+noncanonical(ps), 1),
		"bad cap char":         strings.Replace(s, "&cap="+testCap, "&cap=a.b", 1),
		"javascript broker":    strings.Replace(s, "broker=https", "broker=javascript", 1),
		"broker with userinfo": strings.Replace(s, "broker=https%3A%2F%2F", "broker=https%3A%2F%2Fu%40", 1),
		"exp leading zero":     strings.Replace(s, "&exp=", "&exp=0", 1),
		"too long":             s + "&cap=" + strings.Repeat("a", MaxLinkBytes),
	}
	for name, bad := range cases {
		if bad == s {
			t.Fatalf("%s: case did not modify link", name)
		}
		if _, err := ParseLink(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: accepted or wrong error (%v): %q", name, err, bad)
		}
	}
	// cap is optional.
	if l, err := ParseLink(q + "#ps=" + ps); err != nil || l.Capability != "" {
		t.Fatalf("link without cap: %v", err)
	}
}

func TestNewPairingForSession(t *testing.T) {
	t.Run("sid preserved exactly and pairing works", func(t *testing.T) {
		f := newFixture(t)
		sid := brokerSID(t)
		link, err := f.host.NewPairingForSession(sid, testCap, f.clk.now().Add(30*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(link.String(), "&sid="+sid+"&") || link.BrokerSessionID() != sid {
			t.Fatalf("sid not preserved: %s", link.Redacted())
		}
		parsed, _ := ParseLink(link.String())
		att, err := StartPairing(parsed, f.device, "phone", sdp(fpA), f.clk.now())
		if err != nil {
			t.Fatal(err)
		}
		if B64(att.Request().SessionID).String() != sid {
			t.Fatal("request sid differs from broker sid")
		}
		p, err := f.host.VerifyPairRequest(roundTrip(t, att.Request()))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.host.Approve(context.Background(), p, yes); err != nil {
			t.Fatal(err)
		}
		ans, g, err := f.host.CompletePairing(p, sdp(fpB))
		if err != nil {
			t.Fatal(err)
		}
		if g.Scope != ScopeHubChat {
			t.Fatalf("scope %q", g.Scope)
		}
		if _, err := att.VerifyAnswer(roundTrip(t, ans)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rejects bad broker ids and caps", func(t *testing.T) {
		f := newFixture(t)
		for _, sid := range []string{"", "short", brokerSID(t) + "AA", "AAAAAAAAAAAAAAAAAAAAAA==", "AAAAAAAAAAAAAAAAAAAAAB", "../../AAAAAAAAAAAAAAAAA"} {
			if _, err := f.host.NewPairingForSession(sid, "", f.clk.now().Add(mailboxTTL)); !errors.Is(err, ErrInvalid) {
				t.Errorf("sid %q: %v", sid, err)
			}
		}
		if _, err := f.host.NewPairingForSession(brokerSID(t), "has space", f.clk.now().Add(mailboxTTL)); !errors.Is(err, ErrInvalid) {
			t.Errorf("bad cap: %v", err)
		}
	})
	t.Run("duplicate session id", func(t *testing.T) {
		f := newFixture(t)
		sid := brokerSID(t)
		if _, err := f.host.NewPairingForSession(sid, "", f.clk.now().Add(mailboxTTL)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.host.NewPairingForSession(sid, "", f.clk.now().Add(mailboxTTL)); !errors.Is(err, ErrReplay) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("broker expiry caps link and approval", func(t *testing.T) {
		f := newFixture(t)
		brokerExp := f.clk.now().Add(90 * time.Second)
		link, err := f.host.NewPairingForSession(brokerSID(t), "", brokerExp)
		if err != nil {
			t.Fatal(err)
		}
		if !link.ExpiresAt.Equal(brokerExp) {
			t.Fatalf("link expiry %v, want broker %v", link.ExpiresAt, brokerExp)
		}
		att, _ := StartPairing(link, f.device, "phone", sdp(fpA), f.clk.now())
		p, err := f.host.VerifyPairRequest(att.Request())
		if err != nil {
			t.Fatal(err)
		}
		f.clk.t = brokerExp // ApprovalTTL would allow 2m, broker only 90s
		if err := f.host.Approve(context.Background(), p, yes); !errors.Is(err, ErrExpired) {
			t.Fatalf("approve after broker expiry: %v", err)
		}
		if _, err := f.host.NewPairingForSession(brokerSID(t), "", f.clk.now()); !errors.Is(err, ErrExpired) {
			t.Fatalf("already-expired broker session: %v", err)
		}
	})
}

func TestApproveRevocationAndScope(t *testing.T) {
	t.Run("cancel between approve and complete", func(t *testing.T) {
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
		f.host.CancelPairing(p.SessionID)
		if _, _, err := f.host.CompletePairing(p, sdp(fpB)); !errors.Is(err, ErrUnknownSession) {
			t.Fatalf("got %v", err)
		}
		if gs, _ := f.host.Grants().List(); len(gs) != 0 {
			t.Fatalf("grant persisted after cancel: %+v", gs)
		}
	})
	t.Run("live authorize sees revocation", func(t *testing.T) {
		f := newFixture(t)
		res, g := f.pair()
		if _, err := f.host.Grants().Authorize(g.ID, f.device.PublicKey(), f.clk.now()); err != nil {
			t.Fatal(err)
		}
		// Revocation through a separately opened store (another process).
		other, err := OpenGrantStore(f.host.Grants().Path())
		if err != nil {
			t.Fatal(err)
		}
		if err := other.Revoke(g.ID, f.clk.now()); err != nil {
			t.Fatal(err)
		}
		if _, err := f.host.Grants().Authorize(g.ID, f.device.PublicKey(), f.clk.now()); !errors.Is(err, ErrRevoked) {
			t.Fatalf("got %v", err)
		}
		if _, err := f.host.NewChallenge(NewReconnectHello(f.device, res.GrantID), res.BrokerSessionID); !errors.Is(err, ErrRevoked) {
			t.Fatalf("challenge after revoke: %v", err)
		}
	})
	t.Run("revoked after challenge issued", func(t *testing.T) {
		f := newFixture(t)
		res, g := f.pair()
		ch, err := f.host.NewChallenge(NewReconnectHello(f.device, res.GrantID), res.BrokerSessionID)
		if err != nil {
			t.Fatal(err)
		}
		ra, err := StartReconnect(ch, res.HostKey, f.device, res.GrantID, sdp(fpA), f.clk.now())
		if err != nil {
			t.Fatal(err)
		}
		_ = f.host.Grants().Revoke(g.ID, f.clk.now())
		if _, err := f.host.VerifyReconnect(ra.Request()); !errors.Is(err, ErrRevoked) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("unknown scope or missing expiry rejected", func(t *testing.T) {
		f := newFixture(t)
		_, g := f.pair()
		bad := g
		bad.ID = B64(bytes.Repeat([]byte{9}, GrantIDSize)).String()
		bad.Scope = "hub-admin"
		if err := f.host.Grants().Add(bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("add foreign scope: %v", err)
		}
		bad.Scope, bad.ExpiresAt = ScopeHubChat, time.Time{}
		if err := f.host.Grants().Add(bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("add non-expiring: %v", err)
		}
		// A tampered file entry fails closed too.
		if err := f.host.Grants().update(func(gf *grantFile) error {
			gf.Grants[0].Scope = "hub-admin"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.host.Grants().Authorize(g.ID, f.device.PublicKey(), f.clk.now()); !errors.Is(err, ErrInvalid) {
			t.Fatalf("authorize foreign scope: %v", err)
		}
	})
}

func TestPilotLifetimes(t *testing.T) {
	t.Run("grant defaults to 24h and must expire", func(t *testing.T) {
		f := newFixture(t)
		res, g := f.pair()
		if want := f.clk.now().Add(DefaultGrantTTL); !g.ExpiresAt.Equal(want) || !res.GrantExpiresAt.Equal(want) {
			t.Fatalf("grant expiry %v / device %v, want %v", g.ExpiresAt, res.GrantExpiresAt, want)
		}
		f.clk.t = f.clk.t.Add(DefaultGrantTTL)
		if _, err := f.host.Grants().Authorize(g.ID, f.device.PublicKey(), f.clk.now()); !errors.Is(err, ErrExpired) {
			t.Fatalf("got %v", err)
		}
		if _, err := NewHost(f.host.Identity(), f.host.Grants(), HostOptions{GrantTTL: -1}); err == nil {
			t.Fatal("negative GrantTTL accepted")
		}
	})
	t.Run("device rejects non-expiring grant", func(t *testing.T) {
		f := newFixture(t)
		link, _ := f.newPairing()
		att, _ := StartPairing(link, f.device, "phone", sdp(fpA), f.clk.now())
		p, _ := f.host.VerifyPairRequest(att.Request())
		_ = f.host.Approve(context.Background(), p, yes)
		ans, _, err := f.host.CompletePairing(p, sdp(fpB))
		if err != nil {
			t.Fatal(err)
		}
		ans.GrantExpiresAt = 0
		if _, err := att.VerifyAnswer(ans); !errors.Is(err, ErrInvalid) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("reconnect only via original live mailbox", func(t *testing.T) {
		f := newFixture(t)
		res, g := f.pair()
		if g.MailboxID != res.BrokerSessionID || !g.MailboxExpiresAt.Equal(f.clk.now().Add(mailboxTTL)) {
			t.Fatalf("grant mailbox %q %v", g.MailboxID, g.MailboxExpiresAt)
		}
		hello := NewReconnectHello(f.device, res.GrantID)
		if _, err := f.host.NewChallenge(hello, brokerSID(t)); !errors.Is(err, ErrReconnectUnavailable) {
			t.Fatalf("other mailbox: %v", err)
		}
		// Near the end of the mailbox the challenge is capped at its expiry.
		f.clk.t = g.MailboxExpiresAt.Add(-10 * time.Second)
		ch, err := f.host.NewChallenge(hello, res.BrokerSessionID)
		if err != nil {
			t.Fatal(err)
		}
		if ch.ExpiresAt != g.MailboxExpiresAt.Unix() {
			t.Fatalf("challenge expiry %d, mailbox %d", ch.ExpiresAt, g.MailboxExpiresAt.Unix())
		}
		ra, err := StartReconnect(ch, res.HostKey, f.device, res.GrantID, sdp(fpA), f.clk.now())
		if err != nil {
			t.Fatal(err)
		}
		// Mailbox expires before the request arrives: fail closed.
		f.clk.t = g.MailboxExpiresAt
		if _, err := f.host.VerifyReconnect(ra.Request()); !errors.Is(err, ErrExpired) {
			t.Fatalf("request after mailbox expiry: %v", err)
		}
		// Grant and identities are still valid, but discovery is unavailable;
		// this also holds for a restarted host using the same persisted state.
		if _, err := f.host.Grants().Authorize(g.ID, f.device.PublicKey(), f.clk.now()); err != nil {
			t.Fatalf("grant should remain valid: %v", err)
		}
		if _, err := f.host.NewChallenge(hello, res.BrokerSessionID); !errors.Is(err, ErrReconnectUnavailable) {
			t.Fatalf("after mailbox expiry: %v", err)
		}
		restartedID, err := LoadOrCreateIdentity(filepath.Join(f.dir, "host.key"))
		if err != nil {
			t.Fatal(err)
		}
		store, _ := OpenGrantStore(f.host.Grants().Path())
		restarted, err := NewHost(restartedID, store, HostOptions{Broker: "https://broker.example", Now: f.clk.now})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := restarted.NewChallenge(hello, res.BrokerSessionID); !errors.Is(err, ErrReconnectUnavailable) {
			t.Fatalf("restarted host after mailbox expiry: %v", err)
		}
	})
	t.Run("pairing requires broker expiry", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.host.NewPairingForSession(brokerSID(t), "", time.Time{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("got %v", err)
		}
	})
}

package passkeyauth

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

func TestSessionsExpirationRevocationAndRecentAuth(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sessions := NewSessions(func() time.Time { return now }, nil)
	a, err := sessions.Create("cred-a")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := sessions.Create("cred-b")
	p, err := sessions.Authenticate(a.Token)
	if err != nil {
		t.Fatal(err)
	}
	if sessions.Count() != 2 {
		t.Fatal("count")
	}
	if err := sessions.GrantRecentAuth(p); err != nil {
		t.Fatal(err)
	}
	if err := sessions.ConsumeRecentAuth(p); err != nil {
		t.Fatal(err)
	}
	if err := sessions.ConsumeRecentAuth(p); !errors.Is(err, ErrRecentAuthRequired) {
		t.Fatal(err)
	}
	if n := sessions.RevokeCredential("cred-b"); n != 1 {
		t.Fatal(n)
	}
	if _, err := sessions.Authenticate(b.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal(err)
	}
	restarted := NewSessions(func() time.Time { return now }, nil)
	if _, err := restarted.Authenticate(a.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal("session survived restart")
	}
	now = now.Add(SessionIdleLifetime + time.Second)
	if _, err := sessions.Authenticate(a.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal(err)
	}
}
func TestSessionLogoutAndRevokeOthers(t *testing.T) {
	sessions := NewSessions(nil, nil)
	current, _ := sessions.Create("a")
	other, _ := sessions.Create("b")
	third, _ := sessions.Create("c")
	principal, err := sessions.Authenticate(current.Token)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := sessions.RevokeOthers(principal)
	if err != nil || revoked != 2 {
		t.Fatalf("revoked=%d err=%v", revoked, err)
	}
	for _, token := range []string{other.Token, third.Token} {
		if _, err := sessions.Authenticate(token); !errors.Is(err, ErrInvalidSession) {
			t.Fatal("other session survived")
		}
	}
	if err := sessions.Logout(principal); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Authenticate(current.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal("logout did not revoke current")
	}
}

func TestSessionAbsoluteExpirationDespiteActivity(t *testing.T) {
	now := time.Now()
	s := NewSessions(func() time.Time { return now }, nil)
	issued, err := s.Create("credential")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		now = now.Add(11 * time.Hour)
		_, err = s.Authenticate(issued.Token)
		if now.Before(issued.Info.AbsoluteExpiresAt) && err != nil {
			t.Fatalf("early expiry at %v: %v", now, err)
		}
	}
	if _, err = s.Authenticate(issued.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal("session exceeded absolute lifetime")
	}
}

func TestSessionCapacityDoesNotEvict(t *testing.T) {
	s := NewSessions(nil, nil)
	s.max = 1
	first, err := s.Create("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("b"); !errors.Is(err, ErrSessionCapacity) {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(first.Token); err != nil {
		t.Fatal("valid session evicted")
	}
}
func TestCeremonyCapacityPerPeer(t *testing.T) {
	c := NewCeremonies(nil, nil)
	for i := 0; i < MaxCeremoniesPerPeer; i++ {
		if _, err := c.Create(CeremonyLogin, "192.0.2.1:1", "", "", "", nil, webauthnSession()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Create(CeremonyLogin, "192.0.2.1:2", "", "", "", nil, webauthnSession()); !errors.Is(err, ErrCapacity) {
		t.Fatalf("err=%v", err)
	}
}

func TestCeremonyGlobalCapacityAndExpiredCleanup(t *testing.T) {
	now := time.Now()
	c := NewCeremonies(func() time.Time { return now }, nil)
	var first Ceremony
	for i := 0; i < MaxCeremonies; i++ {
		created, err := c.Create(CeremonyLogin, fmt.Sprintf("192.0.2.%d", i), "", "", "", nil, webauthnSession())
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = created
		}
	}
	if _, err := c.Create(CeremonyLogin, "overflow", "", "", "", nil, webauthnSession()); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	now = now.Add(CeremonyLifetime)
	if _, err := c.Create(CeremonyLogin, "after-expiry", "", "", "", nil, webauthnSession()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Consume(first.CookieToken, CeremonyLogin, "", ""); !errors.Is(err, ErrInvalidCeremony) {
		t.Fatal("expired ceremony survived cleanup")
	}
}

func TestHostGrantSingleUseAndExpiry(t *testing.T) {
	now := time.Now()
	g, err := NewGrants(GrantBootstrap, []byte("abcdefghijklmnopqrst"), func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Verify([]byte("wrong-wrong-wrong-wrong")); !errors.Is(err, ErrInvalidGrant) {
		t.Fatal(err)
	}
	grant, err := g.Verify([]byte("abcdefghijklmnopqrst"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Verify([]byte("abcdefghijklmnopqrst")); !errors.Is(err, ErrInvalidGrant) {
		t.Fatal(err)
	}
	if _, err := g.Authenticate(grant.Token); err != nil {
		t.Fatal(err)
	}
	if err := g.Consume(grant.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Authenticate(grant.Token); !errors.Is(err, ErrInvalidGrant) {
		t.Fatal(err)
	}
}
func TestGrantAndCeremonyExpiry(t *testing.T) {
	now := time.Now()
	g, err := NewGrants(GrantRecovery, []byte("abcdefghijklmnopqrst"), func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(HostGrantLifetime)
	if _, err := g.Verify([]byte("abcdefghijklmnopqrst")); !errors.Is(err, ErrInvalidGrant) {
		t.Fatal(err)
	}
	now = time.Now()
	c := NewCeremonies(func() time.Time { return now }, nil)
	created, err := c.Create(CeremonyLogin, "peer", "", "", "", nil, webauthnSession())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(CeremonyLifetime)
	if _, err := c.Consume(created.CookieToken, CeremonyLogin, "", ""); !errors.Is(err, ErrInvalidCeremony) {
		t.Fatal(err)
	}
}

func TestCeremonyBindingAndReplay(t *testing.T) {
	c := NewCeremonies(nil, nil)
	created, err := c.Create(CeremonyLogin, "127.0.0.1:1", "", "session-a", "", nil, webauthnSession())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Consume(created.CookieToken, CeremonyReauth, "", "session-a"); !errors.Is(err, ErrInvalidCeremony) {
		t.Fatal(err)
	}
	if _, err := c.Consume(created.CookieToken, CeremonyLogin, "", "session-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Consume(created.CookieToken, CeremonyLogin, "", "session-a"); !errors.Is(err, ErrInvalidCeremony) {
		t.Fatal(err)
	}
}
func webauthnSession() (s webauthn.SessionData) { return s }

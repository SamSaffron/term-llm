package passkeyauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	if n, err := sessions.RevokeCredential("cred-b"); err != nil || n != 1 {
		t.Fatalf("revoked=%d err=%v", n, err)
	}
	if _, err := sessions.Authenticate(b.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal(err)
	}
	restarted := NewSessions(func() time.Time { return now }, nil)
	if _, err := restarted.Authenticate(a.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal("in-memory session survived restart")
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
	wrongPrincipal := principal
	wrongPrincipal.CredentialRecordID = "wrong"
	if _, err := sessions.Info(wrongPrincipal); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("mismatched principal accepted: %v", err)
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
		t.Fatalf("err=%v", err)
	}
	if _, err := s.Authenticate(first.Token); err != nil {
		t.Fatal("valid session evicted")
	}
}

func acceptAnyCredential(string) bool { return true }

func TestDurableSessionsSurviveRestartWithoutPersistingRawTokenOrRecentAuth(t *testing.T) {
	now := time.Date(2026, 8, 26, 1, 2, 3, 0, time.UTC)
	path := filepath.Join(privateTempDir(t), "sessions.json")
	opts := SessionsOptions{Path: path, RPID: "hub.example", UserID: "user-handle", Now: func() time.Time { return now }, ValidCredential: func(id string) bool { return id == "cred-a" }}
	sessions, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sessions.Create("cred-a")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := sessions.Authenticate(issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.GrantRecentAuth(principal); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), issued.Token) || !strings.Contains(string(raw), `"token_hash"`) {
		t.Fatalf("session store leaked raw token or omitted hash: %s", raw)
	}
	if err := sessions.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	restartedPrincipal, err := restarted.Authenticate(issued.Token)
	if err != nil {
		t.Fatalf("durable session did not survive restart: %v", err)
	}
	if restartedPrincipal != principal {
		t.Fatalf("principal changed: got %+v want %+v", restartedPrincipal, principal)
	}
	if restarted.HasRecentAuth(restartedPrincipal) {
		t.Fatal("recent authentication survived restart")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode=%04o", info.Mode().Perm())
		}
	}
}

func TestDurableSessionActivityCheckpointInterval(t *testing.T) {
	now := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	path := filepath.Join(privateTempDir(t), "sessions.json")
	opts := SessionsOptions{Path: path, RPID: "hub.example", UserID: "user-handle", ValidCredential: acceptAnyCredential, Now: func() time.Time { return now }}
	sessions, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sessions.Create("cred-a")
	if err != nil {
		t.Fatal(err)
	}
	persistedLastSeen := func() time.Time {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var saved sessionStoreFile
		if err := json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
		return saved.Sessions[0].LastSeenAt
	}
	createdAt := persistedLastSeen()
	now = now.Add(SessionActivityWriteInterval - time.Second)
	if _, err := sessions.Authenticate(issued.Token); err != nil {
		t.Fatal(err)
	}
	if got := persistedLastSeen(); !got.Equal(createdAt) {
		t.Fatalf("activity checkpointed too early: %v", got)
	}
	now = now.Add(2 * time.Second)
	if _, err := sessions.Authenticate(issued.Token); err != nil {
		t.Fatal(err)
	}
	if got := persistedLastSeen(); !got.Equal(now) {
		t.Fatalf("activity checkpoint = %v, want %v", got, now)
	}
	if err := sessions.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	info, err := restarted.Info(Principal{SessionID: issued.Info.ID, CredentialRecordID: "cred-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !info.LastSeenAt.Equal(now) || !info.IdleExpiresAt.Equal(now.Add(SessionIdleLifetime)) {
		t.Fatalf("restarted info = %+v", info)
	}
}

func TestDurableSessionRevocationsSurviveRestart(t *testing.T) {
	now := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	path := filepath.Join(privateTempDir(t), "sessions.json")
	opts := SessionsOptions{Path: path, RPID: "hub.example", UserID: "user-handle", ValidCredential: acceptAnyCredential, Now: func() time.Time { return now }}
	sessions, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := sessions.Create("cred-a")
	other, _ := sessions.Create("cred-b")
	credentialSession, _ := sessions.Create("cred-c")
	principal, _ := sessions.Authenticate(current.Token)
	if n, err := sessions.RevokeOthers(principal); err != nil || n != 2 {
		t.Fatalf("revoke others=%d err=%v", n, err)
	}
	if err := sessions.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{other.Token, credentialSession.Token} {
		if _, err := restarted.Authenticate(token); !errors.Is(err, ErrInvalidSession) {
			t.Fatalf("revoked session survived restart: %v", err)
		}
	}
	principal, err = restarted.Authenticate(current.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Logout(principal); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err = OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Authenticate(current.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("logged-out session survived restart: %v", err)
	}
}

func TestDurableSessionCredentialRevocationAndPruning(t *testing.T) {
	now := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	path := filepath.Join(privateTempDir(t), "sessions.json")
	valid := map[string]bool{"cred-a": true, "cred-b": true}
	opts := SessionsOptions{Path: path, RPID: "hub.example", UserID: "user-handle", Now: func() time.Time { return now }, ValidCredential: func(id string) bool { return valid[id] }}
	sessions, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := sessions.Create("cred-a")
	b, _ := sessions.Create("cred-b")
	if n, err := sessions.RevokeCredential("cred-b"); err != nil || n != 1 {
		t.Fatalf("revoke credential=%d err=%v", n, err)
	}
	if err := sessions.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Authenticate(b.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("credential session survived restart: %v", err)
	}
	valid["cred-a"] = false
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err = OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Count() != 0 {
		t.Fatal("session for missing credential was not pruned")
	}
	if _, err := restarted.Authenticate(a.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("missing-credential session authenticated: %v", err)
	}
}

func TestDurableSessionWriteFailureFailsClosed(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "sessions.json")
	opts := SessionsOptions{Path: path, RPID: "hub.example", UserID: "user-handle", ValidCredential: acceptAnyCredential}
	sessions, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sessions.Create("cred-a")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := sessions.Authenticate(issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Close(); err != nil {
		t.Fatal(err)
	}
	failing, err := OpenSessions(SessionsOptions{
		Path:            path,
		RPID:            "hub.example",
		UserID:          "user-handle",
		ValidCredential: acceptAnyCredential,
		WriteFile:       func(string, []byte, os.FileMode) error { return errors.New("injected write failure") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := failing.Logout(principal); err == nil {
		t.Fatal("expected logout persistence error")
	}
	if _, err := failing.Authenticate(issued.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("failed logout did not fail closed: %v", err)
	}
	if err := failing.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Authenticate(issued.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("revoked session survived failed write and restart: %v", err)
	}
}

func TestDurableSessionStoreExclusiveLock(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "sessions.json")
	opts := SessionsOptions{Path: path, RPID: "hub.example", UserID: "user-handle", ValidCredential: acceptAnyCredential}
	first, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSessions(opts); err == nil {
		t.Fatal("second session store instance acquired the same file")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenSessions(opts)
	if err != nil {
		t.Fatalf("session store remained locked after close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDurableSessionCheckpointFailureBacksOff(t *testing.T) {
	now := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	path := filepath.Join(privateTempDir(t), "sessions.json")
	opts := SessionsOptions{Path: path, RPID: "hub.example", UserID: "user-handle", ValidCredential: acceptAnyCredential, Now: func() time.Time { return now }}
	sessions, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sessions.Create("cred-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Close(); err != nil {
		t.Fatal(err)
	}
	warnings, writes := 0, 0
	failing, err := OpenSessions(SessionsOptions{
		Path:            path,
		RPID:            "hub.example",
		UserID:          "user-handle",
		ValidCredential: acceptAnyCredential,
		Now:             func() time.Time { return now },
		Warnf:           func(string, ...any) { warnings++ },
		WriteFile: func(string, []byte, os.FileMode) error {
			writes++
			return errors.New("injected checkpoint failure")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(SessionActivityWriteInterval)
	if _, err := failing.Authenticate(issued.Token); err != nil {
		t.Fatalf("checkpoint failure rejected valid session: %v", err)
	}
	if _, err := failing.Authenticate(issued.Token); err != nil {
		t.Fatalf("checkpoint backoff rejected valid session: %v", err)
	}
	if warnings != 1 || writes != 1 {
		t.Fatalf("checkpoint failure warnings=%d writes=%d, want 1 each", warnings, writes)
	}
}

func TestDurableSessionCreateWriteFailureLeavesNoSession(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "sessions.json")
	opts := SessionsOptions{Path: path, RPID: "hub.example", UserID: "user-handle", ValidCredential: acceptAnyCredential}
	sessions, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Close(); err != nil {
		t.Fatal(err)
	}
	failing, err := OpenSessions(SessionsOptions{
		Path:            path,
		RPID:            "hub.example",
		UserID:          "user-handle",
		ValidCredential: acceptAnyCredential,
		WriteFile:       func(string, []byte, os.FileMode) error { return errors.New("injected write failure") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.Create("cred-a"); err == nil {
		t.Fatal("session creation succeeded despite persistence failure")
	}
	if failing.Count() != 0 {
		t.Fatal("failed session creation changed in-memory state")
	}
	if err := failing.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Count() != 0 {
		t.Fatal("failed session creation changed durable state")
	}
}

func TestDurableSessionExpiryIsPrunedFromDisk(t *testing.T) {
	now := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	path := filepath.Join(privateTempDir(t), "sessions.json")
	opts := SessionsOptions{Path: path, RPID: "hub.example", UserID: "user-handle", ValidCredential: acceptAnyCredential, Now: func() time.Time { return now }}
	sessions, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sessions.Create("cred-a")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(SessionIdleLifetime + time.Second)
	if err := sessions.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Count() != 0 {
		t.Fatal("expired session was not pruned")
	}
	if _, err := restarted.Authenticate(issued.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("expired session authenticated: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved sessionStoreFile
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Sessions) != 0 {
		t.Fatalf("expired sessions remain on disk: %+v", saved.Sessions)
	}
}

func TestDurableSessionStoreHandlesCorruptionAndRejectsIdentitySymlinkAndMode(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "sessions.json")
	opts := SessionsOptions{Path: path, RPID: "hub.example", UserID: "user-handle", ValidCredential: acceptAnyCredential}
	sessions, err := OpenSessions(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSessions(SessionsOptions{Path: path, RPID: "other.example", UserID: "user-handle", ValidCredential: acceptAnyCredential}); err == nil || !strings.Contains(err.Error(), "different Hub identity") {
		t.Fatalf("identity mismatch error = %v", err)
	}
	if _, err := OpenSessions(SessionsOptions{Path: path, RPID: "hub.example", UserID: "other-user", ValidCredential: acceptAnyCredential}); err == nil || !strings.Contains(err.Error(), "different Hub identity") {
		t.Fatalf("user mismatch error = %v", err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	warnings := 0
	reset, err := OpenSessions(SessionsOptions{Path: path, RPID: "hub.example", UserID: "user-handle", ValidCredential: acceptAnyCredential, Warnf: func(string, ...any) { warnings++ }})
	if err != nil {
		t.Fatalf("corrupt disposable session store prevented startup: %v", err)
	}
	if warnings != 1 || reset.Count() != 0 {
		t.Fatalf("corrupt reset warnings=%d sessions=%d", warnings, reset.Count())
	}
	if err := reset.Close(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		unsafeParent := t.TempDir()
		if err := os.Chmod(unsafeParent, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenSessions(SessionsOptions{Path: filepath.Join(unsafeParent, "sessions.json"), RPID: "hub.example", UserID: "user-handle", ValidCredential: acceptAnyCredential}); err == nil {
			t.Fatal("accepted insecure session store parent")
		}
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err == nil {
			if _, err := OpenSessions(SessionsOptions{Path: link, RPID: "hub.example", UserID: "user-handle", ValidCredential: acceptAnyCredential}); err == nil {
				t.Fatal("accepted symlink session store")
			}
		}
		secure := filepath.Join(dir, "insecure.json")
		opened, err := OpenSessions(SessionsOptions{Path: secure, RPID: "hub.example", UserID: "user-handle", ValidCredential: acceptAnyCredential})
		if err != nil {
			t.Fatal(err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(secure, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenSessions(SessionsOptions{Path: secure, RPID: "hub.example", UserID: "user-handle", ValidCredential: acceptAnyCredential}); err == nil {
			t.Fatal("accepted insecure session store mode")
		}
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

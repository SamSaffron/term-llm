package proxy

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t testing.TB) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestClientCRUD(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	c, err := st.CreateClient(ctx, "acme", "Acme Corp")
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	if c.ID == "" || c.Name != "acme" || c.Description != "Acme Corp" {
		t.Fatalf("unexpected client: %+v", c)
	}

	got, err := st.GetClient(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	if got.Name != "acme" {
		t.Fatalf("GetClient name = %q", got.Name)
	}

	if err := st.SetClientDisabled(ctx, c.ID, true); err != nil {
		t.Fatalf("SetClientDisabled: %v", err)
	}
	got, _ = st.GetClient(ctx, c.ID)
	if !got.Disabled {
		t.Fatal("expected client disabled")
	}

	list, err := st.ListClients(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListClients = %v, %v", list, err)
	}

	if err := st.DeleteClient(ctx, c.ID); err != nil {
		t.Fatalf("DeleteClient: %v", err)
	}
	if _, err := st.GetClient(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := st.CreateClient(ctx, "  ", ""); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	c, _ := st.CreateClient(ctx, "acme", "")

	plaintext, tok, err := st.CreateToken(ctx, c.ID, "ci token", time.Hour)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if plaintext == "" || tok.Hash == HashToken("") {
		t.Fatal("expected non-empty plaintext/hash")
	}
	if tok.Prefix != TokenDisplayPrefix(plaintext) {
		t.Fatalf("prefix mismatch: %q vs %q", tok.Prefix, TokenDisplayPrefix(plaintext))
	}

	// Authenticate succeeds and updates last_used_at.
	gotClient, gotTok, err := st.AuthenticateToken(ctx, plaintext)
	if err != nil {
		t.Fatalf("AuthenticateToken: %v", err)
	}
	if gotClient.ID != c.ID {
		t.Fatalf("authenticated wrong client: %s", gotClient.ID)
	}
	if gotTok.LastUsedAt == nil {
		t.Fatal("expected last_used_at to be set")
	}

	// Wrong token is rejected.
	if _, _, err := st.AuthenticateToken(ctx, "tlp_wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for bad token, got %v", err)
	}

	// Listing never leaks the secret.
	toks, err := st.ListTokens(ctx, c.ID)
	if err != nil || len(toks) != 1 {
		t.Fatalf("ListTokens = %v, %v", toks, err)
	}
	if toks[0].Hash != "" {
		t.Fatal("ListTokens must not expose hash")
	}

	// Revoked token no longer authenticates.
	if err := st.RevokeToken(ctx, tok.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, _, err := st.AuthenticateToken(ctx, plaintext); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected revoked token rejected, got %v", err)
	}
}

func TestTokenExpiry(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return now })
	c, _ := st.CreateClient(ctx, "acme", "")

	plaintext, _, err := st.CreateToken(ctx, c.ID, "", time.Hour)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	// Advance beyond expiry.
	now = now.Add(2 * time.Hour)
	if _, _, err := st.AuthenticateToken(ctx, plaintext); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected expired token rejected, got %v", err)
	}
}

func TestDisabledClientTokenRejected(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	c, _ := st.CreateClient(ctx, "acme", "")
	plaintext, _, _ := st.CreateToken(ctx, c.ID, "", 0)
	if err := st.SetClientDisabled(ctx, c.ID, true); err != nil {
		t.Fatalf("SetClientDisabled: %v", err)
	}
	if _, _, err := st.AuthenticateToken(ctx, plaintext); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected disabled client token rejected, got %v", err)
	}
}

func TestGrantsAndWildcard(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	c, _ := st.CreateClient(ctx, "acme", "")

	if _, err := st.AddGrant(ctx, c.ID, "anthropic", "claude-sonnet-4-5", ""); err != nil {
		t.Fatalf("AddGrant: %v", err)
	}
	// Idempotent add returns the same grant, no duplicate.
	if _, err := st.AddGrant(ctx, c.ID, "anthropic", "claude-sonnet-4-5", ""); err != nil {
		t.Fatalf("AddGrant (again): %v", err)
	}
	grants, _ := st.ListGrants(ctx, c.ID)
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(grants))
	}

	ok, _ := st.HasGrant(ctx, c.ID, "anthropic", "claude-sonnet-4-5")
	if !ok {
		t.Fatal("expected grant to match")
	}
	ok, _ = st.HasGrant(ctx, c.ID, "anthropic", "claude-opus-4-1")
	if ok {
		t.Fatal("did not expect match for un-granted model")
	}

	// Wildcard grant matches any model for the provider.
	if _, err := st.AddGrant(ctx, c.ID, "claude-bin", WildcardModel, ""); err != nil {
		t.Fatalf("AddGrant wildcard: %v", err)
	}
	ok, _ = st.HasGrant(ctx, c.ID, "claude-bin", "anything-goes")
	if !ok {
		t.Fatal("expected wildcard grant to match any model")
	}

	if err := st.DeleteGrant(ctx, grants[0].ID); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}
	ok, _ = st.HasGrant(ctx, c.ID, "anthropic", "claude-sonnet-4-5")
	if ok {
		t.Fatal("expected grant removed")
	}
}

func TestAccessRequestDedupeAndDecide(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	c, _ := st.CreateClient(ctx, "acme", "")

	r1, err := st.RequestAccess(ctx, c.ID, "openai", "gpt-5", "need it")
	if err != nil {
		t.Fatalf("RequestAccess: %v", err)
	}
	if r1.Count != 1 {
		t.Fatalf("expected count 1, got %d", r1.Count)
	}
	// Duplicate pending request dedupes (same row, bumped count).
	r2, err := st.RequestAccess(ctx, c.ID, "openai", "gpt-5", "still need it")
	if err != nil {
		t.Fatalf("RequestAccess (again): %v", err)
	}
	if r2.ID != r1.ID || r2.Count != 2 {
		t.Fatalf("expected dedupe with count 2, got id=%s count=%d", r2.ID, r2.Count)
	}
	pending, _ := st.ListAccessRequests(ctx, RequestPending, "")
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending request, got %d", len(pending))
	}

	// Approving creates a grant and flips status.
	decided, err := st.DecideAccessRequest(ctx, r1.ID, true, "ok")
	if err != nil {
		t.Fatalf("DecideAccessRequest: %v", err)
	}
	if decided.Status != RequestApproved || decided.DecidedAt == nil {
		t.Fatalf("unexpected decided request: %+v", decided)
	}
	ok, _ := st.HasGrant(ctx, c.ID, "openai", "gpt-5")
	if !ok {
		t.Fatal("approving a request should create a grant")
	}
	// Re-deciding a settled request errors.
	if _, err := st.DecideAccessRequest(ctx, r1.ID, false, ""); err == nil {
		t.Fatal("expected error re-deciding a settled request")
	}
	// A new request for the same route can be created again (previous was approved).
	if _, err := st.RequestAccess(ctx, c.ID, "openai", "gpt-5", ""); err != nil {
		t.Fatalf("RequestAccess after approval: %v", err)
	}
}

func TestAuthorizeAllowsGrantedDeniesOthers(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	c, _ := st.CreateClient(ctx, "acme", "")
	if _, err := st.AddGrant(ctx, c.ID, "anthropic", "claude-sonnet-4-5", ""); err != nil {
		t.Fatalf("AddGrant: %v", err)
	}

	// Granted route is allowed and audited.
	dec, err := st.Authorize(ctx, c.ID, "anthropic", "claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !dec.Allowed || dec.RequestID != "" {
		t.Fatalf("expected allow with no request, got %+v", dec)
	}

	// Un-granted route is denied, creates a pending request, and is audited.
	dec, err = st.Authorize(ctx, c.ID, "openai", "gpt-5")
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if dec.Allowed || dec.RequestID == "" {
		t.Fatalf("expected deny with request id, got %+v", dec)
	}
	// Repeat denial dedupes to the same pending request.
	dec2, _ := st.Authorize(ctx, c.ID, "openai", "gpt-5")
	if dec2.RequestID != dec.RequestID {
		t.Fatalf("expected deduped request, got %s vs %s", dec2.RequestID, dec.RequestID)
	}

	audit, err := st.ListAudit(ctx, c.ID, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var allow, deny int
	for _, e := range audit {
		switch e.Decision {
		case "allow":
			allow++
		case "deny":
			deny++
		}
	}
	if allow != 1 || deny != 2 {
		t.Fatalf("expected 1 allow / 2 deny audit entries, got allow=%d deny=%d (total=%d)", allow, deny, len(audit))
	}
}

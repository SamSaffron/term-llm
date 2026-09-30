package cmd

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/hub"
)

func TestHubCacheScopeFor(t *testing.T) {
	first := hubCacheScopeFor("random-state-id", "none")
	if len(first) != 24 || first != hubCacheScopeFor("random-state-id", "none") {
		t.Fatalf("scope must be opaque and deterministic: %q", first)
	}
	for _, scope := range []string{
		hubCacheScopeFor("different-state-id", "none"),
		hubCacheScopeFor("random-state-id", "bearer"),
		hubCacheScopeFor("random-state-id", "passkey"),
		uiCacheScopeFor("random-state-id", "none"),
	} {
		if first == scope {
			t.Fatal("identity, auth mode and domain separator must isolate scopes")
		}
	}
	if hubCacheScopeFor("", "none") != "" {
		t.Fatal("missing identity must disable persistence")
	}
}

func TestHubCacheIdentityState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.json.cache-identity")
	id := hubCacheIdentity(path)
	if len(id) != 64 || id != hubCacheIdentity(path) {
		t.Fatalf("identity is not stable: %q", id)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity must be private: %v %v", info, err)
	}
	if other := hubCacheIdentity(filepath.Join(t.TempDir(), "identity")); other == id || other == "" {
		t.Fatal("independent state must have independent identities")
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A damaged sidecar heals to a fresh identity (a new, empty cache scope).
	if healed := hubCacheIdentity(path); len(healed) != 64 || healed == id || healed != hubCacheIdentity(path) {
		t.Fatalf("corrupt identity must heal to a new stable identity, got %q", healed)
	}
	if matches, _ := filepath.Glob(path + ".tmp-*"); len(matches) != 0 {
		t.Fatalf("identity temp files leaked: %v", matches)
	}
	if hubCacheIdentity(filepath.Join(path, "unavailable")) != "" {
		t.Fatal("unavailable identity must disable persistence")
	}
}

func hubCacheShellConfig(t *testing.T, recorder *httptest.ResponseRecorder) hubPageConfig {
	t.Helper()
	match := regexp.MustCompile(`data-hub-config="([^"]+)"`).FindStringSubmatch(recorder.Body.String())
	if len(match) != 2 {
		t.Fatalf("shell configuration not found: %s", recorder.Body.String())
	}
	var config hubPageConfig
	if err := json.Unmarshal([]byte(html.UnescapeString(match[1])), &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestHubCacheScopeShellInjection(t *testing.T) {
	store := hub.NewStore(filepath.Join(t.TempDir(), "nodes.json"))
	s := newHubServer(hub.NewRegistry(store), store)
	request := func(cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Accept", "text/html")
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: hubAuthCookieName, Value: cookie})
		}
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("shell must remain no-store: %v", w.Header())
		}
		return w
	}
	first := hubCacheShellConfig(t, request(""))
	if first.CacheScope == "" || !first.CacheDisplayAllowed || first.AuthMode != "none" {
		t.Fatalf("no-auth shell cache policy: %+v", first)
	}
	if next := hubCacheShellConfig(t, request("")); next.CacheScope != first.CacheScope {
		t.Fatal("scope changed on reload")
	}
	s.requireAuth = true
	s.token = "hub-secret-one"
	login := hubCacheShellConfig(t, request(""))
	if login.Page != "bearer-login" || login.CacheScope != "" || login.CacheDisplayAllowed {
		t.Fatalf("unauthorized browser must receive only an uncached login shell: %+v", login)
	}
	bearer := hubCacheShellConfig(t, request(s.token))
	if bearer.Page != "dashboard" || bearer.AuthMode != "bearer" || !bearer.CacheDisplayAllowed || bearer.CacheScope == "" || bearer.CacheScope == first.CacheScope {
		t.Fatalf("authenticated bearer shell cache policy: %+v", bearer)
	}
	s.token = "hub-secret-two"
	rotated := hubCacheShellConfig(t, request(s.token))
	if rotated.CacheScope != bearer.CacheScope {
		t.Fatal("token rotation must not derive/change the cache identity")
	}
	s.store = nil
	missing := request(s.token)
	if config := hubCacheShellConfig(t, missing); config.CacheScope != "" || strings.Contains(html.UnescapeString(missing.Body.String()), `"cacheScope"`) {
		t.Fatal("scope must be absent without persisted identity")
	}
}

func TestHubCacheScopePasskeyIdentity(t *testing.T) {
	s := newTestPasskeyHub(t, "")
	first := s.hubCacheScope("passkey")
	if first == "" || first != hubCacheScopeFor(s.passkey.store.User().ID, "passkey") {
		t.Fatal("passkey scope must use the existing non-secret random user identity")
	}
	other := newTestPasskeyHub(t, "")
	if first == other.hubCacheScope("passkey") {
		t.Fatal("independent passkey state must be isolated")
	}
	// Passkey middleware blocks navigation before the dashboard handler runs.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept", "text/html")
	s.handler().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || strings.Contains(w.Body.String(), "cacheScope") {
		t.Fatalf("unauthenticated passkey navigation must not reveal a dashboard: %d %s", w.Code, w.Body.String())
	}
}

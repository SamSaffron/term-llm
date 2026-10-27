package serve

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var extensionLinkPattern = regexp.MustCompile(`<link rel="stylesheet" data-extension="dracula" href="(extensions/[^"]+)">`)

func extensionShell(t *testing.T, s *serveServer, target string, authorize bool) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	if authorize {
		r.Header.Set("Authorization", "Bearer secret")
	}
	w := httptest.NewRecorder()
	s.handleUI(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status %d", target, w.Code)
	}
	return w.Body.String()
}

func TestIndexLinksExtensionStylesheetsBeforeFirstPaint(t *testing.T) {
	e := extensionFixture(t)
	s := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/chat", requireAuth: true, token: "secret"}, extensions: e}
	generation := e.manager.Status().Generation

	body := extensionShell(t, s, "/", true)
	match := extensionLinkPattern.FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("authorized shell has no extension stylesheet:\n%s", body)
	}
	if want := "extensions/" + generation + "/dracula/style.css"; match[1] != want {
		t.Fatalf("href = %q, want %q", match[1], want)
	}
	head := body[:strings.Index(body, "</head>")]
	if !strings.Contains(head, match[0]) || strings.Index(head, match[0]) < strings.Index(head, `rel="stylesheet" href="dist/`) {
		t.Fatal("extension stylesheet must be in <head> after the app stylesheet so it overrides it")
	}

	// The relative href must reach the authenticated asset route under <base>.
	mux := http.NewServeMux()
	s.registerExtensionRoutes(mux)
	asset := httptest.NewRequest(http.MethodGet, "/"+match[1], nil)
	asset.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "secret"})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, asset)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "#282a36") {
		t.Fatalf("linked asset: %d %s", w.Code, w.Body)
	}

	for name, tc := range map[string]struct {
		target    string
		authorize bool
	}{
		"unauthorized shell": {"/", false},
		"safe mode":          {"/?safe-mode=1", true},
	} {
		if body := extensionShell(t, s, tc.target, tc.authorize); strings.Contains(body, "data-extension") {
			t.Errorf("%s must not link extension stylesheets", name)
		}
	}
}

func TestIndexExtensionStylesheetsFollowReloads(t *testing.T) {
	e := extensionFixture(t)
	s := &serveServer{cfg: serveServerConfig{ui: true, basePath: "/chat"}, extensions: e}
	before := extensionLinkPattern.FindStringSubmatch(extensionShell(t, s, "/session-1", false))
	if before == nil {
		t.Fatal("open server shell has no extension stylesheet")
	}

	// The rendered shell is cached; edits must still produce the new generation.
	css := filepath.Join(e.manager.Status().Directory, "dracula", "style.css")
	if err := os.WriteFile(css, []byte("body { background: #000; }"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.reload(); err != nil {
		t.Fatal(err)
	}
	after := extensionLinkPattern.FindStringSubmatch(extensionShell(t, s, "/session-1", false))
	if after == nil || after[1] == before[1] || !strings.Contains(after[1], e.manager.Status().Generation) {
		t.Fatalf("stylesheet did not follow reload: before %v after %v", before, after)
	}

	e.flagSet = true
	e.enabledFlag = nil
	if err := e.reload(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(extensionShell(t, s, "/", false), "data-extension") {
		t.Fatal("disabled extension is still linked")
	}
}

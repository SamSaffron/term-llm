package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/widgets"
)

func TestWidgetIndexUsesProxyPortableLinks(t *testing.T) {
	dir := t.TempDir()
	widgetDir := filepath.Join(dir, "example")
	if err := os.Mkdir(widgetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "title: Example\nmount: example\ncommand: [\"echo\", \"$PORT\"]\n"
	if err := os.WriteFile(filepath.Join(widgetDir, "widget.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := widgets.NewManager(dir, "/ui")
	defer manager.Close()
	srv := &serveServer{widgetsMgr: manager}

	rec := httptest.NewRecorder()
	srv.handleWidgetIndex(rec, httptest.NewRequest(http.MethodGet, "/widgets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="widgets/example/"`) {
		t.Fatalf("index missing relative widget link: %s", body)
	}
	if strings.Contains(body, `href="/ui/widgets/example/"`) {
		t.Fatalf("index baked in node-local base path: %s", body)
	}
}

func TestAdminWidgetsStopRoute(t *testing.T) {
	manager := widgets.NewManager(t.TempDir(), "/ui")
	defer manager.Close()
	srv := &serveServer{widgetsMgr: manager}
	mux := http.NewServeMux()
	srv.registerWidgetRoutes(mux)

	post := httptest.NewRecorder()
	mux.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/admin/widgets/stop", nil))
	if post.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200; body=%s", post.Code, post.Body.String())
	}
	if got := post.Body.String(); got != `{"ok":true}` {
		t.Fatalf("POST body = %q, want %q", got, `{"ok":true}`)
	}

	get := httptest.NewRecorder()
	mux.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/admin/widgets/stop", nil))
	if get.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405; body=%s", get.Code, get.Body.String())
	}
}

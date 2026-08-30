package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/samsaffron/term-llm/internal/widgets"
)

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

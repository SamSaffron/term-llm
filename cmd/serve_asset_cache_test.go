package cmd

import (
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/serveui"
)

func uiBuildAsset(t *testing.T, name string) string {
	t.Helper()
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for _, file := range serveui.ChatAssetManifest().Files {
		if strings.HasPrefix(file, stem+"-") && strings.HasSuffix(file, ext) && len(strings.TrimSuffix(strings.TrimPrefix(file, stem+"-"), ext)) == 8 {
			return file
		}
	}
	t.Fatalf("missing logical build asset %s", name)
	return ""
}

func TestUIAssetCachingPolicy(t *testing.T) {
	version := serveui.AssetVersion()
	immutable := "public, max-age=31536000, immutable"
	tests := []struct {
		path, cache string
		status      int
	}{
		{"/" + serveui.ChatAssetManifest().EntryJS, immutable, 200},
		{"/" + serveui.ChatAssetManifest().EntryJS + "?v=OLD", immutable, 200},
		{"/" + serveui.ChatAssetManifest().EntryCSS[0], immutable, 200},
		{"/icon-512.png?v=" + version, immutable, 200},
		{"/icon-512.png?v=OLD", "no-cache", 200},
		{"/icon-512.png?notv=" + version, "no-cache", 200},
		{"/icon-512.png", "no-cache", 200},
		{"/manifest.webmanifest?v=" + version, immutable, 200},
		{"/manifest.webmanifest?v=OLD", "no-cache", 200},
		{"/sw.js?v=" + version, "no-cache", 200},
		{"/index.html?v=" + version, "no-cache, no-store, must-revalidate", 200},
		{"/index.html?v=OLD", "no-cache, no-store, must-revalidate", 200},
		{"/session-id?v=" + version, "no-cache, no-store, must-revalidate", 200},
		{"/dist/chunks/Lightbox-OLDHASH1.js", "", 404},
		{"/dist/app.js", "", 404},
	}
	for _, base := range []string{"", "/ui", "/node/alpha/chat"} {
		srv := &serveServer{cfg: serveServerConfig{ui: true, basePath: base}}
		handler := http.Handler(http.HandlerFunc(srv.handleUI))
		if base != "" {
			handler = http.StripPrefix(base, handler)
		}
		for _, tt := range tests {
			t.Run(base+tt.path, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, base+tt.path, nil))
				if recorder.Code != tt.status {
					t.Fatalf("status=%d want %d", recorder.Code, tt.status)
				}
				if recorder.Header().Get("Cache-Control") != tt.cache {
					t.Fatalf("cache=%q want %q", recorder.Header().Get("Cache-Control"), tt.cache)
				}
				if tt.status == 200 && tt.cache == "no-cache" && recorder.Header().Get("ETag") == "" {
					t.Fatal("missing ETag")
				}
			})
		}
	}
}

func TestHubJSVersionedCacheScope(t *testing.T) {
	s := hubWithBackend(t, "/chat", func(http.ResponseWriter, *http.Request) {})
	handler := s.handler()
	shell := httptest.NewRecorder()
	handler.ServeHTTP(shell, httptest.NewRequest(http.MethodGet, "/", nil))
	version := serveui.HubAssetVersion()
	if !strings.Contains(shell.Body.String(), `src="/dist/hub.js?v=`+version+`"`) {
		t.Fatal("shell missing versioned Hub JS")
	}
	for _, query := range []string{"", "?v=wrong", "?v=" + version} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/dist/hub.js"+query, nil))
		want := "no-cache"
		if query == "?v="+version {
			want = "public, max-age=31536000, immutable"
		}
		if recorder.Code != 200 || recorder.Header().Get("Cache-Control") != want {
			t.Fatalf("query %s status/cache=%d/%s", query, recorder.Code, recorder.Header().Get("Cache-Control"))
		}
	}
}

package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestServeMediaPublisherAndHandler(t *testing.T) {
	sourceDir := t.TempDir()
	storeDir := t.TempDir()
	source := filepath.Join(sourceDir, "demo.mp4")
	data := append([]byte{0, 0, 0, 24}, []byte("ftypisom0123456789")...)
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	publisher := &serveMediaPublisher{dir: storeDir}
	stored, err := publisher.PublishMedia(context.Background(), source, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(stored) != storeDir || filepath.Ext(stored) != ".mp4" || strings.Contains(stored, "demo") {
		t.Fatalf("stored path = %q", stored)
	}
	storedAgain, err := publisher.PublishMedia(context.Background(), source, "video/mp4")
	if err != nil || storedAgain != stored {
		t.Fatalf("dedupe publish = %q, %v", storedAgain, err)
	}

	server := &serveServer{cfg: serveServerConfig{basePath: "/ui"}, mediaPublisher: publisher}
	mediaURL := server.mediaURL(stored)
	if !strings.HasPrefix(mediaURL, "/ui/media/") {
		t.Fatalf("media URL = %q", mediaURL)
	}
	request := httptest.NewRequest(http.MethodGet, strings.TrimPrefix(mediaURL, "/ui"), nil)
	request.Header.Set("Range", "bytes=4-7")
	response := httptest.NewRecorder()
	server.handleMedia(response, request)
	if response.Code != http.StatusPartialContent || response.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("response = %d headers=%v", response.Code, response.Header())
	}
	body, _ := io.ReadAll(response.Result().Body)
	if string(body) != "ftyp" {
		t.Fatalf("range body = %q", body)
	}
	if response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff header")
	}
}

func TestServeMediaPublisherRejectsTypeMismatch(t *testing.T) {
	source := filepath.Join(t.TempDir(), "fake.png")
	if err := os.WriteFile(source, []byte("<html>not an image</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	publisher := &serveMediaPublisher{dir: t.TempDir()}
	if _, err := publisher.PublishMedia(context.Background(), source, "image/png"); err == nil || !strings.Contains(err.Error(), "media type changed") {
		t.Fatalf("mismatched publish error = %v", err)
	}
}

func TestServeMediaRejectsTraversalAndDoesNotProjectSourcePath(t *testing.T) {
	publisher := &serveMediaPublisher{dir: t.TempDir()}
	server := &serveServer{cfg: serveServerConfig{basePath: "/chat"}, mediaPublisher: publisher}
	for _, path := range []string{"/media/../secret.mp4", "/media/nested/file.mp4", "/media/file.txt"} {
		response := httptest.NewRecorder()
		server.handleMedia(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d", path, response.Code)
		}
	}
	entries := server.toolMediaEntries([]llm.MediaArtifact{{
		Reference: "0123456789abcdef0123456789abcdef", SourcePath: "/private/source/demo.mp4", StoredPath: "/missing/stored.mp4", MediaType: "video/mp4", Name: "demo.mp4",
	}})
	if len(entries) != 0 {
		t.Fatalf("unowned stored path projected: %#v", entries)
	}
}

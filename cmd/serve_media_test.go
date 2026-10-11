package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/testutil/mediafixture"
)

func TestServeMediaPublisherAndHandler(t *testing.T) {
	sourceDir := t.TempDir()
	storeDir := t.TempDir()
	source := filepath.Join(sourceDir, "demo.mp4")
	data := mediafixture.MP4("isom")
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
	for _, c := range []struct {
		name, message string
		data          []byte
	}{
		{"unsupported copy", "published copy failed media verification", []byte("<html>not an image</html>")},
		{"different supported MIME", "media type changed while publishing", mediafixture.MP3()},
	} {
		t.Run(c.name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "fake.png")
			if err := os.WriteFile(source, c.data, 0o600); err != nil {
				t.Fatal(err)
			}
			publisher := &serveMediaPublisher{dir: t.TempDir()}
			if _, err := publisher.PublishMedia(context.Background(), source, "image/png"); err == nil || !strings.Contains(err.Error(), c.message) {
				t.Fatalf("publish error = %v, want %q", err, c.message)
			}
			files, err := os.ReadDir(publisher.dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 0 {
				t.Fatalf("failed verification left files: %v", files)
			}
		})
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

func TestServeAudioPublicationAndRanges(t *testing.T) {
	for mime, data := range mediafixture.Audio() {
		t.Run(mime, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "source.bin")
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			publisher := &serveMediaPublisher{dir: t.TempDir()}
			stored, err := publisher.PublishMedia(context.Background(), source, mime)
			if err != nil {
				t.Fatal(err)
			}
			again, err := publisher.PublishMedia(context.Background(), source, mime)
			if err != nil || again != stored {
				t.Fatalf("dedupe=%q,%v", again, err)
			}
			if mapped, ok := serveMediaTypeForName(stored); !ok || mapped != mime {
				t.Fatalf("mapping=%q,%v", mapped, ok)
			}
			if _, err := publisher.PublishMedia(context.Background(), source, "image/png"); err == nil {
				t.Fatal("mismatch accepted")
			}
			if err := os.Remove(source); err != nil {
				t.Fatal(err)
			}
			s := &serveServer{cfg: serveServerConfig{basePath: "/custom", requireAuth: true, token: "secret"}, mediaPublisher: publisher}
			url := s.mediaURL(stored)
			// Mount the same authenticated/CORS handler and base-path stripping as serve.
			mux := http.NewServeMux()
			mux.Handle("/custom/media/", http.StripPrefix("/custom", s.auth(s.cors(s.handleMedia))))
			unauthorized := httptest.NewRecorder()
			mux.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, url, nil))
			if unauthorized.Code != http.StatusUnauthorized {
				t.Fatalf("auth status=%d", unauthorized.Code)
			}
			cases := []struct {
				method, rng        string
				status, start, end int
			}{
				{"GET", "", 200, 0, len(data) - 1}, {"HEAD", "", 200, 0, -1}, {"GET", "bytes=0-3", 206, 0, 3},
				{"GET", "bytes=4-7", 206, 4, 7}, {"GET", "bytes=-4", 206, len(data) - 4, len(data) - 1},
				{"GET", "bytes=4-", 206, 4, len(data) - 1}, {"GET", "bytes=999999-", 416, 0, -1}, {"POST", "", 405, 0, -1},
			}
			for _, c := range cases {
				req := httptest.NewRequest(c.method, url, nil)
				req.Header.Set("Authorization", "Bearer secret")
				if c.rng != "" {
					req.Header.Set("Range", c.rng)
				}
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				if rec.Code != c.status {
					t.Fatalf("%s %s status=%d want=%d body=%s", c.method, c.rng, rec.Code, c.status, rec.Body.String())
				}
				if c.status == 200 || c.status == 206 {
					if rec.Header().Get("Content-Type") != mime || rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "private, max-age=86400" || !strings.Contains(rec.Header().Get("Vary"), "Authorization") || rec.Header().Get("Accept-Ranges") != "bytes" {
						t.Fatalf("headers=%v", rec.Header())
					}
					if c.method == "HEAD" {
						if rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != strconv.Itoa(len(data)) {
							t.Fatalf("HEAD=%v", rec)
						}
					} else {
						if !bytes.Equal(rec.Body.Bytes(), data[c.start:c.end+1]) {
							t.Fatalf("range body mismatch %s", c.rng)
						}
						if rec.Header().Get("Content-Length") != strconv.Itoa(c.end-c.start+1) {
							t.Fatal("length mismatch")
						}
					}
					if c.status == 206 && rec.Header().Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", c.start, c.end, len(data)) {
						t.Fatalf("range header=%q", rec.Header().Get("Content-Range"))
					}
				}
			}
			entries := s.toolMediaEntries([]llm.MediaArtifact{{Reference: "reference", StoredPath: stored, SourcePath: source, MediaType: mime, Name: "narration", Caption: "Listen"}})
			b, _ := json.Marshal(entries)
			if len(entries) != 1 || strings.Contains(string(b), publisher.dir) || strings.Contains(string(b), source) {
				t.Fatalf("projection=%s", b)
			}
			var raw any
			_ = json.Unmarshal(b, &raw)
			recovered := mediaEntriesFromValue(raw)
			if len(recovered) != 1 || recovered[0] != entries[0] {
				t.Fatalf("replay=%+v", recovered)
			}
		})
	}
}

func TestServeAudioRejectsOversizeSymlinksAndUnownedPaths(t *testing.T) {
	source := filepath.Join(t.TempDir(), "large.mp3")
	if err := os.WriteFile(source, mediafixture.MP3(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(source, 100<<20+1); err != nil {
		t.Fatal(err)
	}
	p := &serveMediaPublisher{dir: t.TempDir()}
	if _, err := p.PublishMedia(context.Background(), source, "audio/mpeg"); err == nil || !strings.Contains(err.Error(), "100 MiB") {
		t.Fatalf("oversize=%v", err)
	}
	link := filepath.Join(p.dir, "symlink.mp3")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	s := &serveServer{mediaPublisher: p}
	rec := httptest.NewRecorder()
	s.handleMedia(rec, httptest.NewRequest("GET", "/media/symlink.mp3", nil))
	if rec.Code != 404 || s.mediaURL(link) != "" || s.mediaURL(source) != "" {
		t.Fatalf("unsafe paths exposed, status=%d", rec.Code)
	}
}

func TestServeMediaPublisherInspectsMetadataPastProbe(t *testing.T) {
	mp3 := mediafixture.MP3()
	tag := make([]byte, 70010)
	copy(tag, "ID3")
	tag[3] = 3
	n := 70000
	for i := 9; i >= 6; i-- {
		tag[i] = byte(n & 127)
		n >>= 7
	}
	tagged := append(tag, mp3...)
	ftyp := mediafixture.MP4("isom")
	moov := mediafixture.MP4("isom", "soun")[len(ftyp):]
	mp4 := append(ftyp, mediafixture.Box("mdat", make([]byte, 1<<20))...)
	mp4 = append(mp4, moov...)
	for mime, data := range map[string][]byte{"audio/mpeg": tagged, "audio/mp4": mp4} {
		t.Run(mime, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "narration")
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			p := &serveMediaPublisher{dir: t.TempDir()}
			stored, err := p.PublishMedia(context.Background(), source, mime)
			if err != nil {
				t.Fatal(err)
			}
			bytes, err := os.ReadFile(stored)
			if err != nil || !reflect.DeepEqual(bytes, data) {
				t.Fatalf("stored copy changed: %v", err)
			}
		})
	}
}

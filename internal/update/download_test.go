package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseChecksum(t *testing.T) {
	const asset = "term-llm_1.2.3_linux_amd64.tar.gz"
	digest := strings.Repeat("ab", sha256.Size)
	valid := digest + "  " + asset
	for _, tc := range []struct {
		name, manifest string
		ok             bool
	}{
		{"valid", valid + "\n", true},
		{"binary mode", digest + " *" + asset, true},
		{"uppercase and CRLF", strings.ToUpper(digest) + "  " + asset + "\r\n", true},
		{"unrelated entry", digest + "  other.tar.gz\n" + valid, true},
		{"missing", digest + "  other.tar.gz", false},
		{"suffix is not match", valid + ".evil", false},
		{"duplicate", valid + "\n" + valid, false},
		{"duplicate different digest", valid + "\n" + strings.Repeat("cd", 32) + "  " + asset, false},
		{"malformed second entry", valid + "\nextra fields " + asset, false},
		{"nonhex", strings.Repeat("z", 64) + "  " + asset, false},
		{"short digest", "ab  " + asset, false},
		{"single space", digest + " " + asset, false},
		{"extra fields", valid + " extra", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := releaseChecksum(tc.manifest, asset)
			if (err == nil) != tc.ok {
				t.Fatalf("releaseChecksum() = %x, %v; success want %v", got, err, tc.ok)
			}
			if tc.ok && !bytes.Equal(got, bytes.Repeat([]byte{0xab}, sha256.Size)) {
				t.Fatalf("wrong digest: %x", got)
			}
		})
	}
}

func TestValidReleaseTag(t *testing.T) {
	for _, tag := range []string{"v1.2.3", "v0.0.1-rc.1", "v1.0.0+build_2"} {
		if !validReleaseTag(tag) {
			t.Errorf("rejected safe tag %q", tag)
		}
	}
	for _, tag := range []string{"", "v", "vdev", "v1/../../evil", "v1?x", "v1#x", "v1\\evil", "v1\n", "v1%2fother"} {
		if validReleaseTag(tag) {
			t.Errorf("accepted unsafe tag %q", tag)
		}
	}
}

func secureTestClient(srv *httptest.Server) *http.Client {
	client := releaseHTTPClient()
	client.Transport = srv.Client().Transport
	return client
}

func TestDownloadFileLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		chunked bool
		status  int
		ok      bool
	}{
		{"at limit", "12345678", false, http.StatusOK, true},
		{"known oversize", "123456789", false, http.StatusOK, false},
		{"chunked oversize", "123456789", true, http.StatusOK, false},
		{"chunked at limit", "12345678", true, http.StatusOK, true},
		{"not found", "missing", false, http.StatusNotFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("User-Agent") != updateUserAgent {
					t.Error("missing updater user agent")
				}
				w.WriteHeader(tc.status)
				if tc.chunked {
					w.(http.Flusher).Flush()
				}
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			dest := filepath.Join(t.TempDir(), "download")
			err := downloadFile(context.Background(), secureTestClient(srv), srv.URL, dest, 8)
			if (err == nil) != tc.ok {
				t.Fatalf("downloadFile() = %v; success want %v", err, tc.ok)
			}
			data, statErr := os.ReadFile(dest)
			if tc.ok && (statErr != nil || string(data) != tc.body) {
				t.Fatalf("download = %q, %v", data, statErr)
			}
			if !tc.ok && !os.IsNotExist(statErr) {
				t.Fatalf("failed download left a file: %v", statErr)
			}
		})
	}
}

func TestReleaseDownloadRedirects(t *testing.T) {
	insecureHits := 0
	insecure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		insecureHits++
	}))
	defer insecure.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/https":
			http.Redirect(w, r, "/asset", http.StatusFound)
		case "/downgrade":
			http.Redirect(w, r, insecure.URL, http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		default:
			io.WriteString(w, "asset")
		}
	}))
	defer srv.Close()
	for _, tc := range []struct {
		path string
		ok   bool
	}{{"/https", true}, {"/downgrade", false}, {"/loop", false}} {
		t.Run(tc.path, func(t *testing.T) {
			err := downloadFile(context.Background(), secureTestClient(srv), srv.URL+tc.path, filepath.Join(t.TempDir(), "asset"), 100)
			if (err == nil) != tc.ok {
				t.Fatalf("downloadFile() = %v, success want %v", err, tc.ok)
			}
		})
	}
	if err := downloadFile(context.Background(), releaseHTTPClient(), insecure.URL, filepath.Join(t.TempDir(), "asset"), 100); err == nil {
		t.Fatal("accepted initial HTTP URL")
	}
	if insecureHits != 0 {
		t.Fatalf("made %d insecure requests", insecureHits)
	}
}

func TestDownloadVerifiedArchive(t *testing.T) {
	const asset = "term-llm_1.2.3_linux_amd64.tar.gz"
	// Inert bytes: tests never run release payloads or replace an executable.
	payload := []byte("release archive fixture")
	goodManifest := fmt.Sprintf("%x  %s\n", sha256.Sum256(payload), asset)
	for _, tc := range []struct {
		name, manifest string
		body           []byte
		ok             bool
	}{
		{"valid", goodManifest, payload, true},
		{"tampered", goodManifest, []byte("tampered"), false},
		{"missing", "", payload, false},
		{"malformed", "bogus  " + asset, payload, false},
		{"duplicate", goodManifest + goodManifest, payload, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archiveRequests := 0
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/checksums.txt":
					io.WriteString(w, tc.manifest)
				case "/" + asset:
					archiveRequests++
					w.Write(tc.body)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			dir := t.TempDir()
			path, err := downloadVerifiedArchive(context.Background(), secureTestClient(srv), srv.URL, asset, dir)
			if (err == nil) != tc.ok {
				t.Fatalf("downloadVerifiedArchive() = %q, %v; success want %v", path, err, tc.ok)
			}
			if tc.ok {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, payload) {
					t.Fatalf("verified archive = %q, %v", got, err)
				}
			} else {
				if path != "" {
					t.Fatalf("returned unverified archive: %s", path)
				}
				if _, err := os.Stat(filepath.Join(dir, asset)); !os.IsNotExist(err) {
					t.Fatalf("left unverified archive: %v", err)
				}
			}
			if tc.name != "valid" && tc.name != "tampered" && archiveRequests != 0 {
				t.Fatal("downloaded archive before validating checksum entry")
			}
		})
	}
}

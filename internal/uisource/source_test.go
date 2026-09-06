package uisource

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/serveui"
)

func TestEmbeddedSourceMatchesBuild(t *testing.T) {
	b, err := embedded.ReadFile("assets/" + archiveName)
	if err != nil {
		t.Skip("run make frontend to build readable source")
	}
	r, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if r.AssetVersion != serveui.AssetVersion() {
		t.Fatalf("source %s != assets %s; rebuild source archive", r.AssetVersion, serveui.AssetVersion())
	}
	if len(r.Files["frontend/src/components/Sidebar.tsx"]) == 0 {
		t.Fatal("missing readable source")
	}
	for p := range r.Files {
		if bytes.Contains([]byte(p), []byte(".test.")) {
			t.Fatalf("test source embedded: %s", p)
		}
	}
	r, err = Resolve(context.Background(), "dev", false)
	if err != nil || r.Quality != "exact" {
		t.Fatalf("%+v %v", r, err)
	}
}
func TestArchiveRejectsTraversalAndLinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind byte
	}{{"../escape", tar.TypeReg}, {"/absolute", tar.TypeReg}, {"link", tar.TypeSymlink}} {
		var buf bytes.Buffer
		z := gzip.NewWriter(&buf)
		w := tar.NewWriter(z)
		if err := w.WriteHeader(&tar.Header{Name: tc.name, Typeflag: tc.kind, Mode: 0644}); err != nil {
			t.Fatal(err)
		}
		w.Close()
		z.Close()
		if _, err := Decode(buf.Bytes()); err == nil {
			t.Fatal("unsafe archive accepted")
		}
	}
}
func TestCandidateVersions(t *testing.T) {
	var releases []release
	for _, tag := range []string{"v1.9.0", "v1.10.0", "v2.0.0", "v2.1.0-beta.1", "garbage"} {
		r := release{Tag: tag}
		r.Assets = append(r.Assets, struct {
			Name string `json:"name"`
		}{archiveName})
		releases = append(releases, r)
	}
	for _, tc := range []struct{ version, want string }{{"dev", "v2.0.0"}, {"1.10.1", "v1.10.0"}, {"v1.10.0", "v1.10.0"}, {"v1.9.1", "v1.9.0"}} {
		got := candidateTags(releases, tc.version)
		if len(got) == 0 || got[0] != tc.want {
			t.Fatalf("%s: %v", tc.version, got)
		}
	}
	if got := candidateTags(releases, "v1.0.0"); len(got) != 0 {
		t.Fatalf("newer fallback: %v", got)
	}
}
func TestCacheDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir, err := CacheDir()
	if err != nil || !bytes.HasSuffix([]byte(dir), []byte("term-llm/ui-source")) {
		t.Fatalf("%s %v", dir, err)
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixtureArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	w := tar.NewWriter(z)
	for _, f := range []struct{ name, body string }{{"source-manifest.json", `{"format_version":1,"version":"v1.2.0","asset_version":"fixture"}`}, {"frontend/src/styles/base/tokens.css", ":root { --bg: purple; }"}} {
		if err := w.WriteHeader(&tar.Header{Name: f.name, Mode: 0644, Size: int64(len(f.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func TestPublishedCacheAndConditionalLookup(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	old := client
	defer func() { client = old }()
	data := fixtureArchive(t)
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	calls := 0
	conditional := false
	client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		body := ""
		code := 200
		headers := http.Header{}
		switch {
		case r.URL.Host == "api.github.com":
			headers.Set("ETag", `"fixture"`)
			if r.Header.Get("If-None-Match") == `"fixture"` {
				conditional = true
				code = 304
			} else {
				body = `[{"tag_name":"v1.2.0","assets":[{"name":"term-llm-ui-source.tar.gz"}]}]`
			}
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			body = sha + "  " + archiveName + "\n"
		case strings.HasSuffix(r.URL.Path, archiveName):
			body = string(data)
		default:
			return nil, fmt.Errorf("unexpected URL %s", r.URL)
		}
		return &http.Response{StatusCode: code, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	ref, err := published(context.Background(), "dev")
	if err != nil || ref.Version != "v1.2.0" {
		t.Fatalf("%+v %v", ref, err)
	}
	if calls != 3 {
		t.Fatalf("download count %d", calls)
	}
	if _, err = published(context.Background(), "dev"); err != nil || calls != 3 {
		t.Fatalf("cache miss: %d %v", calls, err)
	}
	dir, _ := CacheDir()
	raw, err := os.ReadFile(filepath.Join(dir, "releases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c catalog
	if err = json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	c.Fetched = time.Now().Add(-48 * time.Hour)
	raw, _ = json.Marshal(c)
	if err = os.WriteFile(filepath.Join(dir, "releases.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = published(context.Background(), "dev"); err != nil || !conditional || calls != 4 {
		t.Fatalf("ETag not used: %t %d %v", conditional, calls, err)
	}
	// Corruption never returns unverified cache data; offline lookup degrades.
	if err = os.WriteFile(filepath.Join(dir, "releases", "v1.2.0", sha, archiveName), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	client = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("offline") })}
	if _, err = published(context.Background(), "dev"); err == nil {
		t.Fatal("corrupt offline cache accepted")
	}
}

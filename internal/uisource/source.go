// Package uisource provides private, on-demand authoring references. It never
// injects source archives into the browser's ordinary asset payload.
package uisource

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/filelock"
	"github.com/samsaffron/term-llm/internal/serveui"
	"golang.org/x/mod/semver"
)

//go:embed assets/*
var embedded embed.FS

const archiveName = "term-llm-ui-source.tar.gz"
const maxBytes = 32 << 20

var mu sync.Mutex
var resolved = map[string]*Reference{}
var resolvedAt = map[string]time.Time{}

type Reference struct {
	Version      string            `json:"version"`
	AssetVersion string            `json:"asset_version"`
	Digest       string            `json:"digest"`
	Provenance   string            `json:"provenance"`
	Quality      string            `json:"quality"`
	Warning      string            `json:"warning,omitempty"`
	Files        map[string][]byte `json:"-"`
}
type File struct {
	Path   string `json:"path"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}

func (r *Reference) Inventory() []File {
	out := []File{}
	for p, b := range r.Files {
		h := sha256.Sum256(b)
		out = append(out, File{p, len(b), hex.EncodeToString(h[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
func Decode(data []byte) (*Reference, error) {
	z, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer z.Close()
	tr := tar.NewReader(io.LimitReader(z, maxBytes+1))
	files := map[string][]byte{}
	total := int64(0)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg || !fs.ValidPath(h.Name) || strings.Contains(h.Name, "\\") {
			return nil, fmt.Errorf("unsafe source archive entry %q", h.Name)
		}
		total += h.Size
		if h.Size < 0 || total > maxBytes || len(files) >= 2048 {
			return nil, fmt.Errorf("source archive exceeds limits")
		}
		if _, ok := files[h.Name]; ok {
			return nil, fmt.Errorf("duplicate archive entry")
		}
		b, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if err != nil {
			return nil, err
		}
		if int64(len(b)) != h.Size {
			return nil, io.ErrUnexpectedEOF
		}
		files[h.Name] = b
	}
	var manifest struct {
		Version      string `json:"version"`
		AssetVersion string `json:"asset_version"`
		Format       int    `json:"format_version"`
	}
	if err = json.Unmarshal(files["source-manifest.json"], &manifest); err != nil {
		return nil, err
	}
	if manifest.Format != 1 {
		return nil, fmt.Errorf("unsupported source format")
	}
	digest := sha256.Sum256(data)
	return &Reference{Version: manifest.Version, AssetVersion: manifest.AssetVersion, Digest: hex.EncodeToString(digest[:]), Files: files}, nil
}
func Resolve(ctx context.Context, version string, allowPublished bool) (*Reference, error) {
	mu.Lock()
	defer mu.Unlock()
	key := fmt.Sprintf("%s:%t", version, allowPublished)
	if r := resolved[key]; r != nil && (r.Provenance != "github-release" || time.Since(resolvedAt[key]) < 24*time.Hour) {
		return r, nil
	}
	var fallback *Reference
	if b, err := embedded.ReadFile("assets/" + archiveName); err == nil {
		if r, err := Decode(b); err == nil {
			r.Provenance = "embedded"
			r.Quality = "exact"
			if r.AssetVersion == serveui.AssetVersion() {
				resolved[key] = r
				return r, nil
			}
			r.Quality = "approximate"
			r.Warning = "Embedded source differs from running assets"
			fallback = r
		}
	}
	if allowPublished {
		lookupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if r, err := published(lookupCtx, version); err == nil {
			r.Provenance = "github-release"
			resolvedAt[key] = time.Now()
			r.Quality = "approximate"
			r.Warning = "Readable source is a release reference; verify against current compiled assets"
			if r.AssetVersion == serveui.AssetVersion() {
				r.Quality = "exact"
				r.Warning = ""
			}
			resolved[key] = r
			return r, nil
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	files := map[string][]byte{}
	for _, p := range serveui.SourceAssetPaths() {
		if b, err := serveui.StaticAsset(p); err == nil {
			files["compiled/"+p] = b
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no UI source or compiled assets available")
	}
	return &Reference{Version: version, AssetVersion: serveui.AssetVersion(), Provenance: "embedded-assets", Quality: "compiled-only", Warning: "Readable source unavailable; minified CSS/JS remain useful", Files: files}, nil
}
func CacheDir() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "term-llm", "ui-source"), nil
}

type release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
	} `json:"assets"`
}
type catalog struct {
	Fetched  time.Time `json:"fetched_at"`
	ETag     string    `json:"etag,omitempty"`
	Releases []release `json:"releases"`
}

var client = &http.Client{Timeout: 15 * time.Second}

func download(ctx context.Context, url string, limit int64) ([]byte, error) {
	b, _, _, err := downloadConditional(ctx, url, limit, "")
	return b, err
}
func downloadConditional(ctx context.Context, url string, limit int64, etag string) ([]byte, int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, "", err
	}
	req.Header.Set("User-Agent", "term-llm-ui-source")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && etag != "" {
		return nil, resp.StatusCode, etag, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, "", fmt.Errorf("source download: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(b)) > limit {
		return nil, resp.StatusCode, "", fmt.Errorf("source download too large")
	}
	return b, resp.StatusCode, resp.Header.Get("ETag"), err
}
func atomicCache(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}
func candidateTags(releases []release, version string) []string {
	v := version
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	isRelease := semver.IsValid(v)
	out := []string{}
	seen := map[string]bool{}
	for _, r := range releases {
		if r.Draft || r.Prerelease || !semver.IsValid(r.Tag) || semver.Prerelease(r.Tag) != "" || seen[r.Tag] {
			continue
		}
		has := false
		for _, a := range r.Assets {
			if a.Name == archiveName {
				has = true
			}
		}
		if !has {
			continue
		}
		if isRelease && semver.Compare(r.Tag, v) > 0 {
			continue
		}
		out = append(out, r.Tag)
		seen[r.Tag] = true
	}
	sort.Slice(out, func(i, j int) bool { return semver.Compare(out[i], out[j]) > 0 })
	return out
}
func published(ctx context.Context, version string) (*Reference, error) {
	dir, err := CacheDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	unlock, err := filelock.TryLock(filepath.Join(dir, "download.lock"))
	if err != nil {
		return nil, fmt.Errorf("source cache busy or unavailable: %w", err)
	}
	defer unlock()

	var c catalog
	raw, _ := os.ReadFile(filepath.Join(dir, "releases.json"))
	_ = json.Unmarshal(raw, &c)
	if time.Since(c.Fetched) > 24*time.Hour {
		var releases []release
		etag := ""
		for page := 1; page <= 3; page++ {
			conditional := ""
			if page == 1 {
				conditional = c.ETag
			}
			b, status, nextETag, err := downloadConditional(ctx, fmt.Sprintf("https://api.github.com/repos/samsaffron/term-llm/releases?per_page=100&page=%d", page), 4<<20, conditional)
			if status == http.StatusNotModified {
				releases = c.Releases
				etag = c.ETag
				break
			}
			if page == 1 {
				etag = nextETag
			}
			if err != nil {
				break
			}
			var batch []release
			if json.Unmarshal(b, &batch) != nil {
				break
			}
			releases = append(releases, batch...)
			if len(batch) < 100 {
				break
			}
		}
		if len(releases) > 0 {
			c = catalog{Fetched: time.Now(), ETag: etag, Releases: releases}
			b, _ := json.Marshal(c)
			_ = atomicCache(filepath.Join(dir, "releases.json"), b)
		}
	}
	tags := candidateTags(c.Releases, version)
	for i, tag := range tags {
		if i >= 3 {
			break
		}
		base := filepath.Join(dir, "releases", tag)
		var meta struct {
			SHA string `json:"sha256"`
		}
		b, _ := os.ReadFile(filepath.Join(base, "metadata.json"))
		_ = json.Unmarshal(b, &meta)
		if len(meta.SHA) == 64 {
			if _, err := hex.DecodeString(meta.SHA); err == nil {
				b, err := os.ReadFile(filepath.Join(base, meta.SHA, archiveName))
				sum := sha256.Sum256(b)
				if err == nil && hex.EncodeToString(sum[:]) == meta.SHA {
					if r, err := Decode(b); err == nil {
						return r, nil
					}
				}
			}
		}
		url := "https://github.com/samsaffron/term-llm/releases/download/" + tag + "/"
		checks, err := download(ctx, url+"checksums.txt", 1<<20)
		if err != nil {
			continue
		}
		sha := ""
		for _, line := range strings.Split(string(checks), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == archiveName {
				sha = fields[0]
			}
		}
		if len(sha) != 64 {
			continue
		}
		b, err = download(ctx, url+archiveName, maxBytes)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != sha {
			continue
		}
		r, err := Decode(b)
		if err != nil {
			continue
		}
		_ = atomicCache(filepath.Join(base, sha, archiveName), b)
		metadata, _ := json.Marshal(map[string]string{"sha256": sha, "version": tag, "provenance": url + archiveName})
		_ = atomicCache(filepath.Join(base, "metadata.json"), metadata)
		return r, nil
	}
	return nil, fmt.Errorf("published readable source unavailable")
}

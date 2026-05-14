package cmd

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/image"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/serveui"
	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) verboseLog(format string, args ...any) {
	if s.cfg.verbose {
		log.Printf("[verbose] "+format, args...)
	}
}

func (s *serveServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// uiAssetCacheEntry holds the precomputed ETag and gzip-compressed form of an
// embedded UI asset. Both are derived from the raw content and never change
// for the lifetime of the process, so computing them once and caching avoids
// per-request sha256 + gzip work.
type uiAssetCacheEntry struct {
	etag       string
	compressed []byte // gzip-compressed; nil when content type is not compressible
}

// uiAssetCache maps [16]byte (leading bytes of sha256(raw content)) to
// *uiAssetCacheEntry. Using an array key keeps comparisons cheap and
// allocation-free.
var uiAssetCache sync.Map // [16]byte → *uiAssetCacheEntry

// uiGetOrBuildEntry returns the cached entry for data, building and storing it
// on the first call for each unique content.
func uiGetOrBuildEntry(data []byte, compressible bool) *uiAssetCacheEntry {
	sum := sha256.Sum256(data)
	var key [16]byte
	copy(key[:], sum[:16])
	if v, ok := uiAssetCache.Load(key); ok {
		return v.(*uiAssetCacheEntry)
	}
	e := &uiAssetCacheEntry{
		etag: `"` + hex.EncodeToString(sum[:]) + `"`,
	}
	if compressible {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write(data)
		_ = gz.Close()
		e.compressed = buf.Bytes()
	}
	actual, _ := uiAssetCache.LoadOrStore(key, e)
	return actual.(*uiAssetCacheEntry)
}

func uiETagMatches(headerValue, etag string) bool {
	if headerValue == "" || etag == "" {
		return false
	}
	for _, part := range strings.Split(headerValue, ",") {
		candidate := strings.TrimSpace(part)
		if candidate == "*" || candidate == etag || candidate == "W/"+etag {
			return true
		}
	}
	return false
}

func uiAcceptsGzip(headerValue string) bool {
	for _, part := range strings.Split(headerValue, ",") {
		pieces := strings.Split(part, ";")
		if strings.TrimSpace(strings.ToLower(pieces[0])) != "gzip" {
			continue
		}
		accepted := true
		for _, param := range pieces[1:] {
			param = strings.TrimSpace(strings.ToLower(param))
			if strings.HasPrefix(param, "q=") {
				q, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(param, "q=")), 64)
				if err == nil && q <= 0 {
					accepted = false
				}
			}
		}
		return accepted
	}
	return false
}

func uiCompressibleContentType(contentType string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/javascript", "application/json", "application/manifest+json", "application/x-javascript", "image/svg+xml":
		return true
	default:
		return strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml")
	}
}

func uiAddVary(header http.Header, value string) {
	for _, existing := range strings.Split(header.Get("Vary"), ",") {
		if strings.EqualFold(strings.TrimSpace(existing), value) {
			return
		}
	}
	if current := header.Get("Vary"); current != "" {
		header.Set("Vary", current+", "+value)
		return
	}
	header.Set("Vary", value)
}

func serveEmbeddedUIBytes(w http.ResponseWriter, r *http.Request, data []byte, contentType, cacheControl string, conditional bool) {
	header := w.Header()
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	if cacheControl != "" {
		header.Set("Cache-Control", cacheControl)
	}

	compressible := uiCompressibleContentType(contentType)
	if compressible {
		uiAddVary(header, "Accept-Encoding")
	}

	entry := uiGetOrBuildEntry(data, compressible)

	if conditional {
		header.Set("ETag", entry.etag)
		if uiETagMatches(r.Header.Get("If-None-Match"), entry.etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	body := data
	if compressible && entry.compressed != nil && uiAcceptsGzip(r.Header.Get("Accept-Encoding")) {
		body = entry.compressed
		header.Set("Content-Encoding", "gzip")
	}
	header.Set("Content-Length", strconv.Itoa(len(body)))

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

func (s *serveServer) handleUI(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.ui {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	// basePath is already stripped by http.StripPrefix; URL.Path is "/" or "/session-id" etc.
	assetName := strings.TrimPrefix(r.URL.Path, "/")
	if assetName == "index.html" {
		cacheControl := "no-cache, no-store, must-revalidate"
		if strings.Contains(r.URL.RawQuery, "v=") {
			cacheControl = "public, max-age=31536000, immutable"
		}
		serveEmbeddedUIBytes(w, r, s.renderIndexHTML(), "text/html; charset=utf-8", cacheControl, false)
		return
	}
	if assetName == "manifest.webmanifest" {
		cacheControl := "no-cache"
		if strings.Contains(r.URL.RawQuery, "v=") {
			cacheControl = "public, max-age=31536000, immutable"
		}
		serveEmbeddedUIBytes(w, r, serveui.RenderManifest(), "application/manifest+json", cacheControl, true)
		return
	}
	if assetName == "sw.js" {
		serveEmbeddedUIBytes(w, r, serveui.RenderServiceWorker(serveui.RenderOptions{WebRTC: s.webrtcEnabled}), "text/javascript", "no-cache", true)
		return
	}
	if assetName != "" && !strings.Contains(assetName, "..") {
		if data, err := serveui.StaticAsset(assetName); err == nil {
			contentType := mime.TypeByExtension(filepath.Ext(assetName))
			if contentType == "" {
				// mime.TypeByExtension may return empty for .woff2 on some systems.
				switch filepath.Ext(assetName) {
				case ".woff2":
					contentType = "font/woff2"
				default:
					contentType = http.DetectContentType(data)
				}
			}
			cacheControl := "no-cache"
			if strings.Contains(r.URL.RawQuery, "v=") {
				cacheControl = "public, max-age=31536000, immutable"
			}
			serveEmbeddedUIBytes(w, r, data, contentType, cacheControl, true)
			return
		}
	}

	// SPA catch-all: serve index.html for all other paths.
	serveEmbeddedUIBytes(w, r, s.renderIndexHTML(), "text/html; charset=utf-8", "no-cache, no-store, must-revalidate", false)
}

func (s *serveServer) renderIndexHTML() []byte {
	s.indexHTMLOnce.Do(func() {
		s.cachedIndexHTML = s.buildIndexHTML()
	})
	return s.cachedIndexHTML
}

func (s *serveServer) buildIndexHTML() []byte {
	// Inject UI prefix so JS can prefix all API calls with it.
	// Also inject VAPID public key for web push if configured.
	var headSnippet string
	escaped, _ := json.Marshal(s.cfg.basePath)
	headSnippet += `<script>window.TERM_LLM_UI_PREFIX=` + string(escaped) + `;</script>`
	versionEscaped, _ := json.Marshal(serveui.AssetVersion())
	headSnippet += `<script>window.TERM_LLM_UI_VERSION=` + string(versionEscaped) + `;</script>`
	sidebarSessions := s.cfg.sidebarSessions
	if len(sidebarSessions) == 0 {
		sidebarSessions = []string{"all"}
	}
	sidebarEscaped, _ := json.Marshal(sidebarSessions)
	headSnippet += `<script>window.TERM_LLM_SIDEBAR_SESSIONS=` + string(sidebarEscaped) + `;</script>`
	if s.cfgRef != nil {
		if vapidKey := s.cfgRef.Serve.WebPush.VAPIDPublicKey; vapidKey != "" {
			vapidEscaped, _ := json.Marshal(vapidKey)
			headSnippet += `<script>window.TERM_LLM_VAPID_PUBLIC_KEY=` + string(vapidEscaped) + `;</script>`
		}
	}
	headSnippet += s.webrtcHeadSnippet
	return serveui.RenderIndexHTML(s.cfg.basePath, headSnippet, serveui.RenderOptions{WebRTC: s.webrtcEnabled})
}

// prewarmUIAssetCache pre-compresses the service-worker shell assets in a
// background goroutine so the first real browser request finds gzip bytes
// already cached rather than paying the compression cost inline.
func (s *serveServer) prewarmUIAssetCache() {
	go func() {
		// Rendered assets: build + cache in one shot.
		_ = s.renderIndexHTML()
		uiGetOrBuildEntry(serveui.RenderServiceWorker(serveui.RenderOptions{WebRTC: s.webrtcEnabled}), true)
		uiGetOrBuildEntry(serveui.RenderManifest(), true)

		// Static shell assets (SW precache list minus the PNG icon).
		assetNames := []string{
			"app.css",
			"app-core.js", "app-render.js", "app-stream.js",
			"app-sessions.js",
			"markdown-setup.js", "markdown-streaming.js", "decoration.js",
			"vendor/marked/marked.umd.min.js",
			"vendor/dompurify/purify.min.js",
		}
		if s.webrtcEnabled {
			assetNames = append(assetNames, "app-webrtc.js")
		}
		for _, name := range assetNames {
			if data, err := serveui.StaticAsset(name); err == nil {
				uiGetOrBuildEntry(data, true)
			}
		}
	}()
}

func (s *serveServer) imageOutputDir() string {
	if s != nil && s.cfgRef != nil {
		if outputDir := image.ExpandPath(s.cfgRef.Image.OutputDir); outputDir != "" {
			return outputDir
		}
	}
	return image.ExpandPath("~/Pictures/term-llm")
}

func resolveServeRequestPath(baseDir, requestPath, routePrefix string) (string, error) {
	requested := strings.TrimPrefix(requestPath, routePrefix)
	requested = strings.TrimPrefix(requested, "/")
	if requested == "" {
		return "", fmt.Errorf("empty path")
	}
	for _, segment := range strings.Split(requested, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("invalid path segment")
		}
	}

	cleaned := path.Clean("/" + requested)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" || cleaned == "." {
		return "", fmt.Errorf("empty path")
	}

	absDir, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return "", fmt.Errorf("eval base dir: %w", err)
	}

	filePath := filepath.Join(absDir, filepath.FromSlash(cleaned))
	absFile, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		return "", fmt.Errorf("eval file: %w", err)
	}
	if absFile != absDir && !strings.HasPrefix(absFile, absDir+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes base dir")
	}

	return absFile, nil
}

func serveRoutePath(baseRoute, baseDir, servedPath string) string {
	absDir, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		absDir, err = filepath.Abs(baseDir)
		if err != nil {
			return baseRoute + filepath.Base(servedPath)
		}
	}
	absServed, err := filepath.EvalSymlinks(servedPath)
	if err != nil {
		absServed, err = filepath.Abs(servedPath)
		if err != nil {
			return baseRoute + filepath.Base(servedPath)
		}
	}

	relPath, err := filepath.Rel(absDir, absServed)
	if err != nil || relPath == "." || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return baseRoute + filepath.Base(servedPath)
	}

	return baseRoute + filepath.ToSlash(relPath)
}

func canonicalizeServeExistingPath(p string) (string, error) {
	absPath, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func canonicalizeServeDirForWrite(dir string) (string, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absDir)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}

	parent := filepath.Dir(absDir)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return filepath.Clean(absDir), nil
		}
		return "", err
	}
	return filepath.Join(filepath.Clean(resolvedParent), filepath.Base(absDir)), nil
}

func pathWithinDir(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

func (s *serveServer) handleImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	outputDir := s.imageOutputDir()

	absFile, err := resolveServeRequestPath(outputDir, r.URL.Path, "/images/")
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Add("Vary", "Authorization, Cookie")
	serveResolvedFile(w, r, absFile)
}

// serveResolvedFile serves a file whose absolute path has already been
// resolved and validated. It avoids http.ServeFile's implicit
// "/index.html"→"./" redirect and directory-index handling, which would
// otherwise cause requests for a file literally named index.html (or any
// path ending in that segment) to 301 back to the SPA catch-all.
func serveResolvedFile(w http.ResponseWriter, r *http.Request, absFile string) {
	f, err := os.Open(absFile)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// handleFile serves arbitrary files from the configured files-dir.
func (s *serveServer) handleFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	filesDir := s.cfg.filesDir
	if filesDir == "" {
		http.NotFound(w, r)
		return
	}

	absFile, err := resolveServeRequestPath(filesDir, r.URL.Path, "/files/")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Add("Vary", "Authorization, Cookie")
	serveResolvedFile(w, r, absFile)
}

// ensureFileServeable makes the given file available from the configured
// files-dir when it already comes from an approved source directory. Approved
// sources are: the files-dir itself, the image output directory, the uploads
// directory used by saveUploadedFile, and any directory granted to tools via
// --write-dir or cfg.Tools.WriteDirs. Tool output in those locations is
// operator- or server-sanctioned, so republishing it under /files/ respects
// the documented --files-dir contract while still blocking tool results that
// report paths outside any approved location. Returns the serveable path and
// true on success, or ("", false) otherwise.
func (s *serveServer) ensureFileServeable(filePath string) (string, bool) {
	filesDir := s.cfg.filesDir
	if filesDir == "" {
		return "", false
	}

	absDir, err := canonicalizeServeDirForWrite(filesDir)
	if err != nil {
		log.Printf("[serve] ensureFileServeable: resolve dir %s: %v", filesDir, err)
		return "", false
	}
	absFile, err := canonicalizeServeExistingPath(filePath)
	if err != nil {
		log.Printf("[serve] ensureFileServeable: resolve %s: %v", filePath, err)
		return "", false
	}

	if pathWithinDir(absFile, absDir) {
		return absFile, true
	}

	approvedSourceDirs := []string{absDir}
	if imageOutputDir := s.imageOutputDir(); imageOutputDir != "" {
		if absImageOutputDir, err := canonicalizeServeDirForWrite(imageOutputDir); err == nil {
			approvedSourceDirs = append(approvedSourceDirs, absImageOutputDir)
		}
	}
	if uploadsDir := serveUploadsDir(); uploadsDir != "" {
		if absUploads, err := canonicalizeServeDirForWrite(uploadsDir); err == nil {
			approvedSourceDirs = append(approvedSourceDirs, absUploads)
		}
	}
	for _, wd := range s.cfg.writeDirs {
		if absWriteDir, err := canonicalizeServeDirForWrite(wd); err == nil {
			approvedSourceDirs = append(approvedSourceDirs, absWriteDir)
		}
	}

	approved := false
	for _, dir := range approvedSourceDirs {
		if pathWithinDir(absFile, dir) {
			approved = true
			break
		}
	}
	if !approved {
		log.Printf("[serve] ensureFileServeable: rejecting %s outside approved dirs", absFile)
		return "", false
	}

	if err := os.MkdirAll(absDir, 0755); err != nil {
		log.Printf("[serve] ensureFileServeable: mkdir %s: %v", absDir, err)
		return "", false
	}

	src, err := os.Open(absFile)
	if err != nil {
		log.Printf("[serve] ensureFileServeable: open %s: %v", absFile, err)
		return "", false
	}
	defer src.Close()

	// Use a deterministic content-derived name so repeated tool-event emits or
	// history replays don't mint fresh file URLs or leak duplicate copies into
	// the files dir. Hash first so cache hits do not create temporary files or
	// rewrite the destination directory.
	hash := sha256.New()
	if _, err := io.Copy(hash, src); err != nil {
		log.Printf("[serve] ensureFileServeable: hash %s: %v", absFile, err)
		return "", false
	}
	sum := hash.Sum(nil)
	destName := fmt.Sprintf("serve-%s-%s", hex.EncodeToString(sum[:16]), filepath.Base(absFile))
	destPath := filepath.Join(absDir, destName)
	if info, err := os.Stat(destPath); err == nil && !info.IsDir() {
		return destPath, true
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		log.Printf("[serve] ensureFileServeable: rewind %s: %v", absFile, err)
		return "", false
	}

	tmpPath := filepath.Join(absDir, fmt.Sprintf("serve-%s-%s.tmp", randomSuffix(), filepath.Base(absFile)))
	dst, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		log.Printf("[serve] ensureFileServeable: create %s: %v", tmpPath, err)
		return "", false
	}
	copyHash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(dst, copyHash), src); err != nil {
		dst.Close()
		os.Remove(tmpPath)
		log.Printf("[serve] ensureFileServeable: copy to %s: %v", tmpPath, err)
		return "", false
	}
	if !bytes.Equal(copyHash.Sum(nil), sum) {
		dst.Close()
		os.Remove(tmpPath)
		log.Printf("[serve] ensureFileServeable: source changed while materializing %s", absFile)
		return "", false
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmpPath)
		log.Printf("[serve] ensureFileServeable: close %s: %v", tmpPath, err)
		return "", false
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		if info, statErr := os.Stat(destPath); statErr == nil && !info.IsDir() {
			os.Remove(tmpPath)
			return destPath, true
		}
		os.Remove(tmpPath)
		log.Printf("[serve] ensureFileServeable: rename %s -> %s: %v", tmpPath, destPath, err)
		return "", false
	}

	return destPath, true
}

// ensureImageServeable makes the given image path servable via /images/ by
// copying it into the configured image output directory when the source is
// already under an approved location (image output dir itself, the uploads
// dir used by saveUploadedFile, or any operator-granted tool write-dir).
// Tool-reported paths outside every approved dir are rejected so arbitrary
// host files can't be republished through /images/. Returns the serveable
// path and true on success, or ("", false) otherwise.
func (s *serveServer) ensureImageServeable(imgPath string) (string, bool) {
	outputDir := s.imageOutputDir()

	absDir, err := canonicalizeServeDirForWrite(outputDir)
	if err != nil {
		log.Printf("[serve] ensureImageServeable: resolve dir %s: %v", outputDir, err)
		return "", false
	}
	absImg, err := canonicalizeServeExistingPath(imgPath)
	if err != nil {
		log.Printf("[serve] ensureImageServeable: resolve %s: %v", imgPath, err)
		return "", false
	}

	if pathWithinDir(absImg, absDir) {
		return absImg, true
	}

	approvedSourceDirs := []string{absDir}
	if uploadsDir := serveUploadsDir(); uploadsDir != "" {
		if absUploads, err := canonicalizeServeDirForWrite(uploadsDir); err == nil {
			approvedSourceDirs = append(approvedSourceDirs, absUploads)
		}
	}
	for _, wd := range s.cfg.writeDirs {
		if absWriteDir, err := canonicalizeServeDirForWrite(wd); err == nil {
			approvedSourceDirs = append(approvedSourceDirs, absWriteDir)
		}
	}

	approved := false
	for _, dir := range approvedSourceDirs {
		if pathWithinDir(absImg, dir) {
			approved = true
			break
		}
	}
	if !approved {
		log.Printf("[serve] ensureImageServeable: rejecting %s outside approved dirs", absImg)
		return "", false
	}

	if err := os.MkdirAll(absDir, 0755); err != nil {
		log.Printf("[serve] ensureImageServeable: mkdir %s: %v", absDir, err)
		return "", false
	}

	src, err := os.Open(absImg)
	if err != nil {
		log.Printf("[serve] ensureImageServeable: open %s: %v", absImg, err)
		return "", false
	}
	defer src.Close()
	if info, err := src.Stat(); err != nil {
		log.Printf("[serve] ensureImageServeable: stat %s: %v", absImg, err)
		return "", false
	} else if info.IsDir() {
		log.Printf("[serve] ensureImageServeable: rejecting directory %s", absImg)
		return "", false
	} else if info.Size() > maxMaterializedSessionImageBytes {
		log.Printf("[serve] ensureImageServeable: refusing image larger than %d bytes: %s", maxMaterializedSessionImageBytes, absImg)
		return "", false
	}

	// Use a deterministic content-derived name so repeated history fetches
	// don't mint fresh image URLs or leak duplicate copies into the output dir.
	hash := sha256.New()
	limited := &io.LimitedReader{R: src, N: maxMaterializedSessionImageBytes + 1}
	written, err := io.Copy(hash, limited)
	if err != nil {
		log.Printf("[serve] ensureImageServeable: hash %s: %v", absImg, err)
		return "", false
	}
	if written > maxMaterializedSessionImageBytes {
		log.Printf("[serve] ensureImageServeable: refusing image larger than %d bytes: %s", maxMaterializedSessionImageBytes, absImg)
		return "", false
	}

	sum := hash.Sum(nil)
	destName := fmt.Sprintf("serve-%s-%s", hex.EncodeToString(sum[:16]), filepath.Base(absImg))
	destPath := filepath.Join(absDir, destName)
	if info, err := os.Stat(destPath); err == nil && !info.IsDir() {
		return destPath, true
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		log.Printf("[serve] ensureImageServeable: rewind %s: %v", absImg, err)
		return "", false
	}

	tmpPath := filepath.Join(absDir, fmt.Sprintf("serve-%s-%s.tmp", randomSuffix(), filepath.Base(absImg)))
	dst, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		log.Printf("[serve] ensureImageServeable: create %s: %v", tmpPath, err)
		return "", false
	}
	copyHash := sha256.New()
	limited = &io.LimitedReader{R: src, N: maxMaterializedSessionImageBytes + 1}
	written, err = io.Copy(io.MultiWriter(dst, copyHash), limited)
	if err != nil {
		dst.Close()
		os.Remove(tmpPath)
		log.Printf("[serve] ensureImageServeable: copy to %s: %v", tmpPath, err)
		return "", false
	}
	if written > maxMaterializedSessionImageBytes {
		dst.Close()
		os.Remove(tmpPath)
		log.Printf("[serve] ensureImageServeable: refusing image larger than %d bytes: %s", maxMaterializedSessionImageBytes, absImg)
		return "", false
	}
	if !bytes.Equal(copyHash.Sum(nil), sum) {
		dst.Close()
		os.Remove(tmpPath)
		log.Printf("[serve] ensureImageServeable: source changed while materializing %s", absImg)
		return "", false
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmpPath)
		log.Printf("[serve] ensureImageServeable: close %s: %v", tmpPath, err)
		return "", false
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		if info, statErr := os.Stat(destPath); statErr == nil && !info.IsDir() {
			os.Remove(tmpPath)
			return destPath, true
		}
		os.Remove(tmpPath)
		log.Printf("[serve] ensureImageServeable: rename %s -> %s: %v", tmpPath, destPath, err)
		return "", false
	}

	return destPath, true
}

// serveUploadsDir returns the first-party uploads directory used by
// saveUploadedFile. Returns empty string if the data dir is unavailable.
func serveUploadsDir() string {
	dataDir, err := session.GetDataDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dataDir, "uploads")
}

const (
	sessionMessagesPageSize          = 200
	maxMaterializedSessionImageBytes = 25 << 20
)

func parseSessionMessagesLimit(raw string) int {
	limit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || limit <= 0 {
		return sessionMessagesPageSize
	}
	if limit > sessionMessagesPageSize {
		return sessionMessagesPageSize
	}
	return limit
}

func parseSessionMessagesOffset(raw string) int {
	offset, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || offset < 0 {
		return 0
	}
	return offset
}

func imageExtensionForMediaType(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.TrimSpace(strings.ToLower(contentType))
	}
	exts, err := mime.ExtensionsByType(mediaType)
	if err == nil {
		for _, ext := range exts {
			if ext != "" {
				return ext
			}
		}
	}
	switch mediaType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	default:
		return ".img"
	}
}

func (s *serveServer) materializeInlineSessionImage(mediaType, base64Data string) (string, bool) {
	outputDir := s.imageOutputDir()
	absDir, err := canonicalizeServeDirForWrite(outputDir)
	if err != nil {
		log.Printf("[serve] materializeInlineSessionImage: resolve dir %s: %v", outputDir, err)
		return "", false
	}
	if err := os.MkdirAll(absDir, 0755); err != nil {
		log.Printf("[serve] materializeInlineSessionImage: mkdir %s: %v", absDir, err)
		return "", false
	}

	mediaType = strings.TrimSpace(mediaType)
	base64Data = strings.TrimSpace(base64Data)
	if base64.StdEncoding.DecodedLen(len(base64Data)) > maxMaterializedSessionImageBytes+2 {
		log.Printf("[serve] materializeInlineSessionImage: refusing inline image larger than %d bytes", maxMaterializedSessionImageBytes)
		return "", false
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, mediaType)
	_, _ = io.WriteString(hash, "\n")
	_, _ = io.WriteString(hash, base64Data)
	sum := hash.Sum(nil)
	destName := "history-" + hex.EncodeToString(sum[:16]) + imageExtensionForMediaType(mediaType)
	destPath := filepath.Join(absDir, destName)
	if info, err := os.Stat(destPath); err == nil && !info.IsDir() {
		return destPath, true
	}

	tmpPath := destPath + ".tmp-" + randomSuffix()
	dst, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		log.Printf("[serve] materializeInlineSessionImage: create %s: %v", tmpPath, err)
		return "", false
	}
	dec := base64.NewDecoder(base64.StdEncoding, strings.NewReader(base64Data))
	limited := &io.LimitedReader{R: dec, N: maxMaterializedSessionImageBytes + 1}
	written, err := io.Copy(dst, limited)
	if err != nil {
		dst.Close()
		os.Remove(tmpPath)
		log.Printf("[serve] materializeInlineSessionImage: decode %s: %v", destPath, err)
		return "", false
	}
	if written > maxMaterializedSessionImageBytes {
		dst.Close()
		os.Remove(tmpPath)
		log.Printf("[serve] materializeInlineSessionImage: refusing inline image larger than %d bytes", maxMaterializedSessionImageBytes)
		return "", false
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmpPath)
		log.Printf("[serve] materializeInlineSessionImage: close %s: %v", tmpPath, err)
		return "", false
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		if _, statErr := os.Stat(destPath); statErr == nil {
			os.Remove(tmpPath)
			return destPath, true
		}
		os.Remove(tmpPath)
		log.Printf("[serve] materializeInlineSessionImage: rename %s -> %s: %v", tmpPath, destPath, err)
		return "", false
	}
	return destPath, true
}

func (s *serveServer) sessionMessageImageURL(part llm.Part) string {
	if part.ImagePath != "" {
		if served, ok := s.ensureImageServeable(part.ImagePath); ok {
			return serveRoutePath(s.cfg.imagesRoute(), s.imageOutputDir(), served)
		}
	}
	if part.ImageData == nil || strings.TrimSpace(part.ImageData.Base64) == "" {
		return ""
	}
	if served, ok := s.materializeInlineSessionImage(part.ImageData.MediaType, part.ImageData.Base64); ok {
		return serveRoutePath(s.cfg.imagesRoute(), s.imageOutputDir(), served)
	}
	return ""
}

func sessionSummaryProviderKey(cfg *config.Config, sess session.SessionSummary) string {
	provider := strings.TrimSpace(sess.ProviderKey)
	if provider == "" {
		provider = resolveSessionProviderKey(cfg, &session.Session{Provider: sess.Provider})
	}
	return provider
}

func sessionSummaryLastMessageAt(sess session.SessionSummary) time.Time {
	lastMessageAt := sess.LastMessageAt
	if lastMessageAt.IsZero() {
		lastMessageAt = sess.CreatedAt
	}
	return lastMessageAt
}

func (s *serveServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	categories, err := parseSidebarSessionCategories(r.URL.Query().Get("categories"), false)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	includeArchived := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "1") ||
		strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "true")

	sessions, err := s.store.List(r.Context(), session.ListOptions{
		Limit:          100,
		Archived:       includeArchived,
		Categories:     categories,
		SortByActivity: true,
	})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to list sessions")
		return
	}

	type sessionEntry struct {
		ID            string                `json:"id"`
		Number        int64                 `json:"number,omitempty"`
		Name          string                `json:"name,omitempty"`
		ShortTitle    string                `json:"short_title"`
		LongTitle     string                `json:"long_title"`
		Mode          session.SessionMode   `json:"mode,omitempty"`
		Origin        session.SessionOrigin `json:"origin,omitempty"`
		Provider      string                `json:"provider,omitempty"`
		Archived      bool                  `json:"archived"`
		Pinned        bool                  `json:"pinned"`
		CreatedAt     int64                 `json:"created_at"`
		LastMessageAt int64                 `json:"last_message_at"`
		MsgCount      int                   `json:"message_count"`
	}

	result := make([]sessionEntry, 0, len(sessions))
	for _, sess := range sessions {
		provider := sessionSummaryProviderKey(s.cfgRef, sess)
		lastMessageAt := sessionSummaryLastMessageAt(sess)
		result = append(result, sessionEntry{
			Name:          sess.Name,
			ID:            sess.ID,
			Number:        sess.Number,
			ShortTitle:    sess.PreferredShortTitle(),
			LongTitle:     sess.PreferredLongTitle(),
			Mode:          sess.Mode,
			Origin:        sess.Origin,
			Provider:      provider,
			Archived:      sess.Archived,
			Pinned:        sess.Pinned,
			CreatedAt:     sess.CreatedAt.UnixMilli(),
			LastMessageAt: lastMessageAt.UnixMilli(),
			MsgCount:      sess.MessageCount,
		})
	}

	writeJSONConditional(w, r, http.StatusOK, map[string]any{"sessions": result})
}

func ftsQueryFromUser(query string) string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return !(r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= 0x80)
	})
	terms := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		term := strings.Trim(field, `"`)
		if len([]rune(term)) < 2 {
			continue
		}
		key := strings.ToLower(term)
		if seen[key] {
			continue
		}
		seen[key] = true
		terms = append(terms, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " ")
}

func (s *serveServer) handleSessionsSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if s.store == nil {
		writeJSONConditional(w, r, http.StatusOK, map[string]any{"sessions": []any{}})
		return
	}

	categories, err := parseSidebarSessionCategories(r.URL.Query().Get("categories"), false)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	includeArchived := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "1") ||
		strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "true")
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		query = strings.TrimSpace(r.URL.Query().Get("query"))
	}
	ftsQuery := ftsQueryFromUser(query)
	if ftsQuery == "" {
		writeJSONConditional(w, r, http.StatusOK, map[string]any{"sessions": []any{}})
		return
	}
	limit := 20
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		if parsed, err := strconv.Atoi(rawLimit); err == nil && parsed > 0 {
			limit = min(parsed, 50)
		}
	}

	summaries, err := s.store.List(r.Context(), session.ListOptions{
		Limit:          2000,
		Archived:       includeArchived,
		Categories:     categories,
		SortByActivity: true,
	})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to list sessions")
		return
	}
	allowed := make(map[string]session.SessionSummary, len(summaries))
	for _, summary := range summaries {
		allowed[summary.ID] = summary
	}

	matches, err := s.store.Search(r.Context(), ftsQuery, limit*4)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid search query")
		return
	}

	type sessionSearchEntry struct {
		ID            string                `json:"id"`
		Number        int64                 `json:"number,omitempty"`
		Name          string                `json:"name,omitempty"`
		ShortTitle    string                `json:"short_title"`
		LongTitle     string                `json:"long_title"`
		Mode          session.SessionMode   `json:"mode,omitempty"`
		Origin        session.SessionOrigin `json:"origin,omitempty"`
		Provider      string                `json:"provider,omitempty"`
		Archived      bool                  `json:"archived"`
		Pinned        bool                  `json:"pinned"`
		CreatedAt     int64                 `json:"created_at"`
		LastMessageAt int64                 `json:"last_message_at"`
		MsgCount      int                   `json:"message_count"`
		Snippet       string                `json:"snippet,omitempty"`
		MessageID     int64                 `json:"message_id,omitempty"`
	}

	result := make([]sessionSearchEntry, 0, min(limit, len(matches)))
	seen := make(map[string]bool, len(matches))
	for _, match := range matches {
		if seen[match.SessionID] {
			continue
		}
		summary, ok := allowed[match.SessionID]
		if !ok {
			continue
		}
		seen[match.SessionID] = true
		lastMessageAt := sessionSummaryLastMessageAt(summary)
		result = append(result, sessionSearchEntry{
			ID:            summary.ID,
			Number:        summary.Number,
			Name:          summary.Name,
			ShortTitle:    summary.PreferredShortTitle(),
			LongTitle:     summary.PreferredLongTitle(),
			Mode:          summary.Mode,
			Origin:        summary.Origin,
			Provider:      sessionSummaryProviderKey(s.cfgRef, summary),
			Archived:      summary.Archived,
			Pinned:        summary.Pinned,
			CreatedAt:     summary.CreatedAt.UnixMilli(),
			LastMessageAt: lastMessageAt.UnixMilli(),
			MsgCount:      summary.MessageCount,
			Snippet:       match.Snippet,
			MessageID:     match.MessageID,
		})
		if len(result) >= limit {
			break
		}
	}

	writeJSONConditional(w, r, http.StatusOK, map[string]any{"sessions": result})
}

func (s *serveServer) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	// Parse /v1/sessions/{id}/...
	path := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 1 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	sessionID := parts[0]
	// If the path segment is purely numeric, resolve via session number.
	if num, err := strconv.ParseInt(sessionID, 10, 64); err == nil && num > 0 && s.store != nil {
		sess, err := s.store.GetByNumber(r.Context(), num)
		if err != nil || sess == nil {
			http.NotFound(w, r)
			return
		}
		sessionID = sess.ID
	}
	suffix := ""
	if len(parts) > 1 {
		suffix = parts[1]
	}

	if suffix == "" && r.Method == http.MethodPatch {
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		s.handleSessionMetadataPatch(w, r, sessionID)
		return
	}

	if suffix == "interrupt" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		s.handleSessionInterrupt(w, r, sessionID)
		return
	}

	if suffix == "ask_user" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		s.handleSessionAskUser(w, r, sessionID)
		return
	}

	if suffix == "approval" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		s.handleSessionApproval(w, r, sessionID)
		return
	}

	if suffix == "state" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		s.handleSessionState(w, r, sessionID)
		return
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	if suffix != "messages" {
		http.NotFound(w, r)
		return
	}
	if s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}

	limit := parseSessionMessagesLimit(r.URL.Query().Get("limit"))
	offset := parseSessionMessagesOffset(r.URL.Query().Get("offset"))
	msgs, err := s.store.GetMessages(r.Context(), sessionID, limit+1, offset)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to get messages")
		return
	}
	hasMore := len(msgs) > limit
	if hasMore {
		msgs = msgs[:limit]
	}
	nextOffset := offset + len(msgs)

	type partEntry struct {
		Type       string `json:"type"`
		Text       string `json:"text,omitempty"`
		ToolName   string `json:"tool_name,omitempty"`
		ToolArgs   string `json:"tool_arguments,omitempty"`
		ToolCallID string `json:"tool_call_id,omitempty"`
		ImageURL   string `json:"image_url,omitempty"`
		MimeType   string `json:"mime_type,omitempty"`
	}

	type messageEntry struct {
		ID        int64       `json:"id"`
		Role      string      `json:"role"`
		Parts     []partEntry `json:"parts"`
		CreatedAt int64       `json:"created_at"`
	}

	type messagesResponse struct {
		LastResponseID string         `json:"lastResponseId,omitempty"`
		Messages       []messageEntry `json:"messages"`
		HasMore        bool           `json:"has_more"`
		NextOffset     int            `json:"next_offset,omitempty"`
	}

	result := make([]messageEntry, 0, len(msgs))
	for _, msg := range msgs {
		// System and developer messages contain internal prompts — never expose to UI clients.
		if msg.Role == llm.RoleSystem || msg.Role == llm.RoleDeveloper {
			continue
		}
		entry := messageEntry{
			ID:        msg.ID,
			Role:      string(msg.Role),
			CreatedAt: msg.CreatedAt.UnixMilli(),
		}
		if msg.Role == llm.RoleEvent {
			if marker, ok := llm.ParseModelSwapMarker(msg.ToLLMMessage()); ok {
				entry.Parts = append(entry.Parts, partEntry{Type: "model_swap", Text: marker.DisplayText})
			} else {
				for _, p := range msg.Parts {
					if p.Type == llm.PartText && p.Text != "" {
						entry.Parts = append(entry.Parts, partEntry{Type: "text", Text: p.Text})
					}
				}
			}
			if len(entry.Parts) == 0 {
				entry.Parts = []partEntry{}
			}
			result = append(result, entry)
			continue
		}
		for _, p := range msg.Parts {
			switch p.Type {
			case llm.PartText:
				if p.Text != "" {
					entry.Parts = append(entry.Parts, partEntry{
						Type: "text",
						Text: p.Text,
					})
				}
			case llm.PartImage:
				if imageURL := s.sessionMessageImageURL(p); imageURL != "" {
					mimeType := ""
					if p.ImageData != nil {
						mimeType = p.ImageData.MediaType
					}
					entry.Parts = append(entry.Parts, partEntry{
						Type:     "image",
						ImageURL: imageURL,
						MimeType: mimeType,
					})
				}
			case llm.PartToolCall:
				if p.ToolCall != nil {
					pe := partEntry{
						Type:       "tool_call",
						ToolName:   p.ToolCall.Name,
						ToolCallID: p.ToolCall.ID,
					}
					if len(p.ToolCall.Arguments) > 0 {
						pe.ToolArgs = string(p.ToolCall.Arguments)
					}
					entry.Parts = append(entry.Parts, pe)
				}
			case llm.PartToolResult:
				// Omitted: UI ignores tool_result parts and they bloat payloads.
			}
		}
		if len(entry.Parts) == 0 {
			entry.Parts = []partEntry{}
		}
		result = append(result, entry)
	}

	resp := messagesResponse{LastResponseID: latestDurableResponseID(msgs), Messages: result, HasMore: hasMore}
	if hasMore {
		resp.NextOffset = nextOffset
	}
	body, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	etagHash := sha256.New()
	if meta, metaErr := s.store.Get(r.Context(), sessionID); metaErr == nil && meta != nil {
		_, _ = io.WriteString(etagHash, strconv.FormatInt(meta.UpdatedAt.UnixMilli(), 10))
		_, _ = io.WriteString(etagHash, "\n")
	}
	_, _ = etagHash.Write(body)
	etag := `"` + hex.EncodeToString(etagHash.Sum(nil)) + `"`
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	if uiETagMatches(r.Header.Get("If-None-Match"), etag) {
		w.Header().Set("Content-Type", "application/json")
		uiAddVary(w.Header(), "Accept-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSONGzipBody(w, r, http.StatusOK, body)
}

func (s *serveServer) handleSessionInterrupt(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req sessionInterruptRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "message is required")
		return
	}

	rt, ok := s.sessionMgr.Get(sessionID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	if s.responseRuns != nil {
		if runID := s.responseRuns.activeRunID(sessionID); runID != "" {
			if run, ok := s.responseRuns.get(runID); ok {
				run.disableCompaction()
			}
		}
	}

	fastProvider, fastErr := llm.NewFastProvider(s.cfgRef, rt.providerKey)
	if fastErr != nil {
		log.Printf("[serve] fast provider unavailable for interrupt: %v", fastErr)
	}
	action, interruptErr := rt.Interrupt(r.Context(), msg, fastProvider)
	if interruptErr != nil {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", interruptErr.Error())
		return
	}

	actionName := "interject"
	switch action {
	case llm.InterruptCancel:
		actionName = "cancel"
	case llm.InterruptInterject:
		actionName = "interject"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action": actionName,
	})
}

func (s *serveServer) handleSessionMetadataPatch(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}

	var req struct {
		Name     *string `json:"name"`
		Archived *bool   `json:"archived"`
		Pinned   *bool   `json:"pinned"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	sess, err := s.store.Get(r.Context(), sessionID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load session")
		return
	}
	if sess == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}

	if req.Name != nil {
		sess.Name = strings.TrimSpace(*req.Name)
	}
	if req.Archived != nil {
		sess.Archived = *req.Archived
	}
	if req.Pinned != nil {
		sess.Pinned = *req.Pinned
	}
	if err := s.store.Update(r.Context(), sess); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to update session")
		return
	}

	if s.sessionMgr != nil {
		if rt, ok := s.sessionMgr.Get(sessionID); ok {
			rt.mu.Lock()
			if rt.sessionMeta != nil {
				rt.sessionMeta.Name = sess.Name
				rt.sessionMeta.Archived = sess.Archived
				rt.sessionMeta.Pinned = sess.Pinned
				rt.sessionMeta.Origin = sess.Origin
			}
			rt.mu.Unlock()
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          sess.ID,
		"name":        sess.Name,
		"short_title": sess.PreferredShortTitle(),
		"long_title":  sess.PreferredLongTitle(),
		"mode":        sess.Mode,
		"origin":      sess.Origin,
		"archived":    sess.Archived,
		"pinned":      sess.Pinned,
		"created_at":  sess.CreatedAt.UnixMilli(),
	})
}

func (s *serveServer) auth(next http.HandlerFunc) http.HandlerFunc {
	if !s.cfg.requireAuth {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next(w, r)
			return
		}

		var gotToken string
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if scheme, rest, ok := strings.Cut(auth, " "); ok && strings.EqualFold(scheme, "Bearer") {
			gotToken = strings.TrimSpace(rest)
		}
		if gotToken == "" {
			if xKey := strings.TrimSpace(r.Header.Get("x-api-key")); xKey != "" {
				gotToken = xKey
			}
		}
		if gotToken == "" && r.Method == http.MethodGet {
			// Cookie fallback only on safe GET requests (e.g. <img src> fetches
			// that cannot set Authorization headers).
			if cookie, err := r.Cookie("term_llm_token"); err == nil && cookie.Value != "" {
				if decoded, decErr := url.QueryUnescape(cookie.Value); decErr == nil {
					gotToken = decoded
				} else {
					gotToken = cookie.Value
				}
			}
		}

		if gotToken == "" || subtle.ConstantTimeCompare([]byte(gotToken), []byte(s.cfg.token)) != 1 {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid authentication credentials")
			return
		}
		next(w, r)
	}
}

func (s *serveServer) cors(next http.HandlerFunc) http.HandlerFunc {
	allowed := make(map[string]struct{}, len(s.cfg.corsOrigins))
	allowAll := false
	for _, origin := range s.cfg.corsOrigins {
		o := strings.TrimSpace(origin)
		if o == "" {
			continue
		}
		if o == "*" {
			allowAll = true
			continue
		}
		allowed[o] = struct{}{}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if _, ok := allowed[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, session_id, X-Term-LLM-UI-Version, X-API-Key, anthropic-version")
			w.Header().Set("Access-Control-Expose-Headers", "x-session-id, x-session-number, x-response-id, x-term-llm-ui-version")
		}

		w.Header().Set("X-Term-LLM-UI-Version", serveui.AssetVersion())

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next(w, r)
	}
}

func (s *serveServer) handleProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	providers := buildProviderList(s.cfgRef)
	items := make([]map[string]any, 0, len(providers))
	for _, p := range providers {
		if !p.Configured && !p.IsBuiltin {
			continue
		}
		models := p.Models
		if models == nil {
			models = []string{}
		}
		defaultModel := ""
		if pc, ok := s.cfgRef.Providers[p.Name]; ok {
			defaultModel = strings.TrimSpace(pc.Model)
		}
		items = append(items, map[string]any{
			"name":          p.Name,
			"type":          p.Type,
			"models":        models,
			"configured":    p.Configured,
			"is_builtin":    p.IsBuiltin,
			"is_default":    p.Name == s.cfgRef.DefaultProvider,
			"default_model": defaultModel,
		})
	}

	writeJSONConditional(w, r, http.StatusOK, map[string]any{
		"object": "list",
		"data":   items,
	})
}

const serveModelsCacheTTL = 15 * time.Minute

type serveModelsCacheEntry struct {
	models    []llm.ModelInfo
	expiresAt time.Time
}

func (s *serveServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	queryProvider := strings.TrimSpace(r.URL.Query().Get("provider"))
	provider, effectiveName, err := s.getModelsProvider(queryProvider)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	models := make([]llm.ModelInfo, 0)
	// OpenRouter ships hundreds of models and maintains a provider-side warm
	// cache; use it first when available. For other providers, preserve the
	// OpenAI-compatible /v1/models behavior: prefer a fresh/cache upstream
	// ListModels result, and use local curated/configured lists only as a
	// fallback when upstream listing is unavailable or fails.
	pc, hasCfg := s.cfgRef.Providers[effectiveName]
	isOpenRouter := effectiveName == "openrouter" || (hasCfg && string(pc.Type) == "openrouter")
	if isOpenRouter {
		apiKey := ""
		if hasCfg {
			apiKey = pc.ResolvedAPIKey
		}
		for _, id := range llm.GetCachedOpenRouterModels(apiKey) {
			models = append(models, llm.ModelInfo{ID: id})
		}
	}
	if len(models) == 0 {
		models = s.getCachedModelsForProvider(effectiveName)
	}
	if len(models) == 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if lister, ok := provider.(interface {
			ListModels(context.Context) ([]llm.ModelInfo, error)
		}); ok {
			listed, err := lister.ListModels(ctx)
			if err == nil {
				s.setCachedModelsForProvider(effectiveName, listed)
				models = listed
			} else if !errors.Is(err, llm.ErrListModelsUnsupported) {
				s.verboseLog("ListModels(%q) failed: %v", effectiveName, err)
			}
		}
	}
	if len(models) == 0 {
		models = s.getLocalModelsForProvider(effectiveName, queryProvider)
	}

	ids := make([]string, 0, len(models))
	byID := make(map[string]llm.ModelInfo, len(models))
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		if _, ok := byID[m.ID]; ok {
			continue
		}
		byID[m.ID] = m
		ids = append(ids, m.ID)
	}
	// The web UI has a dedicated reasoning-effort selector, so drop
	// "<base>-<effort>" aliases when the base model is also present.
	ids = llm.DedupeEffortVariants(ids)

	// Order: configured default first, then curated models in their authored
	// (popular-first) order, then anything else alpha-sorted. Pure alpha sort
	// buries the most-used models behind less-used variants.
	defaultModel := ""
	if pc, ok := s.cfgRef.Providers[effectiveName]; ok {
		defaultModel = pc.Model
	}
	ids = llm.SortModelIDsByPopularity(effectiveName, defaultModel, ids)

	items := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		m, ok := byID[id]
		if !ok {
			continue
		}
		items = append(items, map[string]any{
			"id":      m.ID,
			"object":  "model",
			"created": m.Created,
			"owned_by": func() string {
				if m.OwnedBy != "" {
					return m.OwnedBy
				}
				return "term-llm"
			}(),
		})
	}

	writeJSONConditional(w, r, http.StatusOK, map[string]any{
		"object": "list",
		"data":   items,
	})
}

func appendModelIDs(dst []llm.ModelInfo, ids []string) []llm.ModelInfo {
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		dst = append(dst, llm.ModelInfo{ID: id})
	}
	return dst
}

func cloneModelInfos(models []llm.ModelInfo) []llm.ModelInfo {
	if len(models) == 0 {
		return nil
	}
	cloned := make([]llm.ModelInfo, len(models))
	copy(cloned, models)
	return cloned
}

func (s *serveServer) getLocalModelsForProvider(effectiveName, queryProvider string) []llm.ModelInfo {
	models := make([]llm.ModelInfo, 0)
	if pc, ok := s.cfgRef.Providers[effectiveName]; ok {
		models = appendModelIDs(models, pc.Models)
		if id := strings.TrimSpace(pc.Model); id != "" {
			models = append(models, llm.ModelInfo{ID: id})
		}
	} else if queryProvider == "" {
		if providerCfg := s.cfgRef.GetActiveProviderConfig(); providerCfg != nil {
			models = appendModelIDs(models, providerCfg.Models)
			if id := strings.TrimSpace(providerCfg.Model); id != "" {
				models = append(models, llm.ModelInfo{ID: id})
			}
		}
	}
	return appendModelIDs(models, llm.ResolveProviderModelIDs(effectiveName))
}

func (s *serveServer) getCachedModelsForProvider(name string) []llm.ModelInfo {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()

	if entry, ok := s.modelsCache[name]; ok {
		if time.Now().Before(entry.expiresAt) {
			return cloneModelInfos(entry.models)
		}
		delete(s.modelsCache, name)
	}
	return nil
}

func (s *serveServer) setCachedModelsForProvider(name string, models []llm.ModelInfo) {
	if len(models) == 0 {
		return
	}

	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()

	if s.modelsCache == nil {
		s.modelsCache = make(map[string]serveModelsCacheEntry)
	}
	s.modelsCache[name] = serveModelsCacheEntry{
		models:    cloneModelInfos(models),
		expiresAt: time.Now().Add(serveModelsCacheTTL),
	}
}

func (s *serveServer) getModelsProvider(name string) (llm.Provider, string, error) {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()

	if s.modelsProviders == nil {
		s.modelsProviders = make(map[string]llm.Provider)
	}

	cacheKey := name
	if cacheKey == "" {
		cacheKey = s.cfgRef.DefaultProvider
	}

	if p, ok := s.modelsProviders[cacheKey]; ok {
		return p, cacheKey, nil
	}

	var provider llm.Provider
	var err error
	if name == "" || name == s.cfgRef.DefaultProvider {
		provider, err = llm.NewProvider(s.cfgRef)
	} else {
		provider, err = llm.NewProviderByName(s.cfgRef, name, "")
	}
	if err != nil {
		return nil, "", err
	}
	s.modelsProviders[cacheKey] = provider
	return provider, cacheKey, nil
}

// sseKeepalive starts a background goroutine that writes an SSE comment ping
// to w every interval while streaming is active. This prevents intermediate
// proxies (e.g. nginx with a short send_timeout) from closing the connection
// during silent periods — e.g. when the LLM is in extended thinking mode or
// the API is slow to emit tokens.
//
// The returned mu must wrap all writes to w inside the RunWithEvents callback.
// Call stop() immediately after RunWithEvents returns; it blocks until the
// goroutine has exited so subsequent final writes to w are safe without a lock.
func sseKeepalive(w http.ResponseWriter, flusher http.Flusher, interval time.Duration) (mu *sync.Mutex, stop func()) {
	mu = &sync.Mutex{}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				_, _ = io.WriteString(w, ": ping\n\n")
				flusher.Flush()
				mu.Unlock()
			case <-done:
				return
			}
		}
	}()
	return mu, func() {
		close(done)
		wg.Wait()
	}
}

// registerResponseID stores a response ID on the runtime and server-wide map,
// pruning old IDs that exceed the per-session cap.
func (s *serveServer) registerResponseID(rt *serveRuntime, respID, sessionID string) {
	pruned := rt.addResponseID(respID)
	s.responseToSession.Store(respID, sessionID)
	if sessionID != "" {
		s.sessionToResponse.Store(sessionID, respID)
	}
	for _, old := range pruned {
		s.responseToSession.Delete(old)
	}
}

func (s *serveServer) unregisterResponseIDs(rt *serveRuntime) {
	if rt == nil {
		return
	}
	for _, rid := range rt.getResponseIDs() {
		s.responseToSession.Delete(rid)
	}
}

func (s *serveServer) unregisterSessionResponseIDs(sessionID string) {
	if sessionID == "" {
		return
	}
	s.sessionToResponse.Delete(sessionID)
	s.responseToSession.Range(func(key, value any) bool {
		sid, ok := value.(string)
		if ok && sid == sessionID {
			s.responseToSession.Delete(key)
		}
		return true
	})
}

func (s *serveServer) runtimeForRequest(ctx context.Context, sessionID string) (*serveRuntime, bool, error) {
	if sessionID == "" {
		// Ephemeral stateless runtime (fresh per request for isolation)
		rt, err := s.sessionMgr.factory(ctx)
		if err != nil {
			return nil, false, err
		}
		return rt, false, nil
	}
	// Stateful sessions should persist beyond a single HTTP request, but
	// creation must still respect request cancellation/timeouts.
	rt, err := s.sessionMgr.GetOrCreate(ctx, sessionID)
	if err != nil {
		return nil, false, err
	}
	return rt, true, nil
}

func runtimeProviderKey(rt *serveRuntime) string {
	if rt == nil {
		return ""
	}
	provider := strings.TrimSpace(rt.providerKey)
	if provider == "" && rt.provider != nil {
		provider = strings.TrimSpace(rt.provider.Name())
	}
	return provider
}

// runtimeForProviderRequest creates a runtime using a specific (non-default) provider.
func (s *serveServer) runtimeForProviderRequest(ctx context.Context, sessionID string, providerName string) (*serveRuntime, bool, error) {
	return s.runtimeForProviderModelRequest(ctx, sessionID, providerName, "")
}

func (s *serveServer) runtimeForProviderModelRequest(ctx context.Context, sessionID string, providerName string, modelName string) (*serveRuntime, bool, error) {
	if s.runtimeFactory == nil {
		return s.runtimeForRequest(ctx, sessionID)
	}
	providerName = strings.TrimSpace(providerName)
	modelName = strings.TrimSpace(modelName)
	if sessionID == "" {
		rt, err := s.runtimeFactory(ctx, providerName, modelName)
		if err != nil {
			return nil, false, err
		}
		return rt, false, nil
	}
	// Check persisted session provider before creating/reusing a runtime.
	// This is the authoritative source — it survives runtime eviction and
	// server restarts, unlike the in-memory providerKey on the runtime.
	if s.store != nil && providerName != "" {
		if sess, err := s.store.Get(ctx, sessionID); err == nil && sess != nil {
			storedProvider := strings.TrimSpace(sess.ProviderKey)
			if storedProvider == "" {
				storedProvider = resolveSessionProviderKey(s.cfgRef, sess)
			}
			if storedProvider != "" && storedProvider != providerName {
				return nil, false, fmt.Errorf("session %q already uses provider %q (requested %q)", sessionID, storedProvider, providerName)
			}
		}
	}
	// Use GetOrCreateWith to get proper in-flight deduplication.
	rt, err := s.sessionMgr.GetOrCreateWith(ctx, sessionID, func(ctx context.Context) (*serveRuntime, error) {
		return s.runtimeFactory(ctx, providerName, modelName)
	})
	if err != nil {
		return nil, false, err
	}
	// Belt-and-suspenders: also check the live runtime in case the store
	// missed (new session not yet persisted, store error, etc.).
	existingProvider := runtimeProviderKey(rt)
	if existingProvider != "" && providerName != "" && existingProvider != providerName {
		return nil, false, fmt.Errorf("session %q already uses provider %q (requested %q)", sessionID, existingProvider, providerName)
	}
	return rt, true, nil
}

// runtimeForFreshProviderRequest starts a fresh conversation, optionally using
// a specific provider, even when the caller reuses an existing session ID.
func (s *serveServer) runtimeForFreshProviderRequest(ctx context.Context, sessionID string, providerName string) (*serveRuntime, bool, error) {
	defaultProvider := ""
	if s.cfgRef != nil {
		defaultProvider = strings.TrimSpace(s.cfgRef.DefaultProvider)
	}
	providerName = strings.TrimSpace(providerName)
	desiredProvider := providerName
	if desiredProvider == "" {
		desiredProvider = defaultProvider
	}
	if s.runtimeFactory == nil && providerName != "" && providerName != defaultProvider {
		desiredProvider = ""
	}
	create := s.sessionMgr.factory
	if s.runtimeFactory != nil && providerName != "" && providerName != defaultProvider {
		create = func(ctx context.Context) (*serveRuntime, error) {
			return s.runtimeFactory(ctx, providerName, "")
		}
	}
	if sessionID == "" {
		rt, err := create(ctx)
		if err != nil {
			return nil, false, err
		}
		return rt, false, nil
	}
	rt, err := s.sessionMgr.ReplaceIdleWith(ctx, sessionID,
		func(existing *serveRuntime) bool {
			return true
		},
		create,
	)
	if err != nil {
		return nil, false, err
	}
	existingProvider := runtimeProviderKey(rt)
	if existingProvider != "" && desiredProvider != "" && existingProvider != desiredProvider {
		return nil, false, fmt.Errorf("session %q already uses provider %q (requested %q)", sessionID, existingProvider, desiredProvider)
	}
	return rt, true, nil
}

// syncPersistedSessionRuntime pins the provider, model, and reasoning_effort
// for the current fresh web conversation. A client may start a fresh
// conversation while reusing an existing session ID, so the persisted session
// row must be updated to match the replacement runtime instead of leaving
// stale provider/model metadata behind from the prior conversation. If the row
// does not yet exist, it is created here so the client-supplied model and
// effort are persisted (rather than the runtime defaults that rt would
// otherwise use when Run creates the row).
func (s *serveServer) syncPersistedSessionRuntime(ctx context.Context, sessionID string, rt *serveRuntime, clientModel, reasoningEffort string) {
	if s.store == nil || sessionID == "" || rt == nil {
		return
	}
	providerKey := strings.TrimSpace(rt.providerKey)
	providerName := providerKey
	if rt.provider != nil {
		if name := strings.TrimSpace(rt.provider.Name()); name != "" {
			providerName = name
		}
	}
	// Prefer the client-requested model: the client's first-message choice is
	// what gets locked. Fall back to the runtime's default only when the
	// client didn't send one.
	modelName := strings.TrimSpace(clientModel)
	if modelName == "" {
		modelName = strings.TrimSpace(rt.defaultModel)
	}
	effort := strings.TrimSpace(reasoningEffort)

	sess, err := s.store.Get(ctx, sessionID)
	if err != nil {
		return
	}
	if sess == nil {
		sess = &session.Session{
			ID:          sessionID,
			Provider:    providerName,
			ProviderKey: providerKey,
			Model:       modelName,
			Mode:        session.ModeChat,
			Origin:      session.OriginWeb,
			Agent:       rt.agentName,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
			Search:      rt.search,
			Tools:       rt.toolsSetting,
			MCP:         rt.mcpSetting,
			Status:      session.StatusActive,
		}
		if effort != "" {
			sess.ReasoningEffort = effort
		}
		if cwd, cwdErr := os.Getwd(); cwdErr == nil {
			sess.CWD = cwd
		}
		if createErr := s.store.Create(ctx, sess); createErr != nil {
			if existing, getErr := s.store.Get(ctx, sessionID); getErr == nil && existing != nil {
				sess = existing
			} else {
				log.Printf("[serve] session Create failed for %s: %v", sessionID, createErr)
				return
			}
		} else {
			rt.mu.Lock()
			rt.sessionMeta = sess
			rt.mu.Unlock()
			return
		}
	}

	changed := false
	if strings.TrimSpace(sess.Provider) != providerName {
		sess.Provider = providerName
		changed = true
	}
	if strings.TrimSpace(sess.ProviderKey) != providerKey {
		sess.ProviderKey = providerKey
		changed = true
	}
	if strings.TrimSpace(sess.Model) != modelName {
		sess.Model = modelName
		changed = true
	}
	if strings.TrimSpace(sess.ReasoningEffort) != effort {
		sess.ReasoningEffort = effort
		changed = true
	}
	if !changed {
		return
	}
	if err := s.store.Update(ctx, sess); err != nil {
		log.Printf("[serve] session Update failed for %s: %v", sessionID, err)
		return
	}
	rt.mu.Lock()
	rt.sessionMeta = sess
	rt.mu.Unlock()
}

func (s *serveServer) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "session store not available")
		return
	}

	switch r.Method {
	case http.MethodPost:
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		var req struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256DH string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
		}
		if err := decodeJSONBody(r, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if req.Endpoint == "" || req.Keys.P256DH == "" || req.Keys.Auth == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "endpoint and keys (p256dh, auth) are required")
			return
		}
		sub := &session.PushSubscription{
			Endpoint:  req.Endpoint,
			KeyP256DH: req.Keys.P256DH,
			KeyAuth:   req.Keys.Auth,
		}
		if err := s.store.SavePushSubscription(r.Context(), sub); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to save subscription")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"ok": true})

	case http.MethodDelete:
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		var req struct {
			Endpoint string `json:"endpoint"`
		}
		if err := decodeJSONBody(r, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if req.Endpoint == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "endpoint is required")
			return
		}
		if err := s.store.DeletePushSubscription(r.Context(), req.Endpoint); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to delete subscription")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		w.Header().Set("Allow", "POST, DELETE")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// POST /v1/messages — Anthropic Messages API
// ---------------------------------------------------------------------------

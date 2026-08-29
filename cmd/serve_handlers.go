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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/image"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/serveui"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/sessiontitle"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/worktree"
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
	resp := map[string]any{"status": "ok"}
	// Identity fields (agent, version, capabilities) are only reported to
	// trusted callers — a valid bearer token, or any caller when auth is
	// disabled — so the unauthenticated health probe stays anonymous. The hub
	// prober sends the node token and uses these for its dashboard.
	if s.healthIdentityTrusted(r) {
		resp["agent"] = s.cfg.agentName
		resp["version"] = Version
		resp["capabilities"] = s.capabilityList()
	}
	writeJSON(w, http.StatusOK, resp)
}

// healthIdentityTrusted reports whether the health request may receive node
// identity fields: either auth is disabled (loopback-only serves), or the
// request carries the serve's bearer token.
func (s *serveServer) healthIdentityTrusted(r *http.Request) bool {
	if !s.cfg.requireAuth {
		return true
	}
	if s.cfg.token == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, prefix)), []byte(s.cfg.token)) == 1
}

// capabilityList describes what this serve exposes, for hub discovery.
func (s *serveServer) capabilityList() []string {
	caps := []string{}
	if s.cfg.ui {
		caps = append(caps, "web")
	}
	if s.cfg.api {
		caps = append(caps, "api")
	}
	if s.jobsV2 != nil {
		caps = append(caps, "jobs")
	}
	if s.widgetsMgr != nil {
		caps = append(caps, "widgets")
	}
	if s.webrtcEnabled {
		caps = append(caps, "voice")
	}
	return caps
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
			extension := filepath.Ext(assetName)
			contentType := mime.TypeByExtension(extension)
			switch extension {
			case ".js", ".mjs":
				contentType = "text/javascript; charset=utf-8"
			case ".css":
				contentType = "text/css; charset=utf-8"
			case ".woff2":
				contentType = "font/woff2"
			default:
				if contentType == "" {
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
		if strings.HasPrefix(assetName, "dist/") {
			http.NotFound(w, r)
			return
		}
		if extension := filepath.Ext(assetName); extension == ".js" || extension == ".mjs" || extension == ".css" {
			http.NotFound(w, r)
			return
		}
	}

	// SPA catch-all: serve index.html only for extensionless application routes.
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
	agentEscaped, _ := json.Marshal(s.cfg.agentName)
	headSnippet += `<script>window.TERM_LLM_AGENT_NAME=` + string(agentEscaped) + `;</script>`
	agentNamesEscaped, _ := json.Marshal(s.cfg.agentNames)
	headSnippet += `<script>window.TERM_LLM_AGENT_NAMES=` + string(agentNamesEscaped) + `;</script>`
	titleEscaped, _ := json.Marshal(s.cfg.uiTitle)
	headSnippet += `<script>window.TERM_LLM_UI_TITLE=` + string(titleEscaped) + `;</script>`
	headSnippet += `<script>window.TERM_LLM_LOCATION_SHARING_ENABLED=` + strconv.FormatBool(!s.cfg.locationSharingDisabled) + `;</script>`
	_, worktreeRootErr := s.currentGitRoot()
	headSnippet += `<script>window.TERM_LLM_WORKTREES_ENABLED=` + strconv.FormatBool(worktreeRootErr == nil) + `;</script>`
	if s.cfg.hubURL != "" {
		hubEscaped, _ := json.Marshal(map[string]string{
			"url":      s.cfg.hubURL,
			"nodeId":   s.cfg.hubNodeID,
			"nodeName": s.cfg.hubNodeName,
		})
		headSnippet += `<script>window.TERM_LLM_HUB=` + string(hubEscaped) + `;</script>`
	}
	if s.cfgRef != nil {
		if vapidKey := s.cfgRef.Serve.WebPush.VAPIDPublicKey; vapidKey != "" {
			vapidEscaped, _ := json.Marshal(vapidKey)
			headSnippet += `<script>window.TERM_LLM_VAPID_PUBLIC_KEY=` + string(vapidEscaped) + `;</script>`
		}
	}
	_, pushPersistent := session.AsPushSubscriptionLifecycleStore(s.store)
	headSnippet += `<script>window.TERM_LLM_PUSH_SUPPORTED=` + strconv.FormatBool(pushPersistent) + `;</script>`
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
			"dist/app.css",
			"dist/app.js",
			"dist/chunks/vendor.js",
		}
		if s.webrtcEnabled {
			assetNames = append(assetNames, "dist/chunks/webrtc.js")
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

func pathWithinDirForOS(path, dir, goos string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	if goos == "windows" {
		path, dir = strings.ToLower(path), strings.ToLower(dir)
	}
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

func pathWithinDir(path, dir string) bool {
	return pathWithinDirForOS(path, dir, runtime.GOOS)
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

func parseSessionMessagesTail(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}

func parseSessionMessagesBeforeSeq(raw string) int {
	seq, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || seq < 0 {
		return 0
	}
	return seq
}

const maxWebSpawnAgentOutputBytes = 64 * 1024

func boundedWebSpawnAgentText(value string, maxBytes int, suffix string) string {
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + suffix
}

func boundedWebSpawnAgentOutput(output string) string {
	return boundedWebSpawnAgentText(output, maxWebSpawnAgentOutputBytes, "\n… (output truncated)")
}

func (s *serveServer) validatedSpawnChildID(parentSessionID, childSessionID string) string {
	parentSessionID = strings.TrimSpace(parentSessionID)
	childSessionID = strings.TrimSpace(childSessionID)
	if s == nil || s.store == nil || parentSessionID == "" || childSessionID == "" || childSessionID == parentSessionID {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	child, err := s.store.Get(ctx, childSessionID)
	if err != nil || child == nil || child.ParentID != parentSessionID {
		return ""
	}
	return childSessionID
}

type sessionMessagePartEntry struct {
	Type            string                         `json:"type"`
	Text            string                         `json:"text,omitempty"`
	SkillActivation *llm.SkillActivationProvenance `json:"skill_activation,omitempty"`
	PathNote        *llm.PathNoteProvenance        `json:"path_note,omitempty"`
	DiffComment     *llm.DiffComment               `json:"diff_comment,omitempty"`
	ToolName        string                         `json:"tool_name,omitempty"`
	ToolInfo        string                         `json:"tool_info,omitempty"`
	ToolArgs        string                         `json:"tool_arguments,omitempty"`
	ToolCallID      string                         `json:"tool_call_id,omitempty"`
	ToolStatus      string                         `json:"tool_status,omitempty"`
	AskUserSummary  string                         `json:"ask_user_summary,omitempty"`
	ImageURL        string                         `json:"image_url,omitempty"`
	Images          []string                       `json:"images,omitempty"`
	ToolError       bool                           `json:"tool_error,omitempty"`
	GuardianReviews []llm.GuardianReview           `json:"guardian_reviews,omitempty"`
	SpawnAgent      *tools.SpawnAgentResult        `json:"spawn_agent,omitempty"`
	MimeType        string                         `json:"mime_type,omitempty"`
	Width           int                            `json:"width,omitempty"`
	Height          int                            `json:"height,omitempty"`
}

type sessionMessageEntry struct {
	ID                      int64                     `json:"id"`
	Sequence                int                       `json:"sequence"`
	Role                    string                    `json:"role"`
	Parts                   []sessionMessagePartEntry `json:"parts"`
	CreatedAt               int64                     `json:"created_at"`
	CompactionTail          bool                      `json:"compaction_tail,omitempty"`
	ClientMessageID         string                    `json:"client_message_id,omitempty"`
	ResponseID              string                    `json:"response_id,omitempty"`
	AssistantSegmentOrdinal *int                      `json:"assistant_segment_ordinal,omitempty"`
	SegmentStartSequence    int64                     `json:"segment_start_sequence,omitempty"`
	SegmentEndSequence      int64                     `json:"segment_end_sequence,omitempty"`
}

type sessionDiffCommentEntry struct {
	MessageID       int64            `json:"message_id"`
	ClientMessageID string           `json:"client_message_id,omitempty"`
	CreatedAt       int64            `json:"created_at"`
	Comment         *llm.DiffComment `json:"diff_comment"`
}

func (s *serveServer) handleSessionDiffComments(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}
	sess, err := s.store.Get(r.Context(), sessionID)
	if err != nil || sess == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	lister, ok := s.store.(session.DiffCommentMessageLister)
	if !ok {
		writeOpenAIError(w, http.StatusNotImplemented, "server_error", "session history does not support inline comment lookup")
		return
	}
	revision := int64(-1)
	if reader, ok := s.store.(interface {
		TranscriptRev(context.Context, string) (int64, error)
	}); ok {
		revision, err = reader.TranscriptRev(r.Context(), sessionID)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to get diff comment revision")
			return
		}
	}
	messages, err := lister.GetDiffCommentMessages(r.Context(), sessionID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to get diff comments")
		return
	}
	comments := make([]sessionDiffCommentEntry, 0)
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Type != llm.PartDiffComment || part.DiffComment == nil {
				continue
			}
			comment := *part.DiffComment
			comment.ContextBefore = append([]llm.DiffCommentContextLine(nil), part.DiffComment.ContextBefore...)
			comment.ContextAfter = append([]llm.DiffCommentContextLine(nil), part.DiffComment.ContextAfter...)
			comments = append(comments, sessionDiffCommentEntry{
				MessageID:       message.ID,
				ClientMessageID: message.ClientMessageID,
				CreatedAt:       message.CreatedAt.UnixMilli(),
				Comment:         &comment,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"comments":       comments,
		"transcript_rev": revision,
	})
}

type sessionMessagesResponse struct {
	LastResponseID  string                `json:"lastResponseId,omitempty"`
	Messages        []sessionMessageEntry `json:"messages"`
	HasMore         bool                  `json:"has_more"`
	NextOffset      int                   `json:"next_offset,omitempty"`
	NextBeforeSeq   int                   `json:"next_before_seq,omitempty"`
	CompactionSeq   *int                  `json:"compaction_seq,omitempty"`
	CompactionCount int                   `json:"compaction_count,omitempty"`
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

func imageDimensionsFromReader(reader io.Reader) (width, height int) {
	metadata, err := io.ReadAll(io.LimitReader(reader, maxImageMetadataBytes))
	if err != nil {
		return 0, 0
	}
	return imageDisplayDimensions(metadata)
}

func sessionMessageImageDimensions(part llm.Part, serveablePath string) (width, height int) {
	if part.ImageData != nil && part.ImageData.Width > 0 && part.ImageData.Height > 0 {
		return part.ImageData.Width, part.ImageData.Height
	}
	if part.ImageData != nil && strings.TrimSpace(part.ImageData.Base64) != "" {
		decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(part.ImageData.Base64))
		if width, height := imageDimensionsFromReader(decoder); width > 0 && height > 0 {
			return width, height
		}
	}
	// The caller supplies only a canonical path already approved by
	// ensureImageServeable; never inspect an arbitrary path from session data.
	if strings.TrimSpace(serveablePath) == "" {
		return 0, 0
	}
	f, err := os.Open(serveablePath)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	return imageDimensionsFromReader(f)
}

func (s *serveServer) sessionMessageImageURL(part llm.Part) (url, serveablePath string) {
	if part.ImagePath != "" {
		if served, ok := s.ensureImageServeable(part.ImagePath); ok {
			return serveRoutePath(s.cfg.imagesRoute(), s.imageOutputDir(), served), served
		}
	}
	if part.ImageData == nil || strings.TrimSpace(part.ImageData.Base64) == "" {
		return "", ""
	}
	if served, ok := s.materializeInlineSessionImage(part.ImageData.MediaType, part.ImageData.Base64); ok {
		return serveRoutePath(s.cfg.imagesRoute(), s.imageOutputDir(), served), served
	}
	return "", ""
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

type webFileChangeSummary struct {
	FileCount int  `json:"file_count"`
	Adds      int  `json:"adds"`
	Dels      int  `json:"dels"`
	Git       bool `json:"git"`
}

type webSessionEntry struct {
	ID            string                `json:"id"`
	Number        int64                 `json:"number,omitempty"`
	Name          string                `json:"name,omitempty"`
	ShortTitle    string                `json:"short_title"`
	LongTitle     string                `json:"long_title"`
	Mode          session.SessionMode   `json:"mode,omitempty"`
	Origin        session.SessionOrigin `json:"origin,omitempty"`
	Provider      string                `json:"provider,omitempty"`
	Model         string                `json:"model,omitempty"`
	ProjectID     string                `json:"project_id,omitempty"`
	ProjectName   string                `json:"project_name,omitempty"`
	CWD           string                `json:"cwd,omitempty"`
	WorktreeDir   string                `json:"worktree_dir,omitempty"`
	Archived      bool                  `json:"archived"`
	Pinned        bool                  `json:"pinned"`
	CreatedAt     int64                 `json:"created_at"`
	LastMessageAt int64                 `json:"last_message_at"`
	MsgCount      int                   `json:"message_count"`
}

type webSelectedSessionEntry struct {
	webSessionEntry
	FileChangeSummary webFileChangeSummary `json:"file_change_summary"`
	PlanSummary       *webPlanSummary      `json:"plan_summary"`
}

func (s *serveServer) webSessionEntryFromSummary(sess session.SessionSummary) webSessionEntry {
	return webSessionEntry{
		Name:          sess.Name,
		ID:            sess.ID,
		Number:        sess.Number,
		ShortTitle:    sess.PreferredShortTitle(),
		LongTitle:     sess.PreferredLongTitle(),
		Mode:          sess.Mode,
		Origin:        sess.Origin,
		Provider:      sessionSummaryProviderKey(s.cfgRef, sess),
		Model:         sess.Model,
		ProjectID:     sess.ProjectID,
		ProjectName:   sess.ProjectName,
		CWD:           sess.CWD,
		WorktreeDir:   sess.WorktreeDir,
		Archived:      sess.Archived,
		Pinned:        sess.Pinned,
		CreatedAt:     sess.CreatedAt.UnixMilli(),
		LastMessageAt: sessionSummaryLastMessageAt(sess).UnixMilli(),
		MsgCount:      sess.MessageCount,
	}
}

func (s *serveServer) webSessionEntryFromSession(sess *session.Session) webSessionEntry {
	lastMessageAt := sess.UpdatedAt
	if lastMessageAt.IsZero() {
		lastMessageAt = sess.CreatedAt
	}
	provider := strings.TrimSpace(sess.ProviderKey)
	if provider == "" {
		provider = resolveSessionProviderKey(s.cfgRef, sess)
	}
	return webSessionEntry{
		Name:          sess.Name,
		ID:            sess.ID,
		Number:        sess.Number,
		ShortTitle:    sess.PreferredShortTitle(),
		LongTitle:     sess.PreferredLongTitle(),
		Mode:          sess.Mode,
		Origin:        sess.Origin,
		Provider:      provider,
		Model:         sess.Model,
		ProjectID:     sess.ProjectID,
		ProjectName:   sess.ProjectName,
		CWD:           sess.CWD,
		WorktreeDir:   sess.WorktreeDir,
		Archived:      sess.Archived,
		Pinned:        sess.Pinned,
		CreatedAt:     sess.CreatedAt.UnixMilli(),
		LastMessageAt: lastMessageAt.UnixMilli(),
		MsgCount:      sess.MessageCount,
	}
}

func (s *serveServer) selectedWebSession(ctx context.Context, selector string, summaries []session.SessionSummary) (*webSelectedSessionEntry, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, nil
	}

	var selected webSessionEntry
	if number, err := strconv.ParseInt(selector, 10, 64); err == nil && number > 0 {
		for _, summary := range summaries {
			if summary.Number == number {
				selected = s.webSessionEntryFromSummary(summary)
				break
			}
		}
		if selected.ID == "" {
			sess, err := s.store.GetByNumber(ctx, number)
			if err != nil || sess == nil {
				return nil, nil
			}
			selected = s.webSessionEntryFromSession(sess)
		}
	} else {
		for _, summary := range summaries {
			if summary.ID == selector {
				selected = s.webSessionEntryFromSummary(summary)
				break
			}
		}
		if selected.ID == "" {
			sess, err := s.store.Get(ctx, selector)
			if err != nil || sess == nil {
				return nil, nil
			}
			selected = s.webSessionEntryFromSession(sess)
		}
	}

	result := &webSelectedSessionEntry{webSessionEntry: selected}
	if fileStore := s.fileTrackStore(); fileStore != nil {
		_, result.FileChangeSummary.Git = s.sessionGitRepo(ctx, selected.ID)
		changes, err := fileStore.ListRecentRunChanges(ctx, selected.ID, 1)
		if err != nil {
			return nil, fmt.Errorf("summarize selected session file changes: %w", err)
		}
		result.FileChangeSummary.FileCount = len(changes)
		for _, change := range changes {
			result.FileChangeSummary.Adds += change.Adds
			result.FileChangeSummary.Dels += change.Dels
		}
	}
	if planStore, ok := planSnapshotStoreForWeb(s.store); ok {
		snapshot, version, err := planStore.LoadPlanSnapshot(ctx, selected.ID)
		if err != nil {
			return nil, fmt.Errorf("load selected session plan: %w", err)
		}
		result.PlanSummary = summarizeWebPlan(snapshot, version)
	}
	return result, nil
}

func (s *serveServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	selectedOnly := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("selected_only")), "1") ||
		strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("selected_only")), "true")
	includeWidgetStatus := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_widget_status")), "1") ||
		strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_widget_status")), "true")
	includeTranscript := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_transcript")), "1") ||
		strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_transcript")), "true")
	selectedSelector := strings.TrimSpace(r.URL.Query().Get("selected_session"))
	if selectedOnly && selectedSelector == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "selected_session is required with selected_only")
		return
	}
	if includeTranscript && selectedSelector == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "selected_session is required with include_transcript")
		return
	}

	var sessions []session.SessionSummary
	nextCursor := ""
	if !selectedOnly {
		categories, err := parseSidebarSessionCategories(r.URL.Query().Get("categories"), false)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		includeArchived := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "1") ||
			strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "true")

		projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
		projectCursorValue := strings.TrimSpace(r.URL.Query().Get("cursor"))
		var projectCursor *session.ProjectSessionCursor
		if projectCursorValue != "" {
			cursor, decodeErr := session.DecodeProjectSessionCursor(projectCursorValue)
			if decodeErr != nil || cursor.ProjectID != projectID {
				writeProjectError(w, http.StatusBadRequest, "invalid_cursor", "session cursor does not belong to this project group")
				return
			}
			projectCursor = &cursor
		}
		groupPage := projectID != "" || projectCursor != nil
		// An explicit limit opts the flat listing into keyset pagination so
		// the projects-disabled sidebar can scroll infinitely like project
		// groups do; legacy callers without a limit keep the bounded snapshot.
		rawLimit := strings.TrimSpace(r.URL.Query().Get("limit"))
		paged := groupPage || rawLimit != ""
		// Ungrouped pages cover the project-less sessions (the "No project"
		// sidebar group) while projects are enabled, and every session when
		// projects are disabled and the sidebar is one flat list.
		noProject := paged && projectID == "" && s.projectsEnabled
		limit := 100
		if paged {
			limit = 13
			if rawLimit != "" {
				if parsed, parseErr := strconv.Atoi(rawLimit); parseErr == nil && parsed > 0 {
					limit = min(parsed, 100) + 1
				}
			}
		}
		sessions, err = s.store.List(r.Context(), session.ListOptions{
			Limit:            limit,
			Archived:         includeArchived,
			Categories:       categories,
			SortByActivity:   true,
			ProjectID:        projectID,
			NoProject:        noProject,
			ProjectCursor:    projectCursor,
			ExcludeSubagents: true,
		})
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to list sessions")
			return
		}
		if paged && len(sessions) == limit {
			pageSize := limit - 1
			boundary := sessions[pageSize-1]
			if projectID == "" {
				// Ungrouped-listing cursors stay bound to the ungrouped listing
				// even when the boundary session carries a legacy project ID.
				boundary.ProjectID = ""
			}
			nextCursor = session.EncodeProjectSessionCursor(boundary)
			sessions = sessions[:pageSize]
		}
	}

	result := make([]webSessionEntry, 0, len(sessions))
	for _, sess := range sessions {
		result = append(result, s.webSessionEntryFromSummary(sess))
	}

	selected, err := s.selectedWebSession(r.Context(), selectedSelector, sessions)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load selected session metadata")
		return
	}

	payload := map[string]any{
		"sessions":         result,
		"selected_session": selected,
	}
	if nextCursor != "" {
		payload["next_cursor"] = nextCursor
	}
	if includeWidgetStatus {
		// The shell HTML is public and process-wide cached so mutable widget
		// status belongs in this primary authenticated startup response instead.
		payload["widget_status"] = s.currentWidgetStatus()
	}
	if includeTranscript && selected != nil {
		// Only the explicitly selected startup session gets transcript I/O. Keep
		// ordinary 100-row sidebar listings summary-only and N+1 free.
		sideload, sideloadErr := s.selectedTranscriptStartupSideload(r.Context(), selected.ID)
		if sideloadErr == nil && sideload != nil {
			payload["selected_transcript"] = sideload
		}
	}
	writeJSONConditional(w, r, http.StatusOK, payload)
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
	if query == "" {
		writeJSONConditional(w, r, http.StatusOK, map[string]any{"sessions": []any{}})
		return
	}
	limit := 20
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		if parsed, err := strconv.Atoi(rawLimit); err == nil && parsed > 0 {
			limit = min(parsed, 50)
		}
	}

	matches, err := s.store.Search(r.Context(), session.SearchOptions{
		Query:            query,
		Categories:       categories,
		Limit:            limit,
		Archived:         includeArchived,
		ProjectID:        strings.TrimSpace(r.URL.Query().Get("project_id")),
		ExcludeSubagents: true,
	})
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
		ProjectID     string                `json:"project_id,omitempty"`
		ProjectName   string                `json:"project_name,omitempty"`
		Archived      bool                  `json:"archived"`
		Pinned        bool                  `json:"pinned"`
		CreatedAt     int64                 `json:"created_at"`
		LastMessageAt int64                 `json:"last_message_at"`
		MsgCount      int                   `json:"message_count"`
		Snippet       string                `json:"snippet,omitempty"`
		MessageID     int64                 `json:"message_id,omitempty"`
	}

	result := make([]sessionSearchEntry, 0, len(matches))
	for _, match := range matches {
		summary := session.SessionSummary{
			ID:                  match.SessionID,
			Number:              match.SessionNumber,
			Name:                match.SessionName,
			Summary:             match.Summary,
			GeneratedShortTitle: match.GeneratedShortTitle,
			GeneratedLongTitle:  match.GeneratedLongTitle,
			TitleSource:         match.TitleSource,
			Provider:            match.Provider,
			ProviderKey:         match.ProviderKey,
			Model:               match.Model,
			Mode:                match.Mode,
			Origin:              match.Origin,
			Archived:            match.Archived,
			Pinned:              match.Pinned,
			MessageCount:        match.MessageCount,
			Status:              match.Status,
			ProjectID:           match.ProjectID,
			ProjectName:         match.ProjectName,
			CreatedAt:           match.SessionCreatedAt,
			UpdatedAt:           match.UpdatedAt,
			LastMessageAt:       match.LastMessageAt,
		}
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
			ProjectID:     summary.ProjectID,
			ProjectName:   summary.ProjectName,
			Archived:      summary.Archived,
			Pinned:        summary.Pinned,
			CreatedAt:     summary.CreatedAt.UnixMilli(),
			LastMessageAt: lastMessageAt.UnixMilli(),
			MsgCount:      summary.MessageCount,
			Snippet:       match.Snippet,
			MessageID:     match.MessageID,
		})
	}

	writeJSONConditional(w, r, http.StatusOK, map[string]any{"sessions": result})
}

func (s *serveServer) getSessionMessagesPageDescending(ctx context.Context, sessionID string, beforeSeq, limit int) ([]session.Message, error) {
	if pager, ok := s.store.(session.MessagesDescendingPager); ok {
		return pager.GetMessagesPageDescending(ctx, sessionID, beforeSeq, limit)
	}

	// Fallback for tests/custom stores that have not implemented the optional
	// reverse pager. The built-in SQLite and logging stores implement the
	// efficient path above.
	msgs, err := s.store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		return nil, err
	}
	capHint := len(msgs)
	if limit > 0 && limit < capHint {
		capHint = limit
	}
	page := make([]session.Message, 0, capHint)
	for i := len(msgs) - 1; i >= 0; i-- {
		if beforeSeq > 0 && msgs[i].Sequence >= beforeSeq {
			continue
		}
		page = append(page, msgs[i])
		if limit > 0 && len(page) >= limit {
			break
		}
	}
	return page, nil
}

func askUserResultSummary(content string) string {
	var result tools.AskUserResult
	if err := json.Unmarshal([]byte(content), &result); err != nil || len(result.Answers) == 0 {
		return ""
	}
	return tools.AskUserAnswerSummary(result.Answers)
}

func (s *serveServer) sessionMessageEntries(msgs []session.Message) []sessionMessageEntry {
	failedToolCalls := make(map[string]bool)
	guardianReviewsByCall := make(map[string][]llm.GuardianReview)
	planToolCalls := make(map[string]bool)
	spawnAgentToolCalls := make(map[string]bool)
	parentSessionID := ""
	for _, msg := range msgs {
		if parentSessionID == "" {
			parentSessionID = strings.TrimSpace(msg.SessionID)
		}
		for _, part := range msg.Parts {
			if part.Type == llm.PartToolCall && part.ToolCall != nil && part.ToolCall.ID != "" {
				if part.ToolCall.Name == "update_plan" {
					planToolCalls[part.ToolCall.ID] = true
				}
				if part.ToolCall.Name == tools.SpawnAgentToolName {
					spawnAgentToolCalls[part.ToolCall.ID] = true
				}
			}
			if part.Type == llm.PartToolResult && part.ToolResult != nil {
				if part.ToolResult.IsError && part.ToolResult.ID != "" {
					failedToolCalls[part.ToolResult.ID] = true
				}
				if part.ToolResult.ID != "" && len(part.ToolResult.GuardianReviews) > 0 {
					guardianReviewsByCall[part.ToolResult.ID] = append(guardianReviewsByCall[part.ToolResult.ID], part.ToolResult.GuardianReviews...)
				}
			}
		}
	}
	result := make([]sessionMessageEntry, 0, len(msgs))
	for i := range msgs {
		msg := &msgs[i]
		// System, ordinary developer, and synthetic goal-steering messages are
		// provider context rather than human-authored transcript rows. Path notes
		// are explicitly marked and expose only their display text/provenance.
		pathNote, isPathNote := msg.PathNoteProvenance()
		if msg.Role == llm.RoleSystem || (msg.Role == llm.RoleDeveloper && !isPathNote) || msg.IsGoalSteering() {
			continue
		}
		entryRole := string(msg.Role)
		if isPathNote {
			entryRole = "path-note"
		}
		entry := sessionMessageEntry{
			ID:                   msg.ID,
			Sequence:             msg.Sequence,
			Role:                 entryRole,
			CreatedAt:            msg.CreatedAt.UnixMilli(),
			CompactionTail:       msg.CompactionTail,
			ClientMessageID:      msg.ClientMessageID,
			ResponseID:           msg.ResponseID,
			SegmentStartSequence: msg.SegmentStartSequence,
			SegmentEndSequence:   msg.SegmentEndSequence,
		}
		if msg.Role == llm.RoleAssistant && msg.ResponseID != "" {
			ordinal := msg.AssistantSegmentOrdinal
			entry.AssistantSegmentOrdinal = &ordinal
		}
		if isPathNote {
			copyProvenance := *pathNote
			copyProvenance.ReadFiles = append([]string(nil), pathNote.ReadFiles...)
			copyProvenance.ModifiedFiles = append([]string(nil), pathNote.ModifiedFiles...)
			entry.Parts = append(entry.Parts, sessionMessagePartEntry{Type: "path_note", Text: msg.PathNoteDisplayText(), PathNote: &copyProvenance})
			result = append(result, entry)
			continue
		}
		if msg.Role == llm.RoleEvent {
			if marker, ok := llm.ParseModelSwapMarker(msg.ToLLMMessage()); ok {
				entry.Parts = append(entry.Parts, sessionMessagePartEntry{Type: "model_swap", Text: marker.DisplayText})
			} else if marker, ok := llm.ParseRunErrorMarker(msg.ToLLMMessage()); ok {
				entry.Parts = append(entry.Parts, sessionMessagePartEntry{Type: "error", Text: marker.Message})
			} else {
				for _, p := range msg.Parts {
					switch p.Type {
					case llm.PartSkillActivation:
						if p.SkillActivation != nil {
							copyProvenance := *p.SkillActivation
							copyProvenance.AllowedTools = append([]string(nil), p.SkillActivation.AllowedTools...)
							entry.Parts = append(entry.Parts, sessionMessagePartEntry{Type: "skill_activation", SkillActivation: &copyProvenance})
						}
					case llm.PartText:
						if p.Text != "" {
							entry.Parts = append(entry.Parts, sessionMessagePartEntry{Type: "text", Text: p.Text})
						}
					}
				}
			}
			if len(entry.Parts) == 0 {
				entry.Parts = []sessionMessagePartEntry{}
			}
			result = append(result, entry)
			continue
		}
		embeddedFiles := make(map[string]bool)
		for _, p := range msg.Parts {
			switch p.Type {
			case llm.PartDiffComment:
				if p.DiffComment != nil {
					copyComment := *p.DiffComment
					copyComment.ContextBefore = append([]llm.DiffCommentContextLine(nil), p.DiffComment.ContextBefore...)
					copyComment.ContextAfter = append([]llm.DiffCommentContextLine(nil), p.DiffComment.ContextAfter...)
					entry.Parts = append(entry.Parts, sessionMessagePartEntry{Type: "diff_comment", DiffComment: &copyComment})
				}
			case llm.PartText:
				text := p.Text
				if msg.Role == llm.RoleUser {
					for _, name := range llm.ExtractEmbeddedFileNames(text) {
						if embeddedFiles[name] {
							continue
						}
						embeddedFiles[name] = true
						entry.Parts = append(entry.Parts, sessionMessagePartEntry{Type: "file", Text: name})
					}
					text = llm.StripEmbeddedFileText(text)
				}
				if text != "" {
					entry.Parts = append(entry.Parts, sessionMessagePartEntry{
						Type: "text",
						Text: text,
					})
				}
			case llm.PartImage:
				if imageURL, serveablePath := s.sessionMessageImageURL(p); imageURL != "" {
					mimeType := ""
					if p.ImageData != nil {
						mimeType = p.ImageData.MediaType
					}
					width, height := sessionMessageImageDimensions(p, serveablePath)
					entry.Parts = append(entry.Parts, sessionMessagePartEntry{
						Type:     "image",
						ImageURL: imageURL,
						MimeType: mimeType,
						Width:    width,
						Height:   height,
					})
				}
			case llm.PartToolActivity:
				if p.ToolActivity != nil {
					pe := sessionMessagePartEntry{
						Type:       "tool_activity",
						ToolName:   p.ToolActivity.Name,
						ToolInfo:   p.ToolActivity.Info,
						ToolCallID: p.ToolActivity.ID,
						ToolStatus: p.ToolActivity.Status,
						ToolError:  p.ToolActivity.Status == llm.ToolActivityFailed,
					}
					if len(p.ToolActivity.Arguments) > 0 {
						pe.ToolArgs = string(p.ToolActivity.Arguments)
					}
					entry.Parts = append(entry.Parts, pe)
				}
			case llm.PartToolCall:
				if p.ToolCall != nil {
					pe := sessionMessagePartEntry{
						Type:            "tool_call",
						ToolName:        p.ToolCall.Name,
						ToolCallID:      p.ToolCall.ID,
						ToolError:       failedToolCalls[p.ToolCall.ID],
						GuardianReviews: append([]llm.GuardianReview(nil), guardianReviewsByCall[p.ToolCall.ID]...),
					}
					if len(p.ToolCall.Arguments) > 0 {
						pe.ToolArgs = string(p.ToolCall.Arguments)
					}
					entry.Parts = append(entry.Parts, pe)
				}
			case llm.PartToolResult:
				if p.ToolResult != nil {
					isPlanResult := p.ToolResult.ID != "" && (p.ToolResult.Name == "update_plan" || planToolCalls[p.ToolResult.ID])
					isAskUserResult := p.ToolResult.Name == tools.AskUserToolName
					isSpawnAgentResult := p.ToolResult.Name == tools.SpawnAgentToolName || spawnAgentToolCalls[p.ToolResult.ID]
					var spawnResult *tools.SpawnAgentResult
					if isSpawnAgentResult {
						if parsed, err := tools.ParseSpawnAgentResult(p.ToolResult.Content); err == nil {
							spawnResult = &parsed
						} else if parsed, displayErr := tools.ParseSpawnAgentResult(p.ToolResult.Display); displayErr == nil {
							spawnResult = &parsed
						}
						if spawnResult != nil {
							spawnResult.Output = boundedWebSpawnAgentOutput(spawnResult.Output)
							spawnResult.Error = boundedWebSpawnAgentText(spawnResult.Error, 16*1024, "… (error truncated)")
							spawnResult.AgentName = boundedWebSpawnAgentText(spawnResult.AgentName, 256, "…")
							spawnResult.SessionID = s.validatedSpawnChildID(parentSessionID, spawnResult.SessionID)
						}
					}
					includeResult := p.ToolResult.IsError || len(p.ToolResult.Images) > 0 || len(p.ToolResult.GuardianReviews) > 0 || isPlanResult || isAskUserResult || spawnResult != nil
					if !includeResult {
						continue
					}
					toolName := p.ToolResult.Name
					if toolName == "" && isSpawnAgentResult {
						toolName = tools.SpawnAgentToolName
					}
					pe := sessionMessagePartEntry{
						Type:            "tool_result",
						ToolName:        toolName,
						ToolCallID:      p.ToolResult.ID,
						ToolError:       p.ToolResult.IsError || (spawnResult != nil && spawnResult.Error != ""),
						GuardianReviews: append([]llm.GuardianReview(nil), p.ToolResult.GuardianReviews...),
						SpawnAgent:      spawnResult,
					}
					if isAskUserResult {
						pe.AskUserSummary = askUserResultSummary(p.ToolResult.Content)
					}
					if len(p.ToolResult.Images) > 0 {
						pe.Images = s.toolImageURLs(p.ToolResult.Images)
					}
					entry.Parts = append(entry.Parts, pe)
				}
			}
		}
		if len(entry.Parts) == 0 {
			entry.Parts = []sessionMessagePartEntry{}
		}
		result = append(result, entry)
	}
	return result
}

func (s *serveServer) writeSessionMessagesResponse(w http.ResponseWriter, r *http.Request, resp sessionMessagesResponse) {
	body, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	etag := jsonPayloadETag(body)
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

func (s *serveServer) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	// Parse /v1/sessions/{id}/...
	path := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 1 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	requestedSessionID := parts[0]
	sessionID := requestedSessionID
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

	if suffix == "children" {
		s.handleSessionChildren(w, r, sessionID)
		return
	}

	if suffix == "project" {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, POST")
			writeProjectError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		s.handleSessionProjectAssignment(w, r, sessionID)
		return
	}

	if suffix == "skills" || suffix == "skills/invoke" || strings.HasPrefix(suffix, "skill-runs/") {
		s.handleSessionSkills(w, r, sessionID, requestedSessionID, suffix)
		return
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

	if suffix == "runtime/compact" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		s.handleSessionRuntimeCompact(w, r, sessionID)
		return
	}

	if suffix == "runtime/undo" || suffix == "runtime/redo" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		s.handleSessionUndoRedo(w, r, sessionID, suffix == "runtime/redo")
		return
	}

	if suffix == "runtime/effort" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		s.handleSessionRuntimeEffort(w, r, sessionID)
		return
	}

	if suffix == "runtime/goal" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		s.handleSessionRuntimeGoal(w, r, sessionID)
		return
	}

	if strings.HasPrefix(suffix, "interjections/") {
		id := strings.TrimPrefix(suffix, "interjections/")
		id = strings.TrimSuffix(id, "/cancel")
		if r.Method != http.MethodDelete && r.Method != http.MethodPost {
			w.Header().Set("Allow", "DELETE, POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		s.handleSessionInterjectionCancel(w, r, sessionID, id)
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

	if suffix == "title/refine" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		s.handleSessionTitleRefine(w, r, sessionID)
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

	if suffix == "mcp" {
		if r.Method != http.MethodGet && r.Method != http.MethodPatch {
			w.Header().Set("Allow", "GET, PATCH")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if r.Method == http.MethodPatch {
			if err := requireJSONContentType(r); err != nil {
				writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
				return
			}
		}
		s.handleSessionMCP(w, r, sessionID)
		return
	}

	if suffix == "diff-comments" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		s.handleSessionDiffComments(w, r, sessionID)
		return
	}

	if suffix == "file-changes" || suffix == "file-changes/diff" || suffix == "file-changes/content" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		switch suffix {
		case "file-changes":
			s.handleSessionFileChanges(w, r, sessionID)
		case "file-changes/diff":
			s.handleSessionFileChangeDiff(w, r, sessionID)
		default:
			s.handleSessionFileChangeContent(w, r, sessionID)
		}
		return
	}

	if suffix == "tree" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		s.handleSessionTree(w, r, sessionID)
		return
	}

	if suffix == "branches" || suffix == "path-notes" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		if suffix == "branches" {
			s.handleCreateSessionBranch(w, r, sessionID)
		} else {
			s.handleSessionPathNotes(w, r, sessionID)
		}
		return
	}

	if suffix == "transcript" || suffix == "transcript/bodies" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if suffix == "transcript" {
			s.handleSessionTranscript(w, r, sessionID)
		} else {
			s.handleSessionTranscriptBodies(w, r, sessionID)
		}
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
	// Keep the legacy messages endpoint for non-UI consumers and cache-first
	// service-worker skew: an older cached web shell may run briefly against a
	// revision-aware server. New clients use /transcript (and only fall back here
	// when that endpoint is absent on an older server).
	if s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}

	query := r.URL.Query()
	limit := parseSessionMessagesLimit(query.Get("limit"))
	_, hasBeforeSeqParam := query["before_seq"]
	reverseMode := parseSessionMessagesTail(query.Get("tail")) || hasBeforeSeqParam

	var msgs []session.Message
	var hasMore bool
	var nextOffset int
	var nextBeforeSeq int
	var err error

	if reverseMode {
		beforeSeq := parseSessionMessagesBeforeSeq(query.Get("before_seq"))
		descending, pageErr := s.getSessionMessagesPageDescending(r.Context(), sessionID, beforeSeq, limit+1)
		if pageErr != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to get messages")
			return
		}
		hasMore = len(descending) > limit
		if hasMore {
			descending = descending[:limit]
		}
		if hasMore && len(descending) > 0 {
			nextBeforeSeq = descending[len(descending)-1].Sequence
		}
		msgs = make([]session.Message, len(descending))
		for i := range descending {
			msgs[len(descending)-1-i] = descending[i]
		}
	} else {
		offset := parseSessionMessagesOffset(query.Get("offset"))
		msgs, err = s.store.GetMessages(r.Context(), sessionID, limit+1, offset)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to get messages")
			return
		}
		hasMore = len(msgs) > limit
		if hasMore {
			msgs = msgs[:limit]
		}
		nextOffset = offset + len(msgs)
	}

	compactionSeq := -1
	compactionCount := 0
	if meta, metaErr := s.store.Get(r.Context(), sessionID); metaErr == nil && session.HasCompactionBoundary(meta) {
		compactionSeq = meta.CompactionSeq
		compactionCount = meta.CompactionCount
	}

	resp := sessionMessagesResponse{
		LastResponseID: s.latestDurableResponseIDForSession(r.Context(), sessionID),
		Messages:       s.sessionMessageEntries(msgs),
		HasMore:        hasMore,
	}
	if compactionSeq >= 0 {
		resp.CompactionSeq = &compactionSeq
		resp.CompactionCount = compactionCount
	}
	if hasMore {
		if reverseMode {
			resp.NextBeforeSeq = nextBeforeSeq
		} else {
			resp.NextOffset = nextOffset
		}
	}
	s.writeSessionMessagesResponse(w, r, resp)
}

func (s *serveServer) handleSessionInterrupt(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req sessionInterruptRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	currentResponseID := ""
	if s.responseRuns != nil {
		currentResponseID = s.responseRuns.activeRunID(sessionID)
	}
	if expected := strings.TrimSpace(req.ExpectedResponseID); expected != "" && expected != currentResponseID {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"type": "response_owner_conflict", "message": "the active response changed",
			},
			"current_response_id": currentResponseID,
		})
		return
	}
	displayText := strings.TrimSpace(req.Message)
	delivery, err := normalizeInterruptDelivery(req.Delivery)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var msg llm.Message
	if len(req.Content) > 0 && strings.TrimSpace(string(req.Content)) != "" && strings.TrimSpace(string(req.Content)) != "null" {
		msg, err = parseUserMessageContent(req.Content)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if displayText == "" {
			displayText = strings.TrimSpace(llm.MessageText(msg))
		} else if strings.TrimSpace(llm.MessageText(msg)) == "" {
			msg.Parts = append(msg.Parts, llm.Part{Type: llm.PartText, Text: displayText})
		}
	} else {
		msg = llm.UserText(displayText)
	}
	if displayText == "" && strings.TrimSpace(llm.MessageText(msg)) == "" && llm.MessageAttachmentSummary(msg) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "message or content is required")
		return
	}

	if s.sessionMgr == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	rt, ok := s.sessionMgr.Get(sessionID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	mentionMessages := []llm.Message{msg}
	s.augmentMessagesWithMentions(r.Context(), rt, sessionID, "", mentionMessages)
	msg = mentionMessages[0]

	var fastProvider llm.Provider
	if delivery == interruptDeliveryAuto {
		var fastErr error
		fastProvider, fastErr = llm.NewFastProvider(s.cfgRef, rt.providerKey)
		if fastErr != nil {
			log.Printf("[serve] fast provider unavailable for interrupt: %v", fastErr)
		}
	}
	clientMessageID := strings.TrimSpace(req.ClientMessageID)
	if clientMessageID == "" {
		clientMessageID = strings.TrimSpace(req.InterjectionID)
	}
	var action llm.InterruptAction
	var replayed bool
	var interruptErr error
	expectedResponseID := strings.TrimSpace(req.ExpectedResponseID)
	if expectedResponseID != "" || req.ExpectedRunEpoch > 0 {
		owned := s.responseRuns.withExpectedActiveRun(sessionID, expectedResponseID, req.ExpectedRunEpoch, func() {
			action, replayed, interruptErr = rt.InterruptMessage(r.Context(), msg, displayText, clientMessageID, fastProvider, delivery)
		})
		if !owned {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": map[string]any{
					"type": "response_owner_conflict", "message": "the active response changed",
				},
				"current_response_id": s.responseRuns.activeRunID(sessionID),
			})
			return
		}
	} else {
		action, replayed, interruptErr = rt.InterruptMessage(r.Context(), msg, displayText, clientMessageID, fastProvider, delivery)
	}
	if interruptErr != nil {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", interruptErr.Error())
		return
	}
	if action == llm.InterruptCancel && !replayed && s.store != nil {
		goalCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), goalPersistTimeout)
		defer cancel()
		if sess, err := s.store.Get(goalCtx, sessionID); err == nil && sess != nil && sess.Goal != nil && sess.Goal.IsActive() {
			goal := sess.Goal.Clone()
			goal.Status = session.GoalStatusPaused
			goal.PausedAt = time.Now()
			goal.UpdatedAt = goal.PausedAt
			goal.LastReason = "paused because the active run was stopped"
			_ = session.UpdateGoal(goalCtx, s.store, sessionID, goal)
		}
	}

	actionName := "interject"
	switch action {
	case llm.InterruptCancel:
		actionName = "cancel"
	case llm.InterruptInterject:
		actionName = "interject"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action":              actionName,
		"replayed":            replayed,
		"interjection_id":     strings.TrimSpace(req.InterjectionID),
		"current_response_id": currentResponseID,
	})
}

func (s *serveServer) handleSessionRuntimeGoal(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req sessionRuntimeGoalRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}
	sess, err := s.store.Get(r.Context(), sessionID)
	if (err != nil || sess == nil) && s.sessionMgr != nil {
		if rt, rtErr := s.sessionMgr.GetOrCreate(r.Context(), sessionID); rtErr == nil && rt != nil {
			if !rt.mu.TryLock() {
				writeOpenAIError(w, http.StatusConflict, "conflict_error", "session is busy; retry after the active response finishes")
				return
			}
			if rt.ensurePersistedSession(r.Context(), sessionID, nil) && rt.sessionMeta != nil {
				sess = rt.sessionMeta
				err = nil
			}
			rt.mu.Unlock()
		}
	}
	if err != nil || sess == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	now := time.Now()
	action := strings.ToLower(strings.TrimSpace(req.Action))
	if action == "" {
		action = "set"
	}
	var goal *session.Goal
	switch action {
	case "set", "edit":
		objective := strings.TrimSpace(req.Objective)
		if objective == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "objective is required")
			return
		}
		budget := 0
		if req.TokenBudget != nil {
			if *req.TokenBudget < 0 {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "token_budget must be non-negative")
				return
			}
			budget = *req.TokenBudget
		}
		if action == "edit" && sess.Goal != nil && sess.Goal.Exists() {
			goal = sess.Goal.Clone()
			goal.Objective = objective
			if req.TokenBudget != nil {
				goal.TokenBudget = budget
			}
			goal.Status = session.GoalStatusActive
			goal.PausedAt = time.Time{}
			goal.CompletedAt = time.Time{}
			goal.BlockedAt = time.Time{}
			goal.UpdatedNotice = true
			goal.UpdatedAt = now
		} else {
			goal = session.NewGoal(objective, budget, now)
		}
	case "pause":
		if sess.Goal == nil || !sess.Goal.Exists() {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "no goal is set")
			return
		}
		goal = sess.Goal.Clone()
		goal.Status = session.GoalStatusPaused
		goal.PausedAt = now
		goal.UpdatedAt = now
	case "resume":
		if sess.Goal == nil || !sess.Goal.Exists() {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "no goal is set")
			return
		}
		if sess.Goal.Status == session.GoalStatusBudgetLimited && sess.Goal.BudgetExhausted() {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "token budget is exhausted; edit the goal with a higher token_budget before resuming")
			return
		}
		goal = sess.Goal.Clone()
		goal.Status = session.GoalStatusActive
		goal.PausedAt = time.Time{}
		goal.CompletedAt = time.Time{}
		goal.BlockedAt = time.Time{}
		goal.UpdatedAt = now
	case "clear":
		goal = nil
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "action must be set, edit, pause, resume, or clear")
		return
	}
	if err := session.UpdateGoal(r.Context(), s.store, sessionID, goal); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to update goal")
		return
	}
	if s.sessionMgr != nil {
		if rt, ok := s.sessionMgr.Get(sessionID); ok && rt != nil {
			if rt.mu.TryLock() {
				if rt.sessionMeta != nil {
					rt.sessionMeta.Goal = goal.Clone()
				}
				rt.mu.Unlock()
			}
		}
	}
	s.publishEvent(serveEventInput{Type: serveEventSessionRuntimeChanged, SessionID: sessionID, Reason: "goal"})
	writeJSON(w, http.StatusOK, map[string]any{"goal": goal})
}

func (s *serveServer) handleSessionRuntimeEffort(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req sessionRuntimeEffortRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if s.sessionMgr == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	rt, ok := s.sessionMgr.Get(sessionID)
	if !ok || rt == nil || rt.engine == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}

	provider := strings.TrimSpace(rt.providerKey)
	if provider == "" && rt.provider != nil {
		provider = strings.TrimSpace(rt.provider.Name())
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(rt.defaultModel)
	}
	effort := normalizeReasoningEffort(req.ReasoningEffort)
	model, effort = normalizeProviderModelEffort(provider, model, effort)
	if model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if effort != "" {
		efforts := llm.ReasoningEffortsForProviderModel(provider, model)
		if len(efforts) > 0 {
			valid := false
			for _, allowed := range efforts {
				if strings.EqualFold(strings.TrimSpace(allowed), effort) {
					valid = true
					break
				}
			}
			if !valid {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("reasoning effort %q is not supported for %s:%s", effort, provider, model))
				return
			}
		}
	}
	if err := rt.QueueActiveRunRuntimeSwitch(model, effort); err != nil {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
		return
	}
	s.publishEvent(serveEventInput{Type: serveEventSessionRuntimeChanged, SessionID: sessionID, Reason: "effort"})
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "queued",
		"model":            model,
		"reasoning_effort": effort,
	})
}

func (s *serveServer) handleSessionInterjectionCancel(w http.ResponseWriter, r *http.Request, sessionID, interjectionID string) {
	interjectionID = strings.TrimSpace(interjectionID)
	if interjectionID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "interjection id is required")
		return
	}
	if s.sessionMgr != nil {
		if rt, ok := s.sessionMgr.Get(sessionID); ok && rt != nil && rt.engine != nil {
			cancelled, err := rt.cancelPendingInterjection(r.Context(), sessionID, interjectionID)
			if err != nil {
				writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to cancel pending interjection")
				return
			}
			if !cancelled {
				writeOpenAIError(w, http.StatusConflict, "conflict_error", "interjection is not queued or has already been committed")
				return
			}
			s.publishEvent(serveEventInput{Type: serveEventSessionRuntimeChanged, SessionID: sessionID, Reason: "interjection_cancelled"})
			writeJSON(w, http.StatusOK, map[string]any{"cancelled": true, "interjection_id": interjectionID})
			return
		}
	}

	cancelled := false
	if pendingStore, ok := session.AsPendingInterjectionStore(s.store); ok {
		entries, err := pendingStore.ListPendingInterjections(r.Context(), sessionID)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to read pending interjection")
			return
		}
		for _, entry := range entries {
			if entry.ID == interjectionID {
				cancelled = true
				break
			}
		}
		if cancelled {
			if err := pendingStore.DeletePendingInterjection(r.Context(), sessionID, interjectionID); err != nil {
				writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to cancel pending interjection")
				return
			}
		}
	}
	if !cancelled {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "interjection is not queued or has already been committed")
		return
	}
	s.publishEvent(serveEventInput{Type: serveEventSessionRuntimeChanged, SessionID: sessionID, Reason: "interjection_cancelled"})
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true, "interjection_id": interjectionID})
}

func (s *serveServer) newTitleProvider() (llm.Provider, error) {
	if s.titleProviderFactory != nil {
		return s.titleProviderFactory(s.cfgRef)
	}
	if s.cfgRef == nil {
		return nil, fmt.Errorf("config is unavailable")
	}
	provider, err := llm.NewFastProvider(s.cfgRef, s.cfgRef.DefaultProvider)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, fmt.Errorf("no fast provider configured for %q", s.cfgRef.DefaultProvider)
	}
	return provider, nil
}

func (s *serveServer) handleSessionTitleRefine(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req struct {
		Preview bool `json:"preview"`
	}
	if r.Body != nil {
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&req); err != nil && err != io.EOF {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
			return
		}
	}
	if s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
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
	previousTitle := sess.PreferredShortTitle()
	previousLongTitle := sess.PreferredLongTitle()

	messages, err := s.store.GetMessages(r.Context(), sess.ID, 80, 0)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load session messages")
		return
	}
	if len(messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "session has no messages to title")
		return
	}

	provider, err := s.newTitleProvider()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to create fast title provider: "+err.Error())
		return
	}
	cand, err := sessiontitle.Generate(r.Context(), provider, sess, messages)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "server_error", "failed to refine title: "+err.Error())
		return
	}

	if !req.Preview {
		sess.Name = ""
		sess.GeneratedShortTitle = cand.ShortTitle
		sess.GeneratedLongTitle = cand.LongTitle
		sess.TitleSource = session.TitleSourceGenerated
		sess.TitleGeneratedAt = time.Now().UTC()
		if len(messages) > 0 {
			sess.TitleBasisMsgSeq = messages[len(messages)-1].Sequence
		}
		if err := s.store.Update(r.Context(), sess); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to save refined title")
			return
		}

		if s.sessionMgr != nil {
			if rt, ok := s.sessionMgr.Get(sessionID); ok {
				trySyncRuntimeSessionMetadata(rt, sess)
			}
		}
		if previousTitle != sess.PreferredShortTitle() || previousLongTitle != sess.PreferredLongTitle() {
			s.publishEvent(serveEventInput{Type: serveEventSessionMetadataChanged, SessionID: sessionID, Reason: "refined_title"})
		}
	}

	generatedShort := sess.GeneratedShortTitle
	generatedLong := sess.GeneratedLongTitle
	preferredShort := sess.PreferredShortTitle()
	preferredLong := sess.PreferredLongTitle()
	if req.Preview {
		generatedShort = cand.ShortTitle
		generatedLong = cand.LongTitle
		preferredShort = cand.ShortTitle
		preferredLong = cand.LongTitle
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":                    sess.ID,
		"name":                  sess.Name,
		"short_title":           preferredShort,
		"long_title":            preferredLong,
		"generated_short_title": generatedShort,
		"generated_long_title":  generatedLong,
		"mode":                  sess.Mode,
		"origin":                sess.Origin,
		"archived":              sess.Archived,
		"pinned":                sess.Pinned,
		"created_at":            sess.CreatedAt.UnixMilli(),
	})
}

// trySyncRuntimeSessionMetadata refreshes the runtime cache only when it is idle.
// An active stream owns rt.mu for its full run, so metadata endpoints must treat
// the already-updated store as authoritative rather than waiting on that lock.
func trySyncRuntimeSessionMetadata(rt *serveRuntime, sess *session.Session) bool {
	if rt == nil || sess == nil || !rt.mu.TryLock() {
		return false
	}
	defer rt.mu.Unlock()
	if rt.sessionMeta == nil {
		return true
	}
	rt.sessionMeta.Name = sess.Name
	rt.sessionMeta.GeneratedShortTitle = sess.GeneratedShortTitle
	rt.sessionMeta.GeneratedLongTitle = sess.GeneratedLongTitle
	rt.sessionMeta.TitleSource = sess.TitleSource
	rt.sessionMeta.TitleGeneratedAt = sess.TitleGeneratedAt
	rt.sessionMeta.TitleBasisMsgSeq = sess.TitleBasisMsgSeq
	rt.sessionMeta.Archived = sess.Archived
	rt.sessionMeta.Pinned = sess.Pinned
	rt.sessionMeta.Origin = sess.Origin
	return true
}

func (s *serveServer) handleSessionMetadataPatch(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}

	var req struct {
		Name                *string `json:"name"`
		GeneratedShortTitle *string `json:"generated_short_title"`
		GeneratedLongTitle  *string `json:"generated_long_title"`
		Archived            *bool   `json:"archived"`
		Pinned              *bool   `json:"pinned"`
	}
	if err := decodeSmallJSON(w, r, &req); err != nil {
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

	beforeName := sess.Name
	beforeShort := sess.GeneratedShortTitle
	beforeLong := sess.GeneratedLongTitle
	beforeArchived := sess.Archived
	beforePinned := sess.Pinned
	if req.Name != nil {
		sess.Name = strings.TrimSpace(*req.Name)
	}
	if req.GeneratedShortTitle != nil {
		sess.GeneratedShortTitle = strings.TrimSpace(*req.GeneratedShortTitle)
		sess.TitleSource = session.TitleSourceGenerated
		sess.TitleGeneratedAt = time.Now().UTC()
	}
	if req.GeneratedLongTitle != nil {
		sess.GeneratedLongTitle = strings.TrimSpace(*req.GeneratedLongTitle)
		sess.TitleSource = session.TitleSourceGenerated
		if sess.TitleGeneratedAt.IsZero() {
			sess.TitleGeneratedAt = time.Now().UTC()
		}
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
			trySyncRuntimeSessionMetadata(rt, sess)
		}
	}
	if beforeName != sess.Name || beforeShort != sess.GeneratedShortTitle || beforeLong != sess.GeneratedLongTitle || beforeArchived != sess.Archived || beforePinned != sess.Pinned {
		s.publishEvent(serveEventInput{Type: serveEventSessionMetadataChanged, SessionID: sessionID, Reason: "metadata"})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":                    sess.ID,
		"name":                  sess.Name,
		"short_title":           sess.PreferredShortTitle(),
		"long_title":            sess.PreferredLongTitle(),
		"generated_short_title": sess.GeneratedShortTitle,
		"generated_long_title":  sess.GeneratedLongTitle,
		"mode":                  sess.Mode,
		"origin":                sess.Origin,
		"archived":              sess.Archived,
		"pinned":                sess.Pinned,
		"created_at":            sess.CreatedAt.UnixMilli(),
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
		if gotToken == "" && cookieAuthAllowed(r) {
			// Cookie fallback is needed for browser-initiated requests that cannot set
			// Authorization headers. Keep it GET-only for normal API/UI routes, but
			// allow all widget methods: widget iframe fetches include same-site cookies,
			// and the widget proxy strips Cookie before forwarding to the app.
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

func cookieAuthAllowed(r *http.Request) bool {
	if r.Method == http.MethodGet {
		return true
	}
	path := r.URL.Path
	return path == "/widgets" || strings.HasPrefix(path, "/widgets/")
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
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID, X-Term-LLM-Session-ID, X-Term-LLM-Draft-ID, X-Term-LLM-Push-Subscription-ID, session_id, Idempotency-Key, X-Idempotency-Key, X-Term-LLM-Request-ID, X-Term-LLM-UI-Version, X-API-Key, anthropic-version")
			w.Header().Set("Access-Control-Expose-Headers", "x-session-id, x-session-number, x-response-id, x-branch-anchor-id, x-term-llm-ui-version")
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
		models = append(models, llm.GetCachedOpenRouterModelInfos(apiKey)...)
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
		if modelConfig, ok := config.ModelConfigForProviderModel(s.cfgRef, effectiveName, m.ID); ok {
			if len(m.ReasoningEfforts) == 0 {
				m.ReasoningEfforts = append([]string(nil), modelConfig.ReasoningEfforts...)
			}
			if m.DefaultReasoningEffort == "" {
				m.DefaultReasoningEffort = strings.TrimSpace(modelConfig.DefaultReasoningEffort)
			}
		}
		if _, ok := byID[m.ID]; ok {
			continue
		}
		byID[m.ID] = m
		ids = append(ids, m.ID)
	}
	// The web UI has a dedicated reasoning-effort selector, so drop
	// "<base>-<effort>" aliases when the base model is also present.
	ids = llm.DedupeEffortVariantsForProvider(effectiveName, ids)

	// Order: configured default first, then curated models in their authored
	// (popular-first) order, then anything else alpha-sorted. Pure alpha sort
	// buries the most-used models behind less-used variants.
	defaultModel := ""
	if pc, ok := s.cfgRef.Providers[effectiveName]; ok {
		defaultModel, _ = normalizeProviderModelEffort(effectiveName, pc.Model, "")
	}
	ids = llm.SortModelIDsByPopularity(effectiveName, defaultModel, ids)

	items := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		m, ok := byID[id]
		if !ok {
			continue
		}
		item := map[string]any{
			"id":      m.ID,
			"object":  "model",
			"created": m.Created,
			"owned_by": func() string {
				if m.OwnedBy != "" {
					return m.OwnedBy
				}
				return "term-llm"
			}(),
		}
		if m.InputLimit > 0 {
			item["input_limit"] = m.InputLimit
		}
		if m.InputPrice > 0 || m.OutputPrice > 0 {
			item["input_price"] = m.InputPrice
			item["output_price"] = m.OutputPrice
		}
		if len(m.ReasoningEfforts) > 0 {
			item["reasoning_efforts"] = append([]string(nil), m.ReasoningEfforts...)
		} else if efforts := llm.ReasoningEffortsForProviderModel(effectiveName, id); len(efforts) > 0 {
			item["reasoning_efforts"] = efforts
		}
		if m.DefaultReasoningEffort != "" {
			item["default_reasoning_effort"] = m.DefaultReasoningEffort
		}
		reasoningModes := m.ReasoningModes
		if len(reasoningModes) == 0 && llm.SupportsReasoningMode(effectiveName, id) {
			reasoningModes = []string{"standard", "pro"}
		}
		if len(reasoningModes) > 0 {
			item["reasoning_modes"] = reasoningModes
		}
		items = append(items, item)
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
// The goroutine also exits when ctx is cancelled between writes.
func sseKeepalive(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, interval time.Duration) (mu *sync.Mutex, stop func()) {
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
			case <-ctx.Done():
				return
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

func (s *serveServer) requestedRuntimeAgent(ctx context.Context, sessionID, requestedAgent string) string {
	if pinned := strings.TrimSpace(s.cfg.agentName); pinned != "" {
		return pinned
	}
	if requested := strings.TrimSpace(requestedAgent); requested != "" {
		return requested
	}
	if s.store != nil && strings.TrimSpace(sessionID) != "" {
		if sess, err := s.store.Get(ctx, sessionID); err == nil && sess != nil {
			return strings.TrimSpace(sess.Agent)
		}
	}
	return ""
}

func (s *serveServer) createRequestRuntime(ctx context.Context, providerName, modelName, agentName string) (*serveRuntime, error) {
	if s.agentRuntimeFactory != nil {
		return s.agentRuntimeFactory(ctx, providerName, modelName, agentName)
	}
	if strings.TrimSpace(agentName) != "" {
		return nil, fmt.Errorf("per-conversation agents are unavailable")
	}
	if s.runtimeFactory != nil {
		return s.runtimeFactory(ctx, providerName, modelName)
	}
	return s.sessionMgr.factory(ctx)
}

func (s *serveServer) runtimeForRequest(ctx context.Context, sessionID string) (*serveRuntime, bool, error) {
	agentName := s.requestedRuntimeAgent(ctx, sessionID, "")
	create := func(ctx context.Context) (*serveRuntime, error) {
		return s.createRequestRuntime(ctx, "", "", agentName)
	}
	if sessionID == "" {
		// Ephemeral stateless runtime (fresh per request for isolation)
		rt, err := create(ctx)
		if err != nil {
			return nil, false, err
		}
		return rt, false, nil
	}
	// Stateful sessions should persist beyond a single HTTP request, but
	// creation must still respect request cancellation/timeouts.
	rt, err := s.sessionMgr.GetOrCreateWith(ctx, sessionID, create)
	if err != nil {
		return nil, false, err
	}
	if err := s.ensureRuntimeBaseDirForSession(ctx, sessionID, rt); err != nil {
		return nil, false, err
	}
	if err := s.ensureRuntimeMCPForSession(ctx, sessionID, rt); err != nil {
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

func (s *serveServer) ensureRuntimeBaseDirForSession(ctx context.Context, sessionID string, rt *serveRuntime) error {
	if s == nil || s.store == nil || sessionID == "" || rt == nil {
		return nil
	}
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil || sess == nil {
		return nil
	}
	if strings.TrimSpace(sess.ProjectID) != "" {
		var binding serveWorkspaceBinding
		if s.projectsEnabled {
			binding, err = s.resolveWorkspace(ctx, serveWorkspaceRequest{SessionID: sessionID, ProjectID: sess.ProjectID})
		} else {
			// Disabling the project UI/API must not strand conversations previously
			// created in project mode. Their immutable snapshot still has to pass the
			// same canonical project/worktree validation before it is restored.
			binding, err = s.resolvePersistedProjectWorkspace(ctx, *sess)
		}
		if err != nil {
			return err
		}
		if rt.toolMgr != nil {
			readOnlyStore, readOnly := s.store.(interface{ ReadOnly() bool })
			if !readOnly || !readOnlyStore.ReadOnly() {
				if err := rt.toolMgr.ConfigureWorkspacePersistence(ctx, s.store, sessionID); err != nil {
					return err
				}
			}
			if err := rt.toolMgr.SetBaseDirWithContext(ctx, binding.RuntimeDir); err != nil {
				return err
			}
			s.configureRuntimeSkillsForDir(rt, binding.RuntimeDir)
		}
		rt.mu.Lock()
		rt.sessionMeta = sess
		rt.mu.Unlock()
		return nil
	}
	if rt.toolMgr == nil {
		return nil
	}
	readOnlyStore, readOnly := s.store.(interface{ ReadOnly() bool })
	if !readOnly || !readOnlyStore.ReadOnly() {
		if err := rt.toolMgr.ConfigureWorkspacePersistence(ctx, s.store, sessionID); err != nil {
			return err
		}
	}
	if strings.TrimSpace(sess.WorktreeDir) == "" {
		// Restore only a root that was explicitly persisted by a prior web request.
		// Empty, ambient, and stale directories remain ineligible.
		cwd := strings.TrimSpace(sess.CWD)
		trustedRoot := ""
		if s.cfg.ui && sess.Origin == session.OriginWeb && cwd != "" {
			if root, rootErr := s.currentGitRoot(); rootErr == nil && sameServePath(cwd, root) {
				trustedRoot = root
			}
		}
		if trustedRoot != "" {
			if err := BindRootSession(ctx, s.store, sess, rt.toolMgr, trustedRoot); err != nil {
				return err
			}
		} else if err := rt.toolMgr.ClearPrimaryWorkspace(ctx); err != nil {
			return err
		}
	} else if err := RestoreWorktreeBinding(ctx, s.store, sess, rt.toolMgr); err != nil {
		return err
	}
	rt.mu.Lock()
	rt.sessionMeta = sess
	rt.mu.Unlock()
	return nil
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
	agentName := s.requestedRuntimeAgent(ctx, sessionID, "")
	if sessionID == "" {
		rt, err := s.createRequestRuntime(ctx, providerName, modelName, agentName)
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
		return s.createRequestRuntime(ctx, providerName, modelName, agentName)
	})
	if err != nil {
		return nil, false, err
	}
	if err := s.ensureRuntimeBaseDirForSession(ctx, sessionID, rt); err != nil {
		return nil, false, err
	}
	if err := s.ensureRuntimeMCPForSession(ctx, sessionID, rt); err != nil {
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
	return s.runtimeForFreshAgentProviderRequest(ctx, sessionID, providerName, "")
}

func (s *serveServer) runtimeForFreshAgentProviderRequest(ctx context.Context, sessionID string, providerName string, requestedAgent string) (*serveRuntime, bool, error) {
	defaultProvider := ""
	if s.cfgRef != nil {
		defaultProvider = strings.TrimSpace(s.cfgRef.DefaultProvider)
	}
	providerName = strings.TrimSpace(providerName)
	desiredProvider := providerName
	if desiredProvider == "" {
		desiredProvider = defaultProvider
	}
	if s.runtimeFactory == nil && s.agentRuntimeFactory == nil && providerName != "" && providerName != defaultProvider {
		desiredProvider = ""
	}
	agentName := s.requestedRuntimeAgent(ctx, sessionID, requestedAgent)
	factoryProvider := ""
	if providerName != "" && providerName != defaultProvider {
		factoryProvider = providerName
	}
	create := s.sessionMgr.factory
	if agentName != "" || factoryProvider != "" || s.agentRuntimeFactory != nil {
		create = func(ctx context.Context) (*serveRuntime, error) {
			return s.createRequestRuntime(ctx, factoryProvider, "", agentName)
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
	if err := s.ensureRuntimeBaseDirForSession(ctx, sessionID, rt); err != nil {
		return nil, false, err
	}
	if err := s.ensureRuntimeMCPForSession(ctx, sessionID, rt); err != nil {
		return nil, false, err
	}
	existingProvider := runtimeProviderKey(rt)
	if existingProvider != "" && desiredProvider != "" && existingProvider != desiredProvider {
		return nil, false, fmt.Errorf("session %q already uses provider %q (requested %q)", sessionID, existingProvider, desiredProvider)
	}
	return rt, true, nil
}

func (s *serveServer) ensurePersistedSessionForProjectBinding(ctx context.Context, sessionID string, rt *serveRuntime, clientModel string) error {
	if s.store == nil || sessionID == "" || rt == nil {
		return fmt.Errorf("session persistence is unavailable")
	}
	existing, err := s.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session before workspace binding: %w", err)
	}
	if existing != nil {
		return nil
	}
	providerKey := strings.TrimSpace(rt.providerKey)
	providerName := providerKey
	if rt.provider != nil && strings.TrimSpace(rt.provider.Name()) != "" {
		providerName = strings.TrimSpace(rt.provider.Name())
	}
	model := strings.TrimSpace(clientModel)
	if model == "" {
		model = strings.TrimSpace(rt.defaultModel)
	}
	now := time.Now()
	provisional := &session.Session{
		ID: sessionID, Provider: providerName, ProviderKey: providerKey, Model: model,
		Mode: session.ModeChat, Origin: session.OriginWeb, Agent: rt.agentName,
		CreatedAt: now, UpdatedAt: now, Search: rt.search, Tools: rt.toolsSetting,
		MCP: rt.mcpSetting, Status: session.StatusActive,
	}
	if err := s.store.Create(ctx, provisional); err != nil {
		// A concurrent fresh request may have created the same shell. Do not update
		// any runtime metadata until this request wins the conditional binding.
		if raced, getErr := s.store.Get(ctx, sessionID); getErr == nil && raced != nil {
			return nil
		}
		return fmt.Errorf("create session before workspace binding: %w", err)
	}
	return nil
}

// syncPersistedSessionRuntime pins the provider, model, reasoning_effort, and
// explicit GPT-5.6 reasoning mode for the current fresh web conversation. A client may start a fresh
// conversation while reusing an existing session ID, so the persisted session
// row must be updated to match the replacement runtime instead of leaving
// stale provider/model metadata behind from the prior conversation. If the row
// does not yet exist, it is created here so the client-supplied model and
// effort are persisted (rather than the runtime defaults that rt would
// otherwise use when Run creates the row).
func (s *serveServer) syncPersistedSessionRuntime(ctx context.Context, sessionID string, rt *serveRuntime, clientModel, clientEffort, reasoningMode string, syncReasoningMode bool, worktreeDir string, useDefaultWorkspace bool) {
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
	effort := strings.TrimSpace(clientEffort)
	modelName, effort = normalizeProviderModelEffort(providerKey, modelName, effort)
	reasoningMode = strings.ToLower(strings.TrimSpace(reasoningMode))
	if syncReasoningMode && ((reasoningMode != "standard" && reasoningMode != "pro") || !llm.SupportsReasoningMode(providerKey, modelName)) {
		reasoningMode = ""
	}

	requestedWorktree := strings.TrimSpace(worktreeDir)
	requestedRoot := ""
	if requestedWorktree != "" {
		if wt, wtErr := worktree.Get(requestedWorktree); wtErr == nil {
			requestedWorktree = wt.Dir
			if root, rootErr := s.currentGitRoot(); rootErr == nil && sameServePath(requestedWorktree, root) {
				requestedRoot = root
				requestedWorktree = ""
			}
		} else {
			log.Printf("[serve] invalid worktree_dir %q for %s: %v", requestedWorktree, sessionID, wtErr)
			requestedWorktree = ""
		}
	}

	sess, err := s.store.Get(ctx, sessionID)
	if err != nil {
		return
	}
	// The first-party web UI explicitly requests the validated Git root for a
	// new root-checkout session. Omission remains unbound for API clients.
	if requestedRoot == "" && requestedWorktree == "" && useDefaultWorkspace && s.cfg.ui && (sess == nil || (sess.Origin == session.OriginWeb && strings.TrimSpace(sess.CWD) == "" && strings.TrimSpace(sess.WorktreeDir) == "")) {
		if root, rootErr := s.currentGitRoot(); rootErr == nil {
			requestedRoot = root
		}
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
		if syncReasoningMode && reasoningMode != "" {
			sess.ReasoningMode = reasoningMode
		}
		if requestedWorktree != "" {
			sess.WorktreeDir = requestedWorktree
			sess.CWD = requestedWorktree
		} else if requestedRoot != "" {
			sess.CWD = requestedRoot
		}
		if createErr := s.store.Create(ctx, sess); createErr != nil {
			if existing, getErr := s.store.Get(ctx, sessionID); getErr == nil && existing != nil {
				sess = existing
			} else {
				log.Printf("[serve] session Create failed for %s: %v", sessionID, createErr)
				return
			}
		} else {
			if sess.WorktreeDir != "" {
				applyRuntimeWorktreeBaseDir(ctx, s.store, sessionID, rt, sess.WorktreeDir)
			} else if sess.CWD != "" {
				applyRuntimeRootBaseDir(ctx, s.store, sessionID, rt, sess.CWD)
			}
			rt.mu.Lock()
			rt.sessionMeta = sess
			rt.mu.Unlock()
			return
		}
	}

	changed := false
	if strings.TrimSpace(sess.Agent) != strings.TrimSpace(rt.agentName) {
		sess.Agent = strings.TrimSpace(rt.agentName)
		changed = true
	}
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
	if syncReasoningMode && strings.ToLower(strings.TrimSpace(sess.ReasoningMode)) != reasoningMode {
		sess.ReasoningMode = reasoningMode
		changed = true
	}
	acceptedWorktree := strings.TrimSpace(sess.WorktreeDir)
	acceptedRoot := ""
	if requestedRoot != "" {
		switch {
		case acceptedWorktree != "":
			log.Printf("[serve] ignoring conflicting root directory %q for %s already bound to worktree %q", requestedRoot, sessionID, acceptedWorktree)
		case strings.TrimSpace(sess.CWD) == "" || sameServePath(sess.CWD, requestedRoot):
			acceptedRoot = requestedRoot
			if strings.TrimSpace(sess.CWD) == "" || filepath.Clean(sess.CWD) != filepath.Clean(requestedRoot) {
				sess.CWD = requestedRoot
				changed = true
			}
		default:
			log.Printf("[serve] ignoring conflicting root directory %q for %s already bound to %q", requestedRoot, sessionID, sess.CWD)
		}
	}
	if requestedWorktree != "" {
		switch {
		case acceptedWorktree == "":
			sess.WorktreeDir = requestedWorktree
			sess.CWD = requestedWorktree
			acceptedWorktree = requestedWorktree
			changed = true
		case sameServePath(acceptedWorktree, requestedWorktree):
			acceptedWorktree = requestedWorktree
			if filepath.Clean(sess.WorktreeDir) != filepath.Clean(requestedWorktree) {
				sess.WorktreeDir = requestedWorktree
				changed = true
			}
			if strings.TrimSpace(sess.CWD) == "" || (sameServePath(sess.CWD, acceptedWorktree) && filepath.Clean(sess.CWD) != filepath.Clean(requestedWorktree)) {
				sess.CWD = requestedWorktree
				changed = true
			}
		default:
			log.Printf("[serve] ignoring conflicting worktree_dir %q for %s already bound to %q", requestedWorktree, sessionID, acceptedWorktree)
		}
	}
	if changed {
		if err := s.store.Update(ctx, sess); err != nil {
			log.Printf("[serve] session Update failed for %s: %v", sessionID, err)
			return
		}
	}
	if acceptedWorktree != "" {
		applyRuntimeWorktreeBaseDir(ctx, s.store, sessionID, rt, acceptedWorktree)
	} else if acceptedRoot != "" {
		applyRuntimeRootBaseDir(ctx, s.store, sessionID, rt, acceptedRoot)
	}
	rt.mu.Lock()
	rt.sessionMeta = sess
	rt.mu.Unlock()
}

func (s *serveServer) syncPersistedSessionReasoningMode(ctx context.Context, sessionID string, rt *serveRuntime, reasoningMode string) {
	if s.store == nil || sessionID == "" {
		return
	}
	reasoningMode = strings.ToLower(strings.TrimSpace(reasoningMode))
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil || sess == nil || strings.ToLower(strings.TrimSpace(sess.ReasoningMode)) == reasoningMode {
		return
	}
	sess.ReasoningMode = reasoningMode
	if err := s.store.Update(ctx, sess); err != nil {
		log.Printf("[serve] session reasoning mode update failed for %s: %v", sessionID, err)
		return
	}
	if rt != nil {
		rt.mu.Lock()
		rt.sessionMeta = sess
		rt.mu.Unlock()
	}
}

func applyRuntimeRootBaseDir(ctx context.Context, store session.Store, sessionID string, rt *serveRuntime, dir string) {
	dir = strings.TrimSpace(dir)
	if dir == "" || rt == nil || rt.toolMgr == nil {
		return
	}
	if err := rt.toolMgr.ConfigureWorkspacePersistence(ctx, store, sessionID); err != nil {
		log.Printf("[serve] configure workspace persistence failed for %s: %v", sessionID, err)
		return
	}
	if err := rt.toolMgr.SetBaseDirWithContext(ctx, dir); err != nil {
		log.Printf("[serve] set root BaseDir failed for %s: %v", sessionID, err)
	}
}

func applyRuntimeWorktreeBaseDir(ctx context.Context, store session.Store, sessionID string, rt *serveRuntime, dir string) {
	dir = strings.TrimSpace(dir)
	if dir == "" || rt == nil || rt.toolMgr == nil {
		return
	}
	if err := rt.toolMgr.ConfigureWorkspacePersistence(ctx, store, sessionID); err != nil {
		log.Printf("[serve] configure workspace persistence failed for %s: %v", sessionID, err)
		return
	}
	if err := rt.toolMgr.SetBaseDirWithContext(ctx, dir); err != nil {
		log.Printf("[serve] set worktree BaseDir failed for %s: %v", sessionID, err)
		return
	}
	_ = worktree.TouchLastBound(dir)
}

func (s *serveServer) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	pushStore, supported := session.AsPushSubscriptionLifecycleStore(s.store)
	if !supported {
		writeOpenAIError(w, http.StatusServiceUnavailable, "unsupported_error", "persistent push subscription storage is unavailable")
		return
	}
	publicKey := ""
	if s.cfgRef != nil {
		publicKey = strings.TrimSpace(s.cfgRef.Serve.WebPush.VAPIDPublicKey)
		if publicKey == "" {
			writeOpenAIError(w, http.StatusServiceUnavailable, "unsupported_error", "web push is not configured")
			return
		}
	} else {
		// Direct handler tests and embedders that construct serveServer without a
		// config still exercise persistence. Production servers always carry cfgRef.
		publicKey = "unconfigured-test-key"
	}
	vapidKeyID := webPushKeyID(publicKey)

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
		req.Endpoint = strings.TrimSpace(req.Endpoint)
		req.Keys.P256DH = strings.TrimSpace(req.Keys.P256DH)
		req.Keys.Auth = strings.TrimSpace(req.Keys.Auth)
		if req.Endpoint == "" || req.Keys.P256DH == "" || req.Keys.Auth == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "endpoint and keys (p256dh, auth) are required")
			return
		}
		if len(req.Endpoint) > 4096 || len(req.Keys.P256DH) > 1024 || len(req.Keys.Auth) > 1024 {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "subscription fields are too large")
			return
		}
		sub, err := pushStore.UpsertPushSubscription(r.Context(), &session.PushSubscription{
			Endpoint: req.Endpoint, KeyP256DH: req.Keys.P256DH, KeyAuth: req.Keys.Auth,
			Status: "active", VAPIDKeyID: vapidKeyID,
		})
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to save subscription")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": sub.ID, "state": sub.Status, "vapid_key_id": sub.VAPIDKeyID,
		})

	case http.MethodDelete:
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		var req struct {
			ID       string `json:"id"`
			Endpoint string `json:"endpoint"`
		}
		if err := decodeJSONBody(r, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		var err error
		if id := strings.TrimSpace(req.ID); id != "" {
			err = pushStore.DeletePushSubscriptionByID(r.Context(), id)
		} else if endpoint := strings.TrimSpace(req.Endpoint); endpoint != "" {
			// Migration fallback for clients enrolled before canonical IDs existed.
			err = s.store.DeletePushSubscription(r.Context(), endpoint)
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "subscription id is required")
			return
		}
		if err != nil {
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

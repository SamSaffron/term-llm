package cmd

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
)

// handleUpload serves files written by saveUploadedBytes from the first-party
// uploads directory. The route exposes the random upload basename and nothing
// else, so requests can never leave that directory: subdirectories, symlinks,
// and non-regular files all 404.
//
// The response is always an attachment because the same URL is followed from
// the cross-origin Hub proxy, where an anchor's download attribute is ignored,
// and the inferred type for an uploaded .html/.svg would otherwise render in
// place. serveResolvedFile still contributes the response-local CSP sandbox.
func (s *serveServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	uploadsDir := serveUploadsDir()
	if uploadsDir == "" {
		http.NotFound(w, r)
		return
	}

	absFile, err := resolveServeRequestPath(uploadsDir, r.URL.Path, "/uploads/")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// resolveServeRequestPath supplies containment and existence. On top of that
	// the request must name a direct child of the uploads dir whose own entry is
	// a regular file: Lstat on the requested name rather than the resolved target
	// is what stops a symlink from standing in for another file.
	canonicalDir, err := canonicalizeServeDirForWrite(uploadsDir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name := path.Base(r.URL.Path)
	if filepath.Dir(absFile) != canonicalDir || filepath.Base(absFile) != name {
		http.NotFound(w, r)
		return
	}
	info, err := os.Lstat(filepath.Join(canonicalDir, name))
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Disposition", uploadDownloadDisposition(name))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Add("Vary", "Authorization, Cookie")
	serveResolvedFile(w, r, absFile)
}

// uploadFileURL maps a stored upload path to its public download route. It
// returns "" unless the path resolves to an existing regular file directly
// inside the uploads dir, so pruned, foreign, or symlinked paths degrade to a
// non-clickable name chip instead of exposing a server path.
func (s *serveServer) uploadFileURL(filePath string) string {
	if s == nil || strings.TrimSpace(filePath) == "" {
		return ""
	}
	uploadsDir := serveUploadsDir()
	if uploadsDir == "" {
		return ""
	}
	canonicalDir, err := canonicalizeServeDirForWrite(uploadsDir)
	if err != nil {
		return ""
	}
	canonicalPath, err := canonicalizeServeExistingPath(filePath)
	if err != nil || !pathWithinDir(canonicalPath, canonicalDir) || filepath.Dir(canonicalPath) != canonicalDir {
		return ""
	}
	info, err := os.Lstat(canonicalPath)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	// Upload basenames derive from a client-supplied filename, so they routinely
	// contain spaces and may contain "#", "?" or "%". The path is already proven
	// to be a direct child, so escape the basename rather than reusing
	// serveRoutePath, which emits unescaped relative paths for server-generated
	// image names and would truncate such a URL in the browser.
	return s.cfg.uploadsRoute() + url.PathEscape(filepath.Base(canonicalPath))
}

// uploadDownloadDisposition builds the attachment header for an upload. Names
// come from the browser or an API client, so quotes, backslashes, and control
// characters must never reach the header verbatim; non-ASCII names additionally
// carry the RFC 5987 form that browsers prefer.
func uploadDownloadDisposition(name string) string {
	safe := sanitizeUploadDownloadName(name)
	disposition := `attachment; filename="` + safe + `"`
	if !isASCIIFilename(safe) {
		disposition += "; filename*=UTF-8''" + encodeRFC5987Value(safe)
	}
	return disposition
}

// sanitizeUploadDownloadName reduces a stored upload name to something safe for
// a quoted Content-Disposition parameter: no path separators, no CRLF or other
// control characters, and no quote or backslash that could end the parameter.
// Slash handling uses path rather than filepath so the header a client sees does
// not depend on the server's OS.
func sanitizeUploadDownloadName(name string) string {
	name = path.Base(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if r == '"' || r == '\\' || unicode.IsControl(r) {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	safe := strings.TrimSpace(b.String())
	if safe == "" || safe == "." || safe == ".." {
		return "download"
	}
	return safe
}

func isASCIIFilename(name string) bool {
	for i := 0; i < len(name); i++ {
		if name[i] > 0x7f {
			return false
		}
	}
	return true
}

// encodeRFC5987Value percent-encodes a filename for the filename* parameter,
// keeping only the attr-char bytes literal.
func encodeRFC5987Value(value string) string {
	const attrChars = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.IndexByte(attrChars, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteString(fmt.Sprintf("%%%02X", c))
	}
	return b.String()
}

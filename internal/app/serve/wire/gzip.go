package wire

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
)

// JSONGzipMinBytes is the exclusive lower bound for gzip-compressing JSON responses.
const JSONGzipMinBytes = 512

// WriteJSONGzip writes payload as JSON and gzip-compresses the response when
// the client advertises gzip support and the marshaled payload is larger than
// JSONGzipMinBytes. Small responses stay uncompressed to avoid gzip overhead.
func WriteJSONGzip(w http.ResponseWriter, r *http.Request, status int, payload any) {
	body, err := SteeringJSON(w, payload)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	WriteJSONGzipBody(w, r, status, body)
}

func WriteJSONGzipBody(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	AddVary(h, "Accept-Encoding")

	if len(body) > JSONGzipMinBytes && AcceptsGzip(r.Header.Get("Accept-Encoding")) {
		var buf bytes.Buffer
		gz, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
		if err == nil {
			_, err = gz.Write(body)
			if closeErr := gz.Close(); err == nil {
				err = closeErr
			}
		}
		if err == nil {
			body = buf.Bytes()
			h.Set("Content-Encoding", "gzip")
		}
	}

	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// WriteJSONConditional marshals payload, sets an ETag, and returns 304 Not
// Modified when the client's If-None-Match header already holds the current
// ETag. Cache-Control: no-cache tells browsers to always revalidate, so they
// issue a conditional GET rather than skipping the request entirely.
func WriteJSONConditional(w http.ResponseWriter, r *http.Request, status int, payload any) {
	body, err := SteeringJSON(w, payload)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-cache")
	etag := JSONPayloadETag(body)
	h.Set("ETag", etag)
	if ETagMatches(r.Header.Get("If-None-Match"), etag) {
		h.Set("Content-Type", "application/json")
		AddVary(h, "Accept-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	WriteJSONGzipBody(w, r, status, body)
}

func JSONPayloadETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func AddVary(header http.Header, value string) {
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

func AcceptsGzip(headerValue string) bool {
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

func ETagMatches(headerValue, etag string) bool {
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

func CompressibleContentType(contentType string) bool {
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

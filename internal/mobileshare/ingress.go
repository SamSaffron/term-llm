package mobileshare

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

type devicePrincipalKey struct{}

// DevicePrincipal identifies a grant that was verified by the mobile-sharing
// transport. It deliberately carries no owner authority.
type DevicePrincipal struct {
	DeviceID string
	Scope    string
}

// WithDevicePrincipal marks a request as admitted by the peer ingress. It is
// intended only for Runtime's in-process dispatcher.
func WithDevicePrincipal(r *http.Request, principal DevicePrincipal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), devicePrincipalKey{}, principal))
}

// DeviceFromRequest returns the verified mobile device principal, if any.
func DeviceFromRequest(r *http.Request) (DevicePrincipal, bool) {
	principal, ok := r.Context().Value(devicePrincipalKey{}).(DevicePrincipal)
	return principal, ok && principal.DeviceID != "" && principal.Scope == ScopeWebV1
}

// AdmitDevice lets an already verified device pass the normal browser auth
// gate. Owner handlers must still reject device principals explicitly.
func AdmitDevice(next, existing http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := DeviceFromRequest(r); ok {
			next(w, r)
			return
		}
		existing(w, r)
	}
}

const ScopeWebV1 = "web-v1"

var errOutsideBasePath = errors.New("request is outside the shared mount")

// prepareMountedRequest validates the advertised browser mount and strips it
// exactly once. The peer sends browser-visible paths (for example
// /chat/v1/sessions), while serve's inner mux expects /v1/sessions.
func prepareMountedRequest(request *http.Request, basePath string) error {
	if basePath == "" || basePath == "/" {
		return nil
	}
	escaped := request.URL.EscapedPath()
	if escaped != basePath && !strings.HasPrefix(escaped, basePath+"/") {
		return errOutsideBasePath
	}
	path := request.URL.Path
	if path != basePath && !strings.HasPrefix(path, basePath+"/") {
		return errOutsideBasePath
	}
	path = strings.TrimPrefix(path, basePath)
	if path == "" {
		path = "/"
	}
	request.URL.Path = path
	request.URL.RawPath = ""
	request.RequestURI = path
	if request.URL.RawQuery != "" {
		request.RequestURI += "?" + request.URL.RawQuery
	}
	return nil
}

// RouteAllowed is the positive mobile-device route policy. New server routes
// remain denied until deliberately classified here and in the route audit test.
func RouteAllowed(method, path string) bool {
	method = strings.ToUpper(method)
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method == http.MethodOptions || strings.HasPrefix(path, "/v1/mobile-sharing") {
		return false
	}
	if method == http.MethodGet && shellPath(path) {
		return true
	}
	if method == http.MethodGet {
		switch path {
		case "/healthz", "/v1/capabilities", "/v1/providers", "/v1/models", "/v1/sidebar",
			"/v1/projects", "/v1/sessions", "/v1/sessions/status", "/v1/sessions/search",
			"/v1/attention", "/v1/events", "/v1/events/poll":
			return true
		}
		if hasAnyPrefix(path, "/dist/", "/images/", "/media/", "/files/", "/uploads/") {
			return true
		}
		if strings.HasPrefix(path, "/v1/responses/") {
			return true
		}
		if strings.HasPrefix(path, "/v1/sessions/") {
			return sessionReadAllowed(path)
		}
		return false
	}
	if method == http.MethodPost {
		switch path {
		case "/v1/sessions", "/v1/responses", "/v1/mentions/search":
			return true
		}
		if strings.HasPrefix(path, "/v1/responses/") && strings.HasSuffix(path, "/cancel") {
			return true
		}
		if strings.HasPrefix(path, "/v1/sessions/") {
			return hasAnySuffix(path, "/approval", "/ask_user", "/attention/seen")
		}
	}
	return false
}

func shellPath(path string) bool {
	if path == "/" || path == "/manifest.webmanifest" || path == "/sw.js" || path == "/favicon.ico" {
		return true
	}
	if path == "/chat" {
		return true
	}
	if !strings.HasPrefix(path, "/chat/") {
		return false
	}
	rest := strings.TrimPrefix(path, "/chat/")
	return rest != "" && !strings.Contains(rest, "/") && !strings.Contains(rest, ".")
}

func sessionReadAllowed(path string) bool {
	rest := strings.TrimPrefix(path, "/v1/sessions/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 1 {
		return parts[0] != ""
	}
	suffix := parts[1]
	switch suffix {
	case "messages", "state", "transcript", "transcript/bodies", "attention/unseen", "children",
		"runtime/approvals", "diff-comments", "inspect", "inspect/bodies":
		return true
	}
	return strings.HasPrefix(suffix, "skill-runs/")
}

func hasAnyPrefix(value string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func hasAnySuffix(value string, suffixes ...string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(value, suffix) {
			return true
		}
	}
	return false
}

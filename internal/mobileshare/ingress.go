package mobileshare

import (
	"context"
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
	// SPA routes do not contain an extension and are outside API/admin mounts.
	return !strings.HasPrefix(path, "/v") && !strings.HasPrefix(path, "/api/") &&
		!strings.HasPrefix(path, "/admin/") && !strings.HasPrefix(path, "/auth/") &&
		!strings.HasPrefix(path, "/widgets") && !strings.HasPrefix(path, "/extensions/") &&
		!strings.Contains(path, ".")
}

func sessionReadAllowed(path string) bool {
	return !strings.Contains(path, "/shell") && !strings.Contains(path, "/mcp") &&
		!strings.Contains(path, "/publish")
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

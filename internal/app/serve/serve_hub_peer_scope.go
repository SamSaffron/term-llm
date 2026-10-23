package serve

import (
	"net/http"
	"strings"
)

// peerChatRoute is a positive METHOD/PATH allowlist relative to one fixed node
// mount. Do not replace it with a blacklist: the node can itself be a Hub, and
// forwarding an unlisted route with its injected bearer would delegate that
// Hub's arbitrary-target proxy/registration/configuration authority.
func peerChatRoute(method, path string) bool {
	read := method == http.MethodGet || method == http.MethodHead
	if path == "" || path == "/" {
		return read
	}
	if read {
		switch path {
		case "/index.html", "/manifest.webmanifest", "/sw.js", "/icon-512.png", "/favicon.ico", "/healthz":
			return true
		case "/v1/capabilities", "/v1/providers", "/v1/models", "/v1/sharing/capabilities", "/v1/sidebar", "/v1/sessions", "/v1/sessions/status", "/v1/sessions/search", "/v1/projects", "/v1/events", "/v1/events/poll", "/v1/attention":
			return true
		}
		for _, prefix := range []string{"/dist/", "/images/", "/media/", "/uploads/"} {
			if strings.HasPrefix(path, prefix) && len(path) > len(prefix) {
				return true
			}
		}
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if read && len(parts) == 1 && parts[0] == "chat" {
		return true
	}
	if read && len(parts) == 2 && parts[0] == "chat" && peerChatID(parts[1]) {
		return true
	}
	if len(parts) < 2 || parts[0] != "v1" {
		return false
	}
	if method == http.MethodPost && len(parts) == 2 && (parts[1] == "responses" || parts[1] == "sessions") {
		return true
	}
	if method == http.MethodPost && len(parts) == 3 && parts[1] == "mentions" && parts[2] == "search" {
		return true
	}
	if len(parts) < 3 || !peerChatID(parts[2]) {
		return false
	}
	switch parts[1] {
	case "responses":
		if read && (len(parts) == 3 || len(parts) == 4 && parts[3] == "events") {
			return true
		}
		if method == http.MethodPost && len(parts) == 4 && parts[3] == "cancel" {
			return true
		}
		return method == http.MethodPost && len(parts) == 6 && parts[3] == "client_tool_calls" && peerChatID(parts[4]) && parts[5] == "result"
	case "sessions":
		if read && len(parts) == 3 {
			return true
		}
		if read && len(parts) == 4 {
			switch parts[3] {
			case "state", "stats", "children", "inspect", "tree", "project":
				return true
			}
		}
		if read && len(parts) == 5 {
			return (parts[3] == "inspect" || parts[3] == "transcript") && parts[4] == "bodies" || parts[3] == "runtime" && parts[4] == "approvals"
		}
		if method == http.MethodPost && len(parts) == 4 && (parts[3] == "approval" || parts[3] == "ask_user") {
			return true
		}
		return method == http.MethodPost && len(parts) == 5 && parts[3] == "attention" && parts[4] == "seen"
	case "projects":
		return read && len(parts) == 3
	default:
		return false
	}
}
func peerChatID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

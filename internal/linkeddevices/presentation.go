package linkeddevices

import (
	"bytes"
	"net/http"
)

// ShapeCapabilities replaces owner-only feature claims with the deliberately
// narrow web-v1 peer profile. It returns true when a peer profile was applied.
func ShapeCapabilities(request *http.Request, payload map[string]any) bool {
	principal, ok := DeviceFromRequest(request)
	if !ok {
		return false
	}
	payload["projects"] = map[string]bool{"enabled": false}
	payload["worktrees"] = map[string]bool{"enabled": false}
	payload["shell"] = map[string]any{"enabled": false, "reason": "not available through Linked devices"}
	payload["live"] = map[string]any{"enabled": false, "reason": "not available through Linked devices"}
	payload["attachments"] = map[string]any{"enabled": false}
	payload["linked_devices"] = map[string]any{
		"active": true, "scope": principal.Scope, "owner_controls": false,
		"transcript_sharing": false, "uploads": false, "voice": false,
		"push_notifications": false, "device_tools": false,
	}
	return true
}

// PeerIndexHTML adds a request-local authentication marker without mutating the
// cached owner shell.
func PeerIndexHTML(request *http.Request, body []byte) []byte {
	principal, ok := DeviceFromRequest(request)
	if !ok {
		return body
	}
	snippet := []byte(`<script>window.TERM_LLM_AUTH_MODE="linked-devices";window.TERM_LLM_LINKED_DEVICES_SCOPE="` + principal.Scope + `";</script></head>`)
	return bytes.Replace(body, []byte("</head>"), snippet, 1)
}

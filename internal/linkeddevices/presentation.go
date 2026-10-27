package linkeddevices

import (
	"bytes"
	"net/http"
)

// ShapeCapabilities replaces owner-only feature claims with the deliberately
// narrow web-v1 peer profile and marks the response private when applied.
func ShapeCapabilities(response http.ResponseWriter, request *http.Request, payload map[string]any) {
	principal, ok := DeviceFromRequest(request)
	if !ok {
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	payload["projects"] = map[string]bool{"enabled": false}
	payload["worktrees"] = map[string]bool{"enabled": false}
	payload["shell"] = map[string]any{"enabled": false, "reason": "not available through Linked devices"}
	// Keep provider readiness and transport exactly as advertised by the host.
	// Linked calls delegate only to the server, never to device/page tools.
	voice := false
	if advertised, ok := payload["live"].(map[string]any); ok {
		live := make(map[string]any, len(advertised))
		for key, value := range advertised {
			live[key] = value
		}
		live["delegation_modes"] = []string{"server"}
		voice, _ = live["enabled"].(bool)
		payload["live"] = live
	}
	payload["attachments"] = map[string]any{"enabled": false}
	payload["linked_devices"] = map[string]any{
		"active": true, "scope": principal.Scope, "owner_controls": false,
		"transcript_sharing": false, "uploads": false, "voice": voice,
		"push_notifications": false, "device_tools": false,
	}
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

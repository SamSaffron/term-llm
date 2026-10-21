package serve

import (
	"net/http"
	"strings"

	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	pushStore, supported := session.AsPushSubscriptionLifecycleStore(s.store)
	if !supported {
		writeOpenAIError(w, http.StatusServiceUnavailable, "unsupported_error", "persistent push subscription storage is unavailable")
		return
	}
	publicKey := ""
	if s.cfgRef != nil {
		var err error
		publicKey, err = appnotify.WebPushPublicKey(s.cfgRef)
		if err != nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "unsupported_error", "web push key is temporarily unavailable")
			return
		}
		if publicKey == "" {
			writeOpenAIError(w, http.StatusServiceUnavailable, "unsupported_error", "web push is not configured")
			return
		}
	} else {
		// Direct handler tests and embedders that construct serveServer without a
		// config still exercise persistence. Production servers always carry cfgRef.
		publicKey = "unconfigured-test-key"
	}
	vapidKeyID := appnotify.WebPushKeyID(publicKey)

	switch r.Method {
	case http.MethodPost:
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		var req struct {
			Endpoint   string `json:"endpoint"`
			ClientKind string `json:"client_kind"`
			Keys       struct {
				P256DH string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
		}
		if err := decodeJSONBody(r, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if req.ClientKind == "" {
			req.ClientKind = session.PushClientBrowser
		}
		if req.ClientKind != session.PushClientBrowser && req.ClientKind != session.PushClientApp {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "client_kind must be browser or app")
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
			Status: "active", VAPIDKeyID: vapidKeyID, ClientKind: req.ClientKind,
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

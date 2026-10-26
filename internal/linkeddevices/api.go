package linkeddevices

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/samsaffron/term-llm/internal/pairing"
	qrcode "github.com/skip2/go-qrcode"
)

// Handler serves the owner-only API. The containing server must still apply
// its normal bearer/passkey authentication middleware.
func (r *Runtime) Handler() http.Handler { return http.HandlerFunc(r.handleOwner) }

func (r *Runtime) handleOwner(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, device := DeviceFromRequest(request); device {
		http.Error(w, "manage linked devices from this server's own browser session", http.StatusForbidden)
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead && !sameOrigin(request) {
		http.Error(w, "same-origin request required", http.StatusForbidden)
		return
	}
	path := strings.TrimPrefix(request.URL.Path, "/v1/linked-devices")
	if request.Method != http.MethodGet && request.Method != http.MethodHead && !r.ownerMutationAllowed(path == "/reset") {
		http.Error(w, "linked devices are unavailable", http.StatusConflict)
		return
	}
	switch {
	case path == "" || path == "/":
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			methodNotAllowed(w, "GET, HEAD")
			return
		}
		writeOwnerJSON(w, http.StatusOK, r.Status())
	case path == "/invitations" && request.Method == http.MethodPost:
		if err := r.NewInvitation(request.Context(), encodeQR); err != nil {
			writeOwnerError(w, err)
			return
		}
		writeOwnerJSON(w, http.StatusCreated, r.status(true))
	case path == "/invitations/current" && request.Method == http.MethodDelete:
		r.CancelInvitation()
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/requests/") && request.Method == http.MethodPost:
		r.handleRequestAction(w, request, strings.TrimPrefix(path, "/requests/"))
	case strings.HasPrefix(path, "/devices/") && request.Method == http.MethodDelete:
		id, err := url.PathUnescape(strings.TrimPrefix(path, "/devices/"))
		if err != nil || id == "" || strings.Contains(id, "/") {
			http.NotFound(w, request)
			return
		}
		if err := r.Revoke(request.Context(), id); err != nil {
			writeOwnerError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case path == "/stop" && request.Method == http.MethodPost:
		if err := r.Stop(); err != nil {
			writeOwnerError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case path == "/resume" && request.Method == http.MethodPost:
		if err := r.Enable(request.Context()); err != nil {
			writeOwnerError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case path == "/reset" && request.Method == http.MethodPost:
		if err := r.Reset(request.Context()); err != nil {
			writeOwnerError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, request)
	}
}

func (r *Runtime) handleRequestAction(w http.ResponseWriter, request *http.Request, rest string) {
	id, action, ok := strings.Cut(rest, "/")
	decoded, err := url.PathUnescape(id)
	if !ok || err != nil || decoded == "" || strings.Contains(decoded, "/") {
		http.NotFound(w, request)
		return
	}
	switch action {
	case "approve":
		var body approveRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			http.Error(w, "invalid approval request", http.StatusBadRequest)
			return
		}
		if err := r.Approve(request.Context(), decoded, body.Code); err != nil {
			writeOwnerError(w, err)
			return
		}
	case "deny":
		if err := r.Deny(decoded); err != nil {
			writeOwnerError(w, err)
			return
		}
	default:
		http.NotFound(w, request)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *Runtime) ownerMutationAllowed(reset bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unlock == nil {
		return false
	}
	if r.availableReason == "" {
		return true
	}
	return reset && (strings.Contains(r.availableReason, "reset is required") || strings.Contains(r.availableReason, "rendezvous service changed"))
}

func sameOrigin(request *http.Request) bool {
	site := strings.ToLower(strings.TrimSpace(request.Header.Get("Sec-Fetch-Site")))
	if site == "cross-site" || site == "same-site" {
		return false
	}
	if site == "same-origin" {
		return true
	}
	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host != "" && strings.EqualFold(parsed.Host, request.Host)
}

func encodeQR(value string) (string, error) {
	png, err := qrcode.Encode(value, qrcode.Medium, 256)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

func writeOwnerJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}
func writeOwnerError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, pairing.ErrUnknownInvitation) || errors.Is(err, pairing.ErrGrantNotFound) {
		status = http.StatusNotFound
	}
	if errors.Is(err, pairing.ErrDenied) {
		status = http.StatusForbidden
	}
	http.Error(w, "linked-device request failed", status)
}

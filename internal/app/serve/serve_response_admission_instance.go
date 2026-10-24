package serve

import (
	"crypto/rand"
	"net/http"
)

const responseServerInstanceHeader = "X-Term-LLM-Server-Instance"

// responseAdmissionInstance is intentionally not persisted or handed over on
// reload. Memory-only idempotency claims must never be replayed against a new
// server incarnation that may have forgotten an accepted mutation.
func (s *serveServer) responseAdmissionInstance() string {
	s.admissionInstanceOnce.Do(func() { s.admissionInstanceID = "srv_" + rand.Text() })
	return s.admissionInstanceID
}

func (s *serveServer) allowResponseAdmission(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return false
	}
	values := r.Header.Values(responseServerInstanceHeader)
	if len(values) > 0 && (len(values) != 1 || values[0] != s.responseAdmissionInstance()) {
		writeOpenAIError(w, http.StatusConflict, "server_instance_changed", "server instance changed; the earlier send may still be running")
		return false
	}
	return true
}

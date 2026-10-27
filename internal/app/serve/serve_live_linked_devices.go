package serve

import (
	"encoding/json"
	"net/http"

	"github.com/samsaffron/term-llm/internal/linkeddevices"
)

// decodeLiveStart shares ordinary SDP validation while applying linked admission
// before session reservation, provider startup or any tool declarations.
func (s *serveServer) decodeLiveStart(w http.ResponseWriter, r *http.Request) (liveStartRequest, bool) {
	var request liveStartRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, liveSDPLimitBytes+liveTextLimitBytes+liveClientToolsLimitBytes)).Decode(&request); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
		return request, false
	}
	if err := linkeddevices.ValidateLiveStart(r, request, s.liveConfig().Provider); err != nil {
		writeOpenAIError(w, err.Status, err.Type, err.Message)
		return request, false
	}
	// Do not trim SDP: the offer is forwarded byte for byte, including CRLF.
	// Gemini uses server-proxied PCM and therefore has no SDP.
	if problem := liveOfferProblem(s.liveConfig().Provider, request.SDP); problem != "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", problem)
		return request, false
	}
	return request, true
}

func (s *serveServer) allowLinkedLiveRequest(w http.ResponseWriter, r *http.Request) bool {
	err := linkeddevices.AuthorizeLiveRequest(r, func(id string) *linkeddevices.LiveOwner {
		if record, ok := s.lookupLiveSession(id); ok {
			return record.linkedOwner
		}
		return nil
	})
	if err != nil {
		writeOpenAIError(w, err.Status, err.Type, err.Message)
	}
	return err == nil
}

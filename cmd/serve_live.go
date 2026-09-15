package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/livecall"
)

// The existing refresh API can block on a cross-process credential lock. Bound
// outstanding calls into it even when an HTTP request or child has been canceled.
var liveChatGPTTokenGate = make(chan struct{}, 1)

// Use the same native OAuth store and generation-safe refresh as the ChatGPT
// provider. Never prompt, print credential errors, or copy refresh tokens to Codex.
func liveChatGPTTokens(ctx context.Context, force bool) (livecall.Tokens, error) {
	select {
	case liveChatGPTTokenGate <- struct{}{}:
		defer func() { <-liveChatGPTTokenGate }()
	case <-ctx.Done():
		return livecall.Tokens{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return livecall.Tokens{}, err
	}
	creds, err := credentials.GetChatGPTCredentials()
	if err != nil {
		return livecall.Tokens{}, livecall.ErrUnavailable
	}
	if force || creds.IsExpired() {
		if err = credentials.RefreshChatGPTCredentials(creds); err != nil {
			return livecall.Tokens{}, livecall.ErrUnavailable
		}
	}
	if err := ctx.Err(); err != nil {
		return livecall.Tokens{}, err
	}
	return livecall.Tokens{AccessToken: creds.AccessToken, AccountID: creds.AccountID}, nil
}

func (s *serveServer) handleLiveCalls(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	// Unlike existing APIs this resource must never spend OAuth quota anonymously.
	if !s.cfg.requireAuth && s.browserAuth == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "live_unavailable", "live calls require serve authentication")
		return
	}
	if s.cfg.liveCodexPath == "" {
		writeOpenAIError(w, http.StatusServiceUnavailable, "live_unavailable", "live calls disabled; configure --live-codex-path")
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", "Content-Type must be application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, livecall.MaxBody))
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request exceeds 131072 bytes")
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "cannot read request")
		}
		return
	}
	var offer livecall.Offer
	// Decode exactly one strict object; no opaque item/tool payloads reach Codex.
	if err = decodeLiveOffer(body, &offer); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	s.liveCallsOnce.Do(func() {
		s.liveCalls = &livecall.Manager{Path: s.cfg.liveCodexPath, Tokens: liveChatGPTTokens, Log: func(message string) { log.Printf("live calls: %s", message) }}
	})
	if s.liveCalls == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "live_unavailable", "server stopping")
		return
	}
	answer, err := s.liveCalls.Start(r.Context(), offer)
	if err != nil {
		status := http.StatusBadGateway
		code := "live_setup_failed"
		message := "Codex live setup failed; check non-secret server diagnostics"
		if errors.Is(err, livecall.ErrUnavailable) {
			status = http.StatusServiceUnavailable
			code = "live_unavailable"
			message = "Codex executable or configured ChatGPT OAuth unavailable"
		} else if errors.Is(err, livecall.ErrCapacity) {
			status = http.StatusTooManyRequests
			code = "live_capacity"
			message = "live call capacity reached"
		} else if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
			message = "live setup timed out"
		}
		writeOpenAIError(w, status, code, message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(answer); err != nil {
		answer.Cancel()
	}
}

func decodeLiveOffer(body []byte, offer *livecall.Offer) error {
	if !utf8.Valid(body) {
		return errors.New("request must be UTF-8 JSON")
	}
	if err := checkLiveJSON(json.NewDecoder(bytes.NewReader(body)), 0); err != nil {
		return errors.New("invalid live JSON: use exact field names, no duplicate keys or null values")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(offer); err != nil {
		return errors.New("invalid live offer fields")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("request must contain one JSON object")
	}
	return offer.Validate()
}

func liveJSONFieldAllowed(depth int, key string) bool {
	if depth == 0 {
		return key == "sdp" || key == "instructions" || key == "initial_items"
	}
	return depth == 2 && (key == "role" || key == "text")
}

func checkLiveJSON(d *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("too deeply nested")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return errors.New("null")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return errors.New("unexpected delimiter")
	}
	keys := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			k, ok := key.(string)
			if !ok || keys[k] {
				return errors.New("duplicate key")
			}
			if !liveJSONFieldAllowed(depth, k) {
				return errors.New("unknown field")
			}
			keys[k] = true
		}
		if err := checkLiveJSON(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

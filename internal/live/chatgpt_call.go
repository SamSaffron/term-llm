package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ErrUnauthorized reports a stale or invalid token, so the caller may refresh
// the credentials and retry once.
var ErrUnauthorized = errors.New("live: unauthorized")

// ErrForbidden reports that the provider refused the requested live session.
// This can indicate account access or an incompatible voice/protocol pairing,
// not just invalid credentials. Surface the message without refreshing tokens.
var ErrForbidden = errors.New("live: access denied")

// realtimeAlphaProtocol selects the frameless bidirectional protocol. The AVAS
// architecture rejects a call without it ("AVAS requires OpenAI-Alpha:
// quicksilver=v2"), and the control channel is joined with the same headers.
const realtimeAlphaProtocol = "quicksilver=v2"

// Auth carries the identity used for call creation and the control channel.
type Auth struct {
	AccessToken string
	AccountID   string
	// Headers carries the shared client identity (originator, User-Agent).
	Headers map[string]string
}

// AuthFunc resolves credentials. refresh asks for a forced token refresh after
// the provider rejected the previous token.
type AuthFunc func(ctx context.Context, refresh bool) (Auth, error)

// apply sets the identity and protocol headers every realtime request carries.
func (a Auth) apply(header http.Header, sessionID string) {
	header.Set("openai-alpha", realtimeAlphaProtocol)
	for key, value := range a.Headers {
		if strings.TrimSpace(value) == "" {
			continue
		}
		header.Set(key, value)
	}
	if token := strings.TrimSpace(a.AccessToken); token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	if accountID := strings.TrimSpace(a.AccountID); accountID != "" {
		header.Set("ChatGPT-Account-ID", accountID)
	}
	if id := strings.TrimSpace(sessionID); id != "" {
		header.Set("x-session-id", id)
	}
}

// CallRequest is the body of a realtime call creation request.
type CallRequest struct {
	SDP     string          `json:"sdp"`
	Session json.RawMessage `json:"session"`
}

// CallResponse is the provider's answer to a call creation request.
type CallResponse struct {
	AnswerSDP string
	CallID    string
}

const callErrorBodyLimit = 4 << 10

// CreateCall exchanges an SDP offer for the provider's answer and call id.
func CreateCall(ctx context.Context, client *http.Client, baseURL string, auth Auth, sessionID string, request CallRequest) (CallResponse, error) {
	if client == nil {
		client = http.DefaultClient
	}
	endpoint, err := callURL(baseURL)
	if err != nil {
		return CallResponse{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return CallResponse{}, fmt.Errorf("encode live call request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return CallResponse{}, fmt.Errorf("build live call request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	auth.apply(httpReq.Header, sessionID)

	resp, err := client.Do(httpReq)
	if err != nil {
		return CallResponse{}, fmt.Errorf("create live call: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return CallResponse{}, fmt.Errorf("%w: %s", ErrUnauthorized, callErrorMessage(resp))
	case http.StatusForbidden:
		return CallResponse{}, fmt.Errorf("%w: %s", ErrForbidden, callErrorMessage(resp))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return CallResponse{}, fmt.Errorf("create live call: http %d: %s", resp.StatusCode, callErrorMessage(resp))
	}

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return CallResponse{}, fmt.Errorf("read live call answer: %w", err)
	}
	// Hand the answer back unmodified for the same reason the offer is not
	// trimmed: the browser's SDP parser needs the trailing CRLF.
	sdp := string(answer)
	if strings.TrimSpace(sdp) == "" {
		return CallResponse{}, errors.New("create live call: empty answer sdp")
	}
	callID := ParseCallID(resp.Header.Get("Location"))
	if callID == "" {
		return CallResponse{}, errors.New("create live call: response is missing a call id")
	}
	return CallResponse{AnswerSDP: sdp, CallID: callID}, nil
}

func callURL(baseURL string) (string, error) {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return "", errors.New("live: call base url is empty")
	}
	parsed, err := url.Parse(strings.TrimRight(trimmed, "/"))
	if err != nil {
		return "", fmt.Errorf("live: invalid call base url %q: %w", baseURL, err)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/realtime/calls"
	query := parsed.Query()
	query.Set("intent", "quicksilver")
	query.Set("architecture", "avas")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func callErrorMessage(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, callErrorBodyLimit))
	message := strings.TrimSpace(string(body))
	if message == "" {
		return http.StatusText(resp.StatusCode)
	}
	return message
}

// ParseCallID extracts the call id from a Location header. The provider
// returns paths such as /v1/live/rtc_abc or /v1/live/<uuid>.
func ParseCallID(location string) string {
	trimmed := strings.TrimSpace(location)
	if trimmed == "" {
		return ""
	}
	path := trimmed
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Path != "" {
		path = parsed.Path
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for i := len(segments) - 1; i >= 0; i-- {
		segment := strings.TrimSpace(segments[i])
		if segment == "" {
			continue
		}
		if strings.HasPrefix(segment, "rtc_") || isUUID(segment) {
			return segment
		}
	}
	return ""
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

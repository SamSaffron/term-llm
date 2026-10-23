package hubnode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RegisterRequest is the body of POST /api/register-node.
type RegisterRequest struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Connection string `json:"connection,omitempty"`
	BasePath   string `json:"base_path"`
	Token      string `json:"token"`
}

// StatusError reports a Hub response that rejected a registration request.
type StatusError struct {
	StatusCode int
	Message    string
	Op         string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("hub %s failed: HTTP %d: %s", e.Op, e.StatusCode, e.Message)
}

// Register creates or updates the node's registration on the Hub.
func Register(ctx context.Context, client *http.Client, hubURL, registrationToken string, req RegisterRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode registration request: %w", err)
	}
	return call(ctx, client, http.MethodPost, hubURL, "", registrationToken, body, "registration")
}

// Unregister removes the node's registration from the Hub.
func Unregister(ctx context.Context, client *http.Client, hubURL, registrationToken, id string) error {
	return call(ctx, client, http.MethodDelete, hubURL, "/"+url.PathEscape(strings.TrimSpace(id)), registrationToken, nil, "deregistration")
}

func call(ctx context.Context, client *http.Client, method, hubURL, suffix, registrationToken string, body []byte, op string) error {
	if client == nil {
		client = http.DefaultClient
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(hubURL), "/") + "/api/register-node" + suffix)
	if err != nil {
		return fmt.Errorf("parse hub url: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(callCtx, method, u.String(), reader)
	if err != nil {
		return err
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(registrationToken))
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = resp.Status
		}
		return &StatusError{StatusCode: resp.StatusCode, Message: msg, Op: op}
	}
	return nil
}

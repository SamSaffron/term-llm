package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
)

const hubRegistrationTokenEnv = "TERM_LLM_HUB_REGISTRATION_TOKEN"

var hubRegistrationCfg struct {
	sync.Mutex
	token        string
	tokenFromEnv bool
}

func init() { captureHubRegistrationEnv() }

func captureHubRegistrationEnv() {
	v := strings.TrimSpace(os.Getenv(hubRegistrationTokenEnv))
	if v == "" {
		return
	}
	hubRegistrationCfg.Lock()
	if hubRegistrationCfg.token == "" {
		hubRegistrationCfg.token = v
		hubRegistrationCfg.tokenFromEnv = true
	}
	hubRegistrationCfg.Unlock()
	_ = os.Unsetenv(hubRegistrationTokenEnv)
}

type hubRegisterNodeRequest struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Connection string `json:"connection,omitempty"`
	BasePath   string `json:"base_path"`
	Token      string `json:"token"`
}

type hubRegistrationInfo struct {
	Enabled           bool   `json:"enabled"`
	TokenConfigured   bool   `json:"token_configured"`
	CanPersistNodes   bool   `json:"can_persist_nodes"`
	RegistrationToken string `json:"registration_token,omitempty"`
}

func hubRegistrationRoute(r *http.Request) bool {
	return r.URL.Path == "/api/register-node"
}

func (s *hubServer) handleRegistrationInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !hubBrowserRequestAllowed(r, false) {
		http.Error(w, "forbidden cross-site hub request", http.StatusForbidden)
		return
	}
	token := strings.TrimSpace(s.registrationToken)
	info := hubRegistrationInfo{
		Enabled:         token != "" && s.store != nil,
		TokenConfigured: token != "",
		CanPersistNodes: s.store != nil,
	}
	if info.Enabled {
		info.RegistrationToken = token
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, info)
}

func hubRegistrationBearerTokenMatches(r *http.Request, want string) bool {
	return hubTokenMatches(strings.TrimSpace(want), bearerTokenFromHeader(r))
}

func (s *hubServer) handleRegisterNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.TrimSpace(s.registrationToken) == "" {
		http.NotFound(w, r)
		return
	}
	if !hubRegistrationBearerTokenMatches(r, s.registrationToken) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid hub registration token")
		return
	}
	if s.store == nil {
		http.Error(w, "node persistence is disabled", http.StatusForbidden)
		return
	}

	var req hubRegisterNodeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		http.Error(w, "node token is required", http.StatusBadRequest)
		return
	}
	connection := strings.ToLower(strings.TrimSpace(req.Connection))
	if connection == "" {
		connection = "reverse"
	}
	if connection != "reverse" {
		http.Error(w, "registration currently supports reverse nodes only", http.StatusBadRequest)
		return
	}

	node := hub.Node{
		ID:         strings.TrimSpace(req.ID),
		Name:       strings.TrimSpace(req.Name),
		Connection: "reverse",
		BasePath:   strings.TrimSpace(req.BasePath),
		Token:      strings.TrimSpace(req.Token),
	}
	if err := node.Normalize(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if existing, ok := s.registry.Lookup(node.ID); ok && existing.Source != hub.SourceLocal {
		http.Error(w, fmt.Sprintf("node id %q is owned by %s and cannot be replaced by registration", node.ID, existing.Source), http.StatusConflict)
		return
	}

	stored, created, err := s.store.Upsert(node)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("hub registration: %s reverse node %q", map[bool]string{true: "created", false: "updated"}[created], stored.ID)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"node": hubNodeView{
			ID:         stored.ID,
			Name:       stored.Name,
			Source:     stored.Source,
			Connection: stored.Connection,
			BasePath:   stored.BasePath,
			ProxyPath:  "/node/" + stored.ID + "/",
			HasToken:   stored.Token != "",
		},
		"created": created,
	})
}

func resolveServeHubRegistrationToken(flagValue string) string {
	captureHubRegistrationEnv()
	if v := strings.TrimSpace(flagValue); v != "" {
		return v
	}
	hubRegistrationCfg.Lock()
	defer hubRegistrationCfg.Unlock()
	return hubRegistrationCfg.token
}

func hubRegistrationEnviron() []string {
	hubRegistrationCfg.Lock()
	defer hubRegistrationCfg.Unlock()
	if !hubRegistrationCfg.tokenFromEnv || hubRegistrationCfg.token == "" {
		return nil
	}
	return []string{hubRegistrationTokenEnv + "=" + hubRegistrationCfg.token}
}

func resetHubRegistrationForTest() func() {
	hubRegistrationCfg.Lock()
	prevToken, prevFromEnv := hubRegistrationCfg.token, hubRegistrationCfg.tokenFromEnv
	hubRegistrationCfg.token, hubRegistrationCfg.tokenFromEnv = "", false
	hubRegistrationCfg.Unlock()
	return func() {
		hubRegistrationCfg.Lock()
		hubRegistrationCfg.token, hubRegistrationCfg.tokenFromEnv = prevToken, prevFromEnv
		hubRegistrationCfg.Unlock()
	}
}

func registerServeHubNode(ctx context.Context, client *http.Client, hubURL, registrationToken string, req hubRegisterNodeRequest) error {
	if client == nil {
		client = http.DefaultClient
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(hubURL), "/") + "/api/register-node")
	if err != nil {
		return fmt.Errorf("parse hub url: %w", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode registration request: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
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
		return fmt.Errorf("hub registration failed: HTTP %d: %s", resp.StatusCode, msg)
	}
	return nil
}

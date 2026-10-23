package serve

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/app/reload"
	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
	"github.com/samsaffron/term-llm/internal/app/serve/hubnode"
	"github.com/samsaffron/term-llm/internal/hub"
)

const hubRegistrationTokenEnv = apphub.RegistrationTokenEnv

var hubRegistrationCfg struct {
	sync.Mutex
	token        string
	tokenFromEnv bool
}

func init() {
	captureHubRegistrationEnv()
	reload.RegisterEnviron(hubRegistrationEnviron)
}

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

type hubRegisterNodeRequest = hubnode.RegisterRequest

type hubRegistrationInfo struct {
	Enabled           bool   `json:"enabled"`
	TokenConfigured   bool   `json:"token_configured"`
	CanPersistNodes   bool   `json:"can_persist_nodes"`
	RegistrationToken string `json:"registration_token,omitempty"`
}

func hubRegistrationRoute(r *http.Request) bool {
	return r.URL.Path == "/api/register-node" || strings.HasPrefix(r.URL.Path, "/api/register-node/")
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
	if r.URL.Path != "/api/register-node" {
		s.handleUnregisterNode(w, r)
		return
	}
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
			ProxyPath:  s.hubPath("/node/" + stored.ID + "/"),
			HasToken:   stored.Token != "",
		},
		"created": created,
	})
}

func (s *hubServer) handleUnregisterNode(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/register-node/")
	if id == "" || strings.Contains(id, "/") || hub.ValidateID(id) != nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
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
	storedNodes, err := s.store.Nodes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, existing := range storedNodes {
		if existing.ID == id && !existing.UsesReverseConnection() {
			http.Error(w, fmt.Sprintf("node %q is not a reverse registered node", id), http.StatusForbidden)
			return
		}
	}

	removed, err := s.store.RemoveIfExists(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if removed {
		log.Printf("hub registration: removed reverse node %q", id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "removed": removed})
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

// Node-side registration clients live in hubnode; these names keep the Hub
// server and its tests on one request shape.
var (
	registerServeHubNode   = hubnode.Register
	unregisterServeHubNode = hubnode.Unregister
)

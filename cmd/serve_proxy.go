package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/proxyadminui"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/serve/proxy"
	"github.com/spf13/cobra"
)

// maxProxyRequestBody bounds the request body the gate buffers in memory to
// inspect and rewrite the requested model before forwarding. LLM chat/response
// payloads are text (plus base64 media) and comfortably fit; a tight cap keeps
// a single authenticated client from pinning large buffers. Oversize requests
// are rejected with 413 rather than silently truncated.
const maxProxyRequestBody = 8 << 20

var (
	serveProxyAdminToken string
	serveProxyDBPath     string
)

func init() {
	serveCmd.Flags().StringVar(&serveProxyAdminToken, "proxy-admin-token", "",
		"Admin API token for `serve proxy` (defaults to $TERM_LLM_PROXY_ADMIN_TOKEN, else auto-generated). Distinct from client bearer tokens.")
	serveCmd.Flags().StringVar(&serveProxyDBPath, "proxy-db", "",
		"Path to the proxy SQLite database (default: <data-dir>/proxy.db)")
}

// proxyServer is the HTTP surface for `term-llm serve proxy`: a standalone,
// locked-down capability proxy. It separates two auth planes:
//
//   - admin plane: a single --proxy-admin-token that manages clients, tokens,
//     grants, access requests, and audit.
//   - client plane: per-client hashed+expiring bearer tokens (in the store)
//     that authorize calls to granted provider/model aliases only.
//
// Session and response-chaining state is namespaced per authenticated client so
// two clients can never share or hijack a session/runtime. The locked-down
// runtime is a pure provider pass-through: zero server-executable tools, no
// skills, no MCP, no injected system prompt, and no agent memory.
//
// PROTOTYPE LIMITATIONS: single admin token (no admin accounts/rotation), no
// per-request quotas or spend limits (pending access requests are capped per
// client but token throughput is not), and no streaming-time re-authorization.
// See command help.
type proxyServer struct {
	store       *proxy.Store
	catalog     *proxy.Catalog
	adminToken  string
	requireAuth bool
	verbose     bool
	errw        io.Writer
}

type proxyClientKeyType struct{}

var proxyClientKey proxyClientKeyType

func proxyClientFromContext(ctx context.Context) (*proxy.Client, bool) {
	c, ok := ctx.Value(proxyClientKey).(*proxy.Client)
	return c, ok
}

func (p *proxyServer) logf(format string, args ...any) {
	if p.verbose {
		log.Printf("[proxy] "+format, args...)
	}
}

// ---- error helpers -------------------------------------------------------

func writeProxyError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"type":    "proxy_error",
			"code":    code,
			"message": message,
		},
	})
}

// writeProxyForbidden emits the structured 403 returned when a client calls a
// provider/model it is not granted. The pending access request id is included
// so operators/clients can reference it.
func (p *proxyServer) writeProxyForbidden(w http.ResponseWriter, provider, model, requested, requestID string) {
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error": map[string]any{
			"type":       "access_denied",
			"code":       "model_access_not_granted",
			"message":    fmt.Sprintf("access to %q is not granted; a pending access request has been recorded (id %s)", requested, requestID),
			"provider":   provider,
			"model":      model,
			"requested":  requested,
			"request_id": requestID,
			"status":     proxy.RequestPending,
		},
	})
}

// ---- auth middlewares ----------------------------------------------------

func proxyBearerToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if scheme, rest, ok := strings.Cut(auth, " "); ok && strings.EqualFold(scheme, "Bearer") {
		if t := strings.TrimSpace(rest); t != "" {
			return t
		}
	}
	if xKey := strings.TrimSpace(r.Header.Get("x-api-key")); xKey != "" {
		return xKey
	}
	return ""
}

// clientAuth authenticates the per-client bearer token against the store.
func (p *proxyServer) clientAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.requireAuth {
			// Loopback-only no-auth mode: synthesize an anonymous client is not
			// possible (grants are per-client), so no-auth still requires a token
			// to identify the client. Fall through to token lookup.
		}
		token := proxyBearerToken(r)
		if token == "" {
			writeProxyError(w, http.StatusUnauthorized, "missing_token", "missing client bearer token")
			return
		}
		client, _, err := p.store.AuthenticateToken(r.Context(), token)
		if err != nil {
			if errors.Is(err, proxy.ErrNotFound) {
				writeProxyError(w, http.StatusUnauthorized, "invalid_token", "invalid or expired client token")
				return
			}
			writeProxyError(w, http.StatusInternalServerError, "auth_error", "authentication failed")
			return
		}
		ctx := context.WithValue(r.Context(), proxyClientKey, client)
		next(w, r.WithContext(ctx))
	}
}

// adminAuth guards the admin plane with the single admin token. The token is
// ALWAYS required — including in loopback no-auth mode — because the admin plane
// can mint client tokens and grants; any co-located process (or a browser lured
// to the loopback address) would otherwise seize full control. An unset admin
// token fails closed (every admin request is rejected).
func (p *proxyServer) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := proxyBearerToken(r)
		if token == "" || p.adminToken == "" || !proxy.ConstantTimeEqual(token, p.adminToken) {
			writeProxyError(w, http.StatusUnauthorized, "invalid_admin_token", "invalid admin token")
			return
		}
		next(w, r)
	}
}

// ---- gate ----------------------------------------------------------------

// gate authorizes an authenticated client's LLM request against its grants,
// then forwards to the reused provider handler with the upstream route pinned.
// Denied or unknown-model requests create/dedupe a pending access request and
// return a structured 403 without ever reaching the provider handler.
func (p *proxyServer) gate(next http.HandlerFunc) http.HandlerFunc {
	return p.clientAuth(func(w http.ResponseWriter, r *http.Request) {
		client, ok := proxyClientFromContext(r.Context())
		if !ok {
			writeProxyError(w, http.StatusUnauthorized, "missing_client", "client not resolved")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxProxyRequestBody))
		_ = r.Body.Close()
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeProxyError(w, http.StatusRequestEntityTooLarge, "request_too_large",
					fmt.Sprintf("request body exceeds the %d byte limit", int64(maxProxyRequestBody)))
				return
			}
			writeProxyError(w, http.StatusBadRequest, "read_error", "failed to read request body")
			return
		}
		requested := peekRequestModel(body)
		if requested == "" {
			writeProxyError(w, http.StatusBadRequest, "model_required", "request must specify a model")
			return
		}

		// Resolve the requested model to a concrete provider/model route.
		provider, model := "", requested
		if alias, ok := p.catalog.Resolve(requested); ok {
			provider, model = alias.Provider, alias.Model
		}

		dec, err := p.store.Authorize(r.Context(), client.ID, provider, model)
		if err != nil {
			writeProxyError(w, http.StatusInternalServerError, "authorize_error", "authorization check failed")
			return
		}
		if !dec.Allowed {
			p.logf("deny client=%s requested=%q provider=%q model=%q request=%s", client.ID, requested, provider, model, dec.RequestID)
			p.writeProxyForbidden(w, provider, model, requested, dec.RequestID)
			return
		}

		// Wildcard grants route to the provider's default model.
		routeModel := model
		if routeModel == proxy.WildcardModel {
			routeModel = ""
		}
		// Rewrite the client-facing alias to the concrete upstream model so the
		// reused handlers send a model the provider understands.
		if newBody, rerr := rewriteRequestModel(body, routeModel); rerr == nil {
			body = newBody
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", strconv.Itoa(len(body)))

		p.logf("allow client=%s provider=%s model=%s", client.ID, provider, routeModel)
		ctx := withProxyForcedRoute(r.Context(), provider, routeModel)
		// Scope all session/response-chain state to this client so two clients
		// can never share or hijack each other's session or runtime.
		ctx = withServeSessionNamespace(ctx, proxySessionNamespace(client.ID))
		next(w, r.WithContext(ctx))
	})
}

// peekRequestModel extracts the top-level "model" from a JSON request body.
func peekRequestModel(body []byte) string {
	var peek struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		return ""
	}
	return strings.TrimSpace(peek.Model)
}

// rewriteRequestModel replaces (or removes, when model is empty) the top-level
// "model" field, preserving every other field byte-for-byte via RawMessage.
func rewriteRequestModel(body []byte, model string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	if model == "" {
		delete(obj, "model")
	} else {
		enc, err := json.Marshal(model)
		if err != nil {
			return nil, err
		}
		obj["model"] = enc
	}
	return json.Marshal(obj)
}

// ---- client self-service handlers ---------------------------------------

// proxyOpenAIModel is a single entry in the OpenAI-compatible /v1/models
// response returned to clients.
type proxyOpenAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// handleClientModels lists the models the calling client is granted, in the
// OpenAI /v1/models shape: {"object":"list","data":[{id,object,created,owned_by}]}.
// Only granted aliases are exposed to a client; the full routable catalog is
// admin-only (GET /admin/proxy/models). This prevents the client plane from
// leaking the operator's entire provider/model inventory.
func (p *proxyServer) handleClientModels(w http.ResponseWriter, r *http.Request) {
	client, ok := proxyClientFromContext(r.Context())
	if !ok {
		writeProxyError(w, http.StatusUnauthorized, "missing_client", "client not resolved")
		return
	}
	created := time.Now().Unix()
	data := make([]proxyOpenAIModel, 0)
	for _, a := range p.catalog.List() {
		granted, _ := p.store.HasGrant(r.Context(), client.ID, a.Provider, a.Model)
		if !granted {
			continue
		}
		data = append(data, proxyOpenAIModel{
			ID:      a.Alias,
			Object:  "model",
			Created: created,
			OwnedBy: a.Provider,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (p *proxyServer) handleWhoami(w http.ResponseWriter, r *http.Request) {
	client, ok := proxyClientFromContext(r.Context())
	if !ok {
		writeProxyError(w, http.StatusUnauthorized, "missing_client", "client not resolved")
		return
	}
	grants, _ := p.store.ListGrants(r.Context(), client.ID)
	writeJSON(w, http.StatusOK, map[string]any{"client": client, "grants": grants})
}

// handleClientRequestAccess lets a client explicitly request access to a
// provider/model (self-service). It dedupes against existing pending requests.
func (p *proxyServer) handleClientRequestAccess(w http.ResponseWriter, r *http.Request) {
	client, ok := proxyClientFromContext(r.Context())
	if !ok {
		writeProxyError(w, http.StatusUnauthorized, "missing_client", "client not resolved")
		return
	}
	var req struct {
		Model    string `json:"model"`
		Provider string `json:"provider"`
		Reason   string `json:"reason"`
	}
	if err := decodeProxyJSON(r, &req); err != nil {
		writeProxyError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	provider, model := strings.TrimSpace(req.Provider), strings.TrimSpace(req.Model)
	if model == "" && provider == "" {
		writeProxyError(w, http.StatusBadRequest, "model_required", "provider and/or model is required")
		return
	}
	// Resolve friendly aliases to concrete routes when possible.
	if model != "" {
		if alias, ok := p.catalog.Resolve(model); ok {
			if provider == "" {
				provider = alias.Provider
			}
			model = alias.Model
		}
	}
	ar, err := p.store.RequestAccess(r.Context(), client.ID, provider, model, strings.TrimSpace(req.Reason))
	if err != nil {
		if errors.Is(err, proxy.ErrTooManyPendingRequests) {
			writeProxyError(w, http.StatusTooManyRequests, "too_many_pending_requests",
				"pending access request limit reached; wait for existing requests to be reviewed")
			return
		}
		writeProxyError(w, http.StatusInternalServerError, "request_error", "failed to record access request")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"access_request": ar})
}

// ---- admin handlers ------------------------------------------------------

func (p *proxyServer) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": p.catalog.List()})
}

func (p *proxyServer) handleAdminListClients(w http.ResponseWriter, r *http.Request) {
	clients, err := p.store.ListClients(r.Context())
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "list_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": clients})
}

func (p *proxyServer) handleAdminCreateClient(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeProxyJSON(r, &req); err != nil {
		writeProxyError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	c, err := p.store.CreateClient(r.Context(), req.Name, req.Description)
	if err != nil {
		writeProxyError(w, http.StatusBadRequest, "create_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"client": c})
}

func (p *proxyServer) handleAdminGetClient(w http.ResponseWriter, r *http.Request) {
	c, err := p.store.GetClient(r.Context(), r.PathValue("id"))
	if err != nil {
		p.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"client": c})
}

func (p *proxyServer) handleAdminPatchClient(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Disabled *bool `json:"disabled"`
	}
	if err := decodeProxyJSON(r, &req); err != nil {
		writeProxyError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	id := r.PathValue("id")
	if req.Disabled != nil {
		if err := p.store.SetClientDisabled(r.Context(), id, *req.Disabled); err != nil {
			p.writeStoreErr(w, err)
			return
		}
	}
	c, err := p.store.GetClient(r.Context(), id)
	if err != nil {
		p.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"client": c})
}

func (p *proxyServer) handleAdminDeleteClient(w http.ResponseWriter, r *http.Request) {
	if err := p.store.DeleteClient(r.Context(), r.PathValue("id")); err != nil {
		p.writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *proxyServer) handleAdminListTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := p.store.ListTokens(r.Context(), r.PathValue("id"))
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "list_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": toks})
}

func (p *proxyServer) handleAdminCreateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Note       string `json:"note"`
		TTLSeconds int64  `json:"ttl_seconds"`
	}
	if err := decodeProxyJSON(r, &req); err != nil {
		writeProxyError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	plaintext, tok, err := p.store.CreateToken(r.Context(), r.PathValue("id"), req.Note, ttl)
	if err != nil {
		p.writeStoreErr(w, err)
		return
	}
	// The plaintext secret is returned exactly once.
	writeJSON(w, http.StatusCreated, map[string]any{"token": plaintext, "token_info": tok})
}

func (p *proxyServer) handleAdminRevokeToken(w http.ResponseWriter, r *http.Request) {
	if err := p.store.RevokeToken(r.Context(), r.PathValue("id")); err != nil {
		p.writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *proxyServer) handleAdminListGrants(w http.ResponseWriter, r *http.Request) {
	grants, err := p.store.ListGrants(r.Context(), r.PathValue("id"))
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "list_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": grants})
}

func (p *proxyServer) handleAdminCreateGrant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Note     string `json:"note"`
	}
	if err := decodeProxyJSON(r, &req); err != nil {
		writeProxyError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	g, err := p.store.AddGrant(r.Context(), r.PathValue("id"), req.Provider, req.Model, req.Note)
	if err != nil {
		p.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"grant": g})
}

func (p *proxyServer) handleAdminDeleteGrant(w http.ResponseWriter, r *http.Request) {
	if err := p.store.DeleteGrant(r.Context(), r.PathValue("id")); err != nil {
		p.writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *proxyServer) handleAdminListRequests(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	clientID := strings.TrimSpace(r.URL.Query().Get("client_id"))
	reqs, err := p.store.ListAccessRequests(r.Context(), status, clientID)
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "list_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_requests": reqs})
}

func (p *proxyServer) handleAdminDecideRequest(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Note string `json:"note"`
		}
		// Body is optional for approve/deny.
		_ = decodeProxyJSON(r, &req)
		ar, err := p.store.DecideAccessRequest(r.Context(), r.PathValue("id"), approve, req.Note)
		if err != nil {
			p.writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_request": ar})
	}
}

func (p *proxyServer) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	clientID := strings.TrimSpace(r.URL.Query().Get("client_id"))
	limit := 0
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	entries, err := p.store.ListAudit(r.Context(), clientID, limit)
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "list_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": entries})
}

func (p *proxyServer) writeStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, proxy.ErrNotFound) {
		writeProxyError(w, http.StatusNotFound, "not_found", "record not found")
		return
	}
	writeProxyError(w, http.StatusBadRequest, "store_error", err.Error())
}

func decodeProxyJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty body is allowed; caller validates required fields
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// handleProxyAdminAsset serves the public UI shell. Management data remains
// protected by the existing admin API middleware; the shell stores the supplied
// admin token in JavaScript memory only.
func handleProxyAdminAsset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		asset, err := proxyadminui.Asset(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if name == "index.html" {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		}
		_, _ = w.Write(asset)
	}
}

// handler builds the proxy HTTP mux. LLM endpoints are gated and forwarded to
// the reused provider handlers on llmServer; the admin and self-service planes
// are served directly alongside the embedded management UI.
func (p *proxyServer) handler(llmServer *serveServer) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", handleProxyAdminAsset("index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /proxy-admin.css", handleProxyAdminAsset("proxy-admin.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /proxy-admin.js", handleProxyAdminAsset("proxy-admin.js", "text/javascript; charset=utf-8"))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "platform": "proxy"})
	})

	// Client plane: reused provider protocol handlers behind the gate.
	mux.HandleFunc("POST /v1/responses", p.gate(llmServer.handleResponses))
	mux.HandleFunc("POST /v1/chat/completions", p.gate(llmServer.handleChatCompletions))
	mux.HandleFunc("POST /v1/messages", p.gate(llmServer.handleAnthropicMessages))

	// Client plane: self-service.
	mux.HandleFunc("GET /v1/models", p.clientAuth(p.handleClientModels))
	mux.HandleFunc("GET /v1/proxy/whoami", p.clientAuth(p.handleWhoami))
	mux.HandleFunc("POST /v1/proxy/access-requests", p.clientAuth(p.handleClientRequestAccess))

	// Admin plane.
	mux.HandleFunc("GET /admin/proxy/models", p.adminAuth(p.handleAdminModels))
	mux.HandleFunc("GET /admin/proxy/clients", p.adminAuth(p.handleAdminListClients))
	mux.HandleFunc("POST /admin/proxy/clients", p.adminAuth(p.handleAdminCreateClient))
	mux.HandleFunc("GET /admin/proxy/clients/{id}", p.adminAuth(p.handleAdminGetClient))
	mux.HandleFunc("PATCH /admin/proxy/clients/{id}", p.adminAuth(p.handleAdminPatchClient))
	mux.HandleFunc("DELETE /admin/proxy/clients/{id}", p.adminAuth(p.handleAdminDeleteClient))
	mux.HandleFunc("GET /admin/proxy/clients/{id}/tokens", p.adminAuth(p.handleAdminListTokens))
	mux.HandleFunc("POST /admin/proxy/clients/{id}/tokens", p.adminAuth(p.handleAdminCreateToken))
	mux.HandleFunc("DELETE /admin/proxy/tokens/{id}", p.adminAuth(p.handleAdminRevokeToken))
	mux.HandleFunc("GET /admin/proxy/clients/{id}/grants", p.adminAuth(p.handleAdminListGrants))
	mux.HandleFunc("POST /admin/proxy/clients/{id}/grants", p.adminAuth(p.handleAdminCreateGrant))
	mux.HandleFunc("DELETE /admin/proxy/grants/{id}", p.adminAuth(p.handleAdminDeleteGrant))
	mux.HandleFunc("GET /admin/proxy/requests", p.adminAuth(p.handleAdminListRequests))
	mux.HandleFunc("POST /admin/proxy/requests/{id}/approve", p.adminAuth(p.handleAdminDecideRequest(true)))
	mux.HandleFunc("POST /admin/proxy/requests/{id}/deny", p.adminAuth(p.handleAdminDecideRequest(false)))
	mux.HandleFunc("GET /admin/proxy/audit", p.adminAuth(p.handleAdminAudit))

	return mux
}

// ensureProxyExclusive returns an error if the proxy platform is requested
// alongside any other platform. It is a no-op when proxy is not requested.
func ensureProxyExclusive(platformNames []string) error {
	if !platformContains(platformNames, "proxy") {
		return nil
	}
	if len(platformNames) != 1 {
		return fmt.Errorf("the proxy platform is mutually exclusive and cannot be combined with other platforms (got: %s)", strings.Join(platformNames, ", "))
	}
	return nil
}

// runServeProxy runs the mutually-exclusive `proxy` platform. It builds a
// locked-down serve runtime (no server tools, no agent memory, no session
// persistence), opens the proxy store, and serves the admin + client HTTP API.
func runServeProxy(ctx context.Context, cmd *cobra.Command, cfg *config.Config, requireAuth bool) error {
	out := cmd.ErrOrStderr()

	responseTimeout, err := resolveServeResponseTimeout(cmd.Flags().Changed("response-timeout"), serveResponseTimeout, cfg.Serve.ResponseTimeout)
	if err != nil {
		return err
	}

	// Admin token is intentionally separate from client bearer tokens.
	adminToken := strings.TrimSpace(serveProxyAdminToken)
	adminSource := "flag"
	if adminToken == "" {
		if env := strings.TrimSpace(os.Getenv("TERM_LLM_PROXY_ADMIN_TOKEN")); env != "" {
			adminToken, adminSource = env, "env"
		}
	}
	// The admin token is always required (see adminAuth), so generate one even in
	// loopback no-auth mode rather than leaving the admin plane unprotected.
	if adminToken == "" {
		adminToken, err = generateServeToken()
		if err != nil {
			return fmt.Errorf("generate proxy admin token: %w", err)
		}
		adminSource = "generated"
	}

	store, err := proxy.Open(serveProxyDBPath)
	if err != nil {
		return fmt.Errorf("open proxy store: %w", err)
	}
	defer store.Close()

	catalog := proxy.BuildCatalog(cfg)

	// Locked-down runtime factory: no tools, no MCP, no search, no agent, no
	// session store — the proxy is a pure provider pass-through gated by grants.
	runtimeFactory := func(ctx context.Context, providerName, providerModel string) (*serveRuntime, error) {
		runner := &cmdRunner{baseCfg: cfg, defaults: cmdRunnerOptions{
			Provider:         serveProvider,
			Tools:            "", // no server tools
			MCP:              "", // no MCP tools
			NoSearch:         true,
			ProxyPassthrough: true, // bare engine: zero server tools/skills, no system prompt
			Debug:            serveDebug,
			DebugRaw:         debugRaw,
			ErrWriter:        io.Discard,
			// Store intentionally nil: no session persistence / agent memory.
		}}
		env, err := runner.prepare(ctx, runpkg.Request{
			Platform:     runpkg.PlatformWeb,
			Provider:     strings.TrimSpace(providerName),
			Model:        strings.TrimSpace(providerModel),
			DeferSession: true,
		}, nil)
		if err != nil {
			return nil, err
		}
		rt := env.runtime
		rt.platform = "web"
		rt.Touch()
		return rt, nil
	}
	factory := func(ctx context.Context) (*serveRuntime, error) { return runtimeFactory(ctx, "", "") }
	sessionMgr := newServeSessionManager(serveSessionTTL, serveSessionMax, factory)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sessionMgr.CloseContext(shutdownCtx)
	}()

	llmServer := &serveServer{
		cfg: serveServerConfig{
			host:                serveHost,
			port:                servePort,
			requireAuth:         false, // the proxy gate performs client auth, not serveServer.auth
			api:                 true,
			suppressServerTools: true, // never surface server-executed tool calls to proxy clients
			verbose:             serveVerbose,
			responseTimeout:     responseTimeout,
		},
		sessionMgr:     sessionMgr,
		cfgRef:         cfg,
		runtimeFactory: runtimeFactory,
	}
	llmServer.shutdownCh = make(chan struct{})

	p := &proxyServer{
		store:       store,
		catalog:     catalog,
		adminToken:  adminToken,
		requireAuth: requireAuth,
		verbose:     serveVerbose,
		errw:        out,
	}

	proxyHandler := p.handler(llmServer)
	// Match the other HTTP serve surfaces: all proxy UI and API routes live
	// beneath --base-path, with the prefix stripped before dispatch.
	outerMux := http.NewServeMux()
	outerMux.Handle(serveBasePath+"/", http.StripPrefix(serveBasePath, proxyHandler))
	outerMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, serveBasePath+"/", http.StatusTemporaryRedirect)
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", serveHost, servePort),
		Handler:           outerMux,
		ReadHeaderTimeout: serveReadHeaderTimeout,
		IdleTimeout:       serveIdleTimeout,
	}

	aliases := catalog.List()
	fmt.Fprintf(out, "term-llm serve proxy listening on http://%s:%d%s/\n", serveHost, servePort, serveBasePath)
	fmt.Fprintf(out, "auth: %s\n", authSummary(requireAuth))
	switch adminSource {
	case "generated":
		fmt.Fprintf(out, "admin token: %s (auto-generated; export TERM_LLM_PROXY_ADMIN_TOKEN to persist)\n", adminToken)
	case "env":
		fmt.Fprintf(out, "admin token: %s (from $TERM_LLM_PROXY_ADMIN_TOKEN)\n", adminToken)
	default:
		fmt.Fprintf(out, "admin token: %s\n", adminToken)
	}
	if !requireAuth {
		fmt.Fprintf(out, "note: client bearer auth disabled (loopback only); admin token still required\n")
	}
	fmt.Fprintf(out, "proxy db: %s\n", serveProxyDBPathDisplay())
	fmt.Fprintf(out, "exported provider/model aliases: %d\n", len(aliases))
	fmt.Fprintf(out, "note: PROTOTYPE — no per-token quotas; single admin token; per-client session isolation; server tools, skills & system prompt disabled\n")

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		close(llmServer.shutdownCh)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("proxy server: %w", err)
		}
		return nil
	}
}

func serveProxyDBPathDisplay() string {
	if strings.TrimSpace(serveProxyDBPath) != "" {
		return serveProxyDBPath
	}
	return "<data-dir>/proxy.db"
}

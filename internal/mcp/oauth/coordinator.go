package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const (
	flowLifetime       = 10 * time.Minute
	refreshExpirySkew  = 5 * time.Minute
	refreshTimeout     = 30 * time.Second
	metadataTimeout    = 15 * time.Second
	preflightTimeout   = 15 * time.Second
	maximumSafeErrSize = 300
)

type callbackResult struct {
	code  string
	state string
	iss   string
	err   error
}

type flowRecord struct {
	flow              Flow
	callbackState     string
	callbackDelivered bool
	callback          chan callbackResult
	ready             chan struct{}
	done              chan struct{}
	cancel            context.CancelFunc
	readyOnce         sync.Once
	doneOnce          sync.Once
}

// Coordinator owns process-global flow deduplication and persistent token
// sources. Multiple Coordinator values remain safe through the store file lock.
type Coordinator struct {
	store   Store
	client  *http.Client
	initErr error

	mu          sync.Mutex
	flows       map[string]*flowRecord
	byEndpoint  map[string]string
	byState     map[string]string
	refreshErrs map[string]error
}

func NewCoordinator(store Store) *Coordinator {
	return &Coordinator{
		store: store, client: http.DefaultClient, flows: make(map[string]*flowRecord),
		byEndpoint: make(map[string]string), byState: make(map[string]string),
		refreshErrs: make(map[string]error),
	}
}

var (
	defaultCoordinatorOnce sync.Once
	defaultCoordinator     *Coordinator
)

func DefaultCoordinator() *Coordinator {
	defaultCoordinatorOnce.Do(func() {
		path, err := DefaultStorePath()
		defaultCoordinator = NewCoordinator(NewFileStore(path))
		defaultCoordinator.initErr = err
	})
	return defaultCoordinator
}

// Handler constructs the SDK v1.7 authorization handler used by ordinary MCP
// connections. Its fetcher is deliberately non-interactive.
func (c *Coordinator) Handler(endpoint string, options Options) (sdkauth.OAuthHandler, error) {
	return c.newHandler(endpoint, options, "http://127.0.0.1/callback", false, nil)
}

func (c *Coordinator) newHandler(endpoint string, options Options, redirectURL string, force bool, interactive *flowRecord) (sdkauth.OAuthHandler, error) {
	if c == nil || c.store == nil {
		return nil, fmt.Errorf("MCP OAuth coordinator is unavailable")
	}
	if c.initErr != nil {
		return nil, fmt.Errorf("resolve MCP OAuth store: %w", c.initErr)
	}
	canonical, err := CanonicalEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	client := options.HTTPClient
	if client == nil {
		client = c.client
	}
	var stored *Session
	stored, err = c.store.Load(canonical)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("load MCP OAuth credentials: %w", err)
	}
	if errors.Is(err, ErrNotFound) {
		stored = nil
	}

	var initial oauth2.TokenSource
	if stored != nil && !force {
		initial = &persistentTokenSource{
			store: c.store, endpoint: canonical, session: cloneSession(stored),
			client: client, coordinator: c,
		}
	}

	var callbackIssuer string
	fetcher := func(context.Context, *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		return nil, ErrAuthenticationRequired
	}
	if interactive != nil {
		fetcher = func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
			u, err := url.Parse(args.URL)
			if err != nil {
				return nil, fmt.Errorf("invalid authorization URL")
			}
			state := u.Query().Get("state")
			if state == "" {
				return nil, fmt.Errorf("authorization server did not provide state")
			}
			c.mu.Lock()
			if interactive.flow.State == FlowStarting {
				interactive.flow.State = FlowPending
				interactive.flow.AuthorizationURL = args.URL
				interactive.callbackState = state
				c.byState[state] = interactive.flow.ID
			}
			c.mu.Unlock()
			interactive.readyOnce.Do(func() { close(interactive.ready) })

			select {
			case result := <-interactive.callback:
				if result.err != nil {
					return nil, result.err
				}
				callbackIssuer = result.iss
				return &sdkauth.AuthorizationResult{Code: result.code, State: result.state, Iss: result.iss}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	handlerConfig := &sdkauth.AuthorizationCodeHandlerConfig{
		RedirectURL:              redirectURL,
		AuthorizationCodeFetcher: fetcher,
		RequestRefreshToken:      true,
		Client:                   client,
		InitialTokenSource:       initial,
	}
	if options.ClientIDMetadataURL != "" {
		handlerConfig.ClientIDMetadataDocumentConfig = &sdkauth.ClientIDMetadataDocumentConfig{URL: options.ClientIDMetadataURL}
	}
	clientID, clientSecret, issuer := options.ClientID, options.ClientSecret, ""
	if clientID == "" && stored != nil && stored.Config.ClientID != "" {
		// Reuse the persisted (usually DCR-issued) client so refreshes and
		// re-authorizations keep one registration. An interactive flow must
		// present a redirect URI compatible with that registration: exact
		// match, or RFC 8252 loopback redirects that differ only by port.
		// Otherwise fall back to dynamic registration for the new redirect.
		if interactive == nil || redirectCompatible(stored.Config.RedirectURL, redirectURL) {
			clientID, clientSecret, issuer = stored.Config.ClientID, stored.Config.ClientSecret, stored.Issuer
		}
	}
	if clientID != "" {
		credentials := &oauthex.ClientCredentials{ClientID: clientID, Issuer: issuer}
		if clientSecret != "" {
			credentials.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: clientSecret}
		}
		handlerConfig.PreregisteredClient = credentials
	}
	if clientID == "" {
		handlerConfig.DynamicClientRegistrationConfig = &sdkauth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				RedirectURIs: []string{redirectURL}, TokenEndpointAuthMethod: "none",
				GrantTypes:    []string{"authorization_code", "refresh_token"},
				ResponseTypes: []string{"code"}, ClientName: "term-llm",
				Scope: strings.Join(options.Scopes, " "),
			},
		}
	}
	handlerConfig.NewTokenSource = func(ctx context.Context, cfg *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
		resolvedIssuer := callbackIssuer
		if resolvedIssuer == "" {
			resolvedIssuer = inferIssuer(cfg.Endpoint.AuthURL)
		}
		revocationEndpoint := ""
		if resolvedIssuer != "" {
			metadataCtx, cancel := context.WithTimeout(ctx, metadataTimeout)
			if metadata, metadataErr := sdkauth.GetAuthServerMetadata(metadataCtx, resolvedIssuer, client); metadataErr == nil && metadata != nil {
				resolvedIssuer = metadata.Issuer
				revocationEndpoint = metadata.RevocationEndpoint
			}
			cancel()
		}
		saved, err := c.store.Update(canonical, func(current *Session) (*Session, error) {
			version := uint64(0)
			if current != nil {
				version = current.Version
			}
			return &Session{
				Version: version, Endpoint: canonical, Issuer: resolvedIssuer,
				Config: configFromOAuth2(cfg), Token: cloneToken(token),
				RevocationEndpoint: revocationEndpoint,
			}, nil
		})
		if err != nil {
			return nil, fmt.Errorf("save MCP OAuth credentials: %w", err)
		}
		c.setRefreshError(canonical, nil)
		return &persistentTokenSource{
			store: c.store, endpoint: canonical, session: saved,
			client: client, coordinator: c,
		}, nil
	}
	handler, err := sdkauth.NewAuthorizationCodeHandler(handlerConfig)
	if err != nil {
		return nil, err
	}
	configuredScopes := append([]string(nil), options.Scopes...)
	if stored != nil {
		configuredScopes = unionScopeStrings(configuredScopes, stored.Config.Scopes)
	}
	if len(configuredScopes) > 0 {
		return &scopeOAuthHandler{OAuthHandler: handler, scopes: configuredScopes}, nil
	}
	return handler, nil
}

type scopeOAuthHandler struct {
	sdkauth.OAuthHandler
	scopes []string
}

func (h *scopeOAuthHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	challenges, err := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	if err != nil {
		return h.OAuthHandler.Authorize(ctx, req, resp)
	}
	foundBearer := false
	for index := range challenges {
		if challenges[index].Scheme != "bearer" {
			continue
		}
		foundBearer = true
		existing := strings.Fields(challenges[index].Params["scope"])
		challenges[index].Params["scope"] = strings.Join(unionScopeStrings(existing, h.scopes), " ")
		break
	}
	if !foundBearer {
		challenges = append(challenges, oauthex.Challenge{Scheme: "bearer", Params: map[string]string{"scope": strings.Join(h.scopes, " ")}})
	}
	cloned := *resp
	cloned.Header = resp.Header.Clone()
	cloned.Header.Del("WWW-Authenticate")
	for _, challenge := range challenges {
		cloned.Header.Add("WWW-Authenticate", formatChallenge(challenge))
	}
	return h.OAuthHandler.Authorize(ctx, req, &cloned)
}

func unionScopeStrings(left, right []string) []string {
	seen := make(map[string]bool, len(left)+len(right))
	result := make([]string, 0, len(left)+len(right))
	for _, scope := range append(append([]string(nil), left...), right...) {
		if scope = strings.TrimSpace(scope); scope != "" && !seen[scope] {
			seen[scope] = true
			result = append(result, scope)
		}
	}
	return result
}

func formatChallenge(challenge oauthex.Challenge) string {
	keys := make([]string, 0, len(challenge.Params))
	for key := range challenge.Params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(challenge.Params[key])
		parts = append(parts, key+`="`+value+`"`)
	}
	if len(parts) == 0 {
		return challenge.Scheme
	}
	return challenge.Scheme + " " + strings.Join(parts, ", ")
}

// Start begins or adopts an interactive authorization flow. It waits only for
// discovery to produce the authorization URL; completion continues in a
// background goroutine until callback, cancellation, or expiry.
func (c *Coordinator) Start(ctx context.Context, endpoint string, options Options, redirectURL string, force bool) (*Flow, error) {
	canonical, err := CanonicalEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if _, err := url.ParseRequestURI(redirectURL); err != nil {
		return nil, fmt.Errorf("invalid OAuth redirect URL: %w", err)
	}
	if !force {
		if status := c.Status(canonical); status.State == AuthSignedIn {
			return nil, fmt.Errorf("already signed in")
		}
	}

	c.mu.Lock()
	c.expireFlowsLocked(time.Now())
	if id := c.byEndpoint[canonical]; id != "" {
		if record := c.flows[id]; record != nil && (record.flow.State == FlowStarting || record.flow.State == FlowPending) {
			flow := record.flow
			c.mu.Unlock()
			return &flow, nil
		}
	}
	id, err := randomID(24)
	if err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("create OAuth flow ID: %w", err)
	}
	flowCtx, cancel := context.WithTimeout(context.Background(), flowLifetime)
	record := &flowRecord{
		flow:     Flow{ID: id, Endpoint: canonical, ExpiresAt: time.Now().Add(flowLifetime).UTC(), State: FlowStarting},
		callback: make(chan callbackResult, 1), ready: make(chan struct{}), done: make(chan struct{}), cancel: cancel,
	}
	c.flows[id] = record
	c.byEndpoint[canonical] = id
	c.mu.Unlock()

	handler, err := c.newHandler(canonical, options, redirectURL, force, record)
	if err != nil {
		c.finishFlow(record, FlowFailed, err)
		return nil, err
	}
	go c.runFlow(flowCtx, record, handler, options)

	select {
	case <-record.ready:
		flow, _ := c.Flow(id)
		if flow.State == FlowFailed {
			return flow, errors.New(flow.Error)
		}
		flow.Created = true
		return flow, nil
	case <-ctx.Done():
		c.Cancel(canonical, id)
		return nil, ctx.Err()
	case <-time.After(preflightTimeout + metadataTimeout):
		c.Cancel(canonical, id)
		return nil, fmt.Errorf("OAuth discovery timed out")
	}
}

func (c *Coordinator) runFlow(ctx context.Context, record *flowRecord, handler sdkauth.OAuthHandler, options Options) {
	response := c.authorizationChallenge(ctx, record.flow.Endpoint, options)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, record.flow.Endpoint, nil)
	if err == nil {
		err = handler.Authorize(ctx, req, response)
	}
	if err == nil {
		c.finishFlow(record, FlowSucceeded, nil)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		c.finishFlow(record, FlowExpired, err)
		return
	}
	if errors.Is(err, context.Canceled) {
		c.mu.Lock()
		state := record.flow.State
		c.mu.Unlock()
		if state == FlowStarting || state == FlowPending {
			c.finishFlow(record, FlowCanceled, err)
		}
		return
	}
	c.finishFlow(record, FlowFailed, err)
}

func (c *Coordinator) authorizationChallenge(ctx context.Context, endpoint string, options Options) *http.Response {
	client := options.HTTPClient
	if client == nil {
		client = c.client
	}
	preflightCtx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(preflightCtx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if resp, requestErr := client.Do(req); requestErr == nil {
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				if resp.Body == nil {
					resp.Body = io.NopCloser(strings.NewReader(""))
				}
				if resp.Header.Get("WWW-Authenticate") == "" && len(options.Scopes) > 0 {
					resp.Header.Set("WWW-Authenticate", `Bearer scope="`+strings.Join(options.Scopes, " ")+`"`)
				}
				return resp
			}
			_ = resp.Body.Close()
		}
	}
	header := make(http.Header)
	if len(options.Scopes) > 0 {
		header.Set("WWW-Authenticate", `Bearer scope="`+strings.Join(options.Scopes, " ")+`"`)
	}
	return &http.Response{StatusCode: http.StatusUnauthorized, Header: header, Body: io.NopCloser(strings.NewReader(""))}
}

// CompleteCallback atomically consumes a callback state capability.
func (c *Coordinator) CompleteCallback(state, code, issuer, oauthError string) (string, bool) {
	c.mu.Lock()
	id := c.byState[state]
	record := c.flows[id]
	if id == "" || record == nil || record.callbackState != state || record.flow.State != FlowPending || time.Now().After(record.flow.ExpiresAt) {
		c.mu.Unlock()
		return "", false
	}
	delete(c.byState, state)
	record.callbackState = ""
	record.callbackDelivered = true
	c.mu.Unlock()

	result := callbackResult{code: code, state: state, iss: issuer}
	if oauthError != "" {
		result.err = fmt.Errorf("authorization was denied")
	} else if code == "" {
		result.err = fmt.Errorf("authorization callback did not include a code")
	}
	record.callback <- result
	return id, true
}

func (c *Coordinator) Cancel(endpoint, flowID string) bool {
	canonical, _ := CanonicalEndpoint(endpoint)
	c.mu.Lock()
	record := c.flows[flowID]
	if record == nil || record.callbackDelivered || (canonical != "" && record.flow.Endpoint != canonical) || (record.flow.State != FlowStarting && record.flow.State != FlowPending) {
		c.mu.Unlock()
		return false
	}
	record.flow.State = FlowCanceled
	record.flow.Error = "authorization canceled"
	if record.callbackState != "" {
		delete(c.byState, record.callbackState)
		record.callbackState = ""
	}
	delete(c.byEndpoint, record.flow.Endpoint)
	cancel := record.cancel
	record.readyOnce.Do(func() { close(record.ready) })
	record.doneOnce.Do(func() { close(record.done) })
	c.mu.Unlock()
	cancel()
	return true
}

func (c *Coordinator) Flow(id string) (*Flow, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireFlowsLocked(time.Now())
	record := c.flows[id]
	if record == nil {
		return nil, false
	}
	flow := record.flow
	return &flow, true
}

func (c *Coordinator) Wait(ctx context.Context, id string) (*Flow, error) {
	c.mu.Lock()
	record := c.flows[id]
	c.mu.Unlock()
	if record == nil {
		return nil, fmt.Errorf("unknown OAuth flow")
	}
	select {
	case <-record.done:
		flow, _ := c.Flow(id)
		return flow, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Coordinator) finishFlow(record *flowRecord, state FlowState, err error) {
	c.mu.Lock()
	if record.flow.State == FlowCanceled && state != FlowCanceled {
		c.mu.Unlock()
		return
	}
	record.flow.State = state
	record.flow.AuthorizationURL = ""
	record.flow.Error = safeError(err)
	if state == FlowSucceeded {
		record.flow.Error = ""
	}
	if record.callbackState != "" {
		delete(c.byState, record.callbackState)
		record.callbackState = ""
	}
	delete(c.byEndpoint, record.flow.Endpoint)
	record.readyOnce.Do(func() { close(record.ready) })
	record.doneOnce.Do(func() { close(record.done) })
	c.mu.Unlock()
	record.cancel()
}

func (c *Coordinator) expireFlowsLocked(now time.Time) {
	for id, record := range c.flows {
		if now.After(record.flow.ExpiresAt.Add(flowLifetime)) && record.flow.State != FlowStarting && record.flow.State != FlowPending {
			delete(c.flows, id)
			continue
		}
		if now.After(record.flow.ExpiresAt) && (record.flow.State == FlowStarting || record.flow.State == FlowPending) {
			record.flow.State = FlowExpired
			record.flow.AuthorizationURL = ""
			record.flow.Error = "authorization expired"
			delete(c.byEndpoint, record.flow.Endpoint)
			if record.callbackState != "" {
				delete(c.byState, record.callbackState)
				record.callbackState = ""
			}
			record.readyOnce.Do(func() { close(record.ready) })
			record.doneOnce.Do(func() { close(record.done) })
			record.cancel()
		}
	}
}

func (c *Coordinator) Status(endpoint string) AuthStatus {
	status := AuthStatus{State: AuthSignedOut, CanSignIn: true}
	if c == nil || c.store == nil {
		return status
	}
	canonical, err := CanonicalEndpoint(endpoint)
	if err != nil {
		return status
	}
	c.mu.Lock()
	if id := c.byEndpoint[canonical]; id != "" {
		if record := c.flows[id]; record != nil && (record.flow.State == FlowStarting || record.flow.State == FlowPending) {
			status.State = AuthWaiting
			c.mu.Unlock()
			return status
		}
	}
	refreshErr := c.refreshErrs[canonical]
	c.mu.Unlock()
	session, err := c.store.Load(canonical)
	if err != nil {
		return status
	}
	status.Issuer = session.Issuer
	status.Scopes = append([]string(nil), session.Config.Scopes...)
	status.StoragePath = c.store.Path()
	status.CanSignOut = true
	status.ExpiresAt = session.Token.Expiry
	if errors.Is(refreshErr, ErrRefreshRejected) || (!session.Token.Expiry.IsZero() && time.Now().After(session.Token.Expiry) && session.Token.RefreshToken == "") {
		status.State = AuthRequired
		return status
	}
	if refreshErr != nil {
		status.State = AuthRetry
		return status
	}
	if tokenNeedsRefresh(session.Token) {
		if session.Token.RefreshToken != "" {
			status.State = AuthExpired
		} else {
			status.State = AuthRequired
		}
		return status
	}
	status.State = AuthSignedIn
	return status
}

func (c *Coordinator) Logout(ctx context.Context, endpoint string, localOnly bool) error {
	session, err := c.store.Load(endpoint)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load MCP OAuth credentials: %w", err)
	}
	var revokeErr error
	if !localOnly && session.RevocationEndpoint != "" {
		revokeErr = c.revoke(ctx, session)
	}
	if _, err := c.store.Delete(endpoint); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("delete MCP OAuth credentials: %w", err)
	}
	canonical, _ := CanonicalEndpoint(endpoint)
	c.setRefreshError(canonical, nil)
	return revokeErr
}

func (c *Coordinator) revoke(ctx context.Context, session *Session) error {
	token := session.Token.RefreshToken
	hint := "refresh_token"
	if token == "" {
		token, hint = session.Token.AccessToken, "access_token"
	}
	if token == "" {
		return nil
	}
	values := url.Values{"token": {token}, "token_type_hint": {hint}}
	useBasicAuth := session.Config.ClientSecret != "" && session.Config.Endpoint.AuthStyle == oauth2.AuthStyleInHeader
	if !useBasicAuth {
		values.Set("client_id", session.Config.ClientID)
		if session.Config.ClientSecret != "" {
			values.Set("client_secret", session.Config.ClientSecret)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, session.RevocationEndpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return fmt.Errorf("create OAuth revocation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if useBasicAuth {
		req.SetBasicAuth(session.Config.ClientID, session.Config.ClientSecret)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("revoke MCP OAuth grant: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("revoke MCP OAuth grant: authorization server returned %s", resp.Status)
	}
	return nil
}

func (c *Coordinator) setRefreshError(endpoint string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		delete(c.refreshErrs, endpoint)
	} else {
		c.refreshErrs[endpoint] = err
	}
}

type persistentTokenSource struct {
	store       Store
	endpoint    string
	session     *Session
	client      *http.Client
	coordinator *Coordinator
	mu          sync.Mutex
}

func (s *persistentTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var returned *oauth2.Token
	var refreshRejected bool
	updated, err := s.store.Update(s.endpoint, func(current *Session) (*Session, error) {
		if current == nil || current.Token == nil {
			return nil, ErrAuthenticationRequired
		}
		// A newer process may already have rotated the refresh token. Always
		// adopt the disk generation before deciding whether network I/O is needed.
		s.session = cloneSession(current)
		if !tokenNeedsRefresh(current.Token) {
			returned = cloneToken(current.Token)
			return nil, nil
		}
		if current.Token.RefreshToken == "" {
			if current.Token.Valid() {
				returned = cloneToken(current.Token)
				return nil, nil
			}
			return nil, ErrAuthenticationRequired
		}
		cfg := current.Config.oauth2Config()
		// Refresh runs while holding the store lock so rotation is serialized
		// across processes; bound it so a hung token endpoint cannot block
		// every other credential operation indefinitely.
		refreshCtx, cancelRefresh := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancelRefresh()
		refreshCtx = context.WithValue(refreshCtx, oauth2.HTTPClient, s.client)
		refreshSeed := cloneToken(current.Token)
		// oauth2.Config.TokenSource otherwise reuses a token until its own small
		// expiry delta. Mark only the in-memory seed expired so our five-minute
		// proactive refresh skew is honored without changing the stored grant.
		refreshSeed.Expiry = time.Now().Add(-time.Second)
		token, refreshErr := cfg.TokenSource(refreshCtx, refreshSeed).Token()
		if refreshErr != nil {
			classified := classifyRefreshError(refreshErr)
			s.coordinator.setRefreshError(s.endpoint, classified)
			if errors.Is(classified, ErrRefreshRejected) {
				current.Token.RefreshToken = ""
				refreshRejected = true
				return current, nil
			}
			return nil, classified
		}
		returned = cloneToken(token)
		next := cloneSession(current)
		next.Token = cloneToken(token)
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	if refreshRejected {
		return nil, ErrRefreshRejected
	}
	if updated != nil {
		s.session = updated
	}
	if returned == nil && updated != nil {
		returned = cloneToken(updated.Token)
	}
	if returned == nil {
		return nil, ErrAuthenticationRequired
	}
	s.coordinator.setRefreshError(s.endpoint, nil)
	return returned, nil
}

func tokenNeedsRefresh(token *oauth2.Token) bool {
	if token == nil || token.AccessToken == "" {
		return true
	}
	if token.Expiry.IsZero() {
		return false
	}
	return time.Now().Add(refreshExpirySkew).After(token.Expiry)
}

func classifyRefreshError(err error) error {
	if err == nil {
		return nil
	}
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) && retrieveErr.ErrorCode == "invalid_grant" {
		return ErrRefreshRejected
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "invalid_grant") || strings.Contains(lower, "revoked") {
		return ErrRefreshRejected
	}
	return fmt.Errorf("temporarily unable to refresh MCP OAuth grant")
}

// redirectCompatible reports whether an interactive authorization may reuse a
// client registration recorded for storedRedirect when redirecting to next.
func redirectCompatible(storedRedirect, next string) bool {
	if storedRedirect == "" || next == "" {
		return storedRedirect == next
	}
	if storedRedirect == next {
		return true
	}
	a, errA := url.Parse(storedRedirect)
	b, errB := url.Parse(next)
	if errA != nil || errB != nil {
		return false
	}
	// RFC 8252 §7.3: authorization servers must allow loopback redirect URIs
	// to vary by port at request time.
	return a.Scheme == b.Scheme && a.Path == b.Path &&
		isLoopbackHost(a.Hostname()) && isLoopbackHost(b.Hostname())
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func inferIssuer(authURL string) string {
	u, err := url.Parse(authURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	for _, suffix := range []string{"/oauth2/authorize", "/oauth/authorize", "/authorize"} {
		if strings.HasSuffix(u.Path, suffix) {
			u.Path = strings.TrimSuffix(u.Path, suffix)
			break
		}
	}
	u.RawQuery, u.Fragment = "", ""
	return strings.TrimSuffix(u.String(), "/")
}

func randomID(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func cloneToken(token *oauth2.Token) *oauth2.Token {
	if token == nil {
		return nil
	}
	copy := *token
	return &copy
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrAuthenticationRequired) {
		return "sign-in required"
	}
	if errors.Is(err, ErrRefreshRejected) {
		return "stored authorization expired or was revoked; sign in again"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "authorization expired"
	}
	if errors.Is(err, context.Canceled) {
		return "authorization canceled"
	}
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		return "authorization server rejected the token request"
	}
	text := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, err.Error())
	text = strings.TrimSpace(text)
	if len(text) > maximumSafeErrSize {
		text = text[:maximumSafeErrSize] + "…"
	}
	return text
}

package oauth

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const builtinClientIDMetadataURL = "https://term-llm.com/oauth/client.json"

// builtinPublicClientConfig persists token auth method none for our shared
// public identity rather than probing HTTP Basic on every refresh.
func builtinPublicClientConfig(cfg *oauth2.Config) *oauth2.Config {
	if cfg.ClientID != builtinClientIDMetadataURL || cfg.ClientSecret != "" || cfg.Endpoint.AuthStyle != oauth2.AuthStyleAutoDetect {
		return cfg
	}
	copy := *cfg
	copy.Endpoint.AuthStyle = oauth2.AuthStyleInParams
	return &copy
}

// prepareInteractiveHandler chooses the shared native identity only when no
// explicit or reusable registered client was selected. The SDK prefers CIMD to
// preregistered clients, so attaching it unconditionally would change precedence.
func prepareInteractiveHandler(ctx context.Context, resourceURL, expectedBuiltinIssuer string, challenges []oauthex.Challenge, config *sdkauth.AuthorizationCodeHandlerConfig) (sdkauth.OAuthHandler, *oauthex.AuthServerMeta, error) {
	client, replay := newDiscoveryClient(config.Client)
	metadataCtx, cancel := context.WithTimeout(ctx, metadataTimeout)
	metadata, err := discoverAuthorizationServer(metadataCtx, resourceURL, challenges, client)
	cancel()
	if err != nil {
		return nil, nil, fmt.Errorf("discover OAuth authorization server: %w", err)
	}
	// Revalidating the shared identity must not discard the stored grant's
	// issuer pin, even if capabilities now require selecting DCR instead.
	if expectedBuiltinIssuer != "" {
		if metadata == nil {
			return nil, nil, fmt.Errorf("cannot verify stored OAuth issuer without authorization server metadata; sign out before switching authorization servers")
		}
		// Match SDK v1.8 preregistered-client issuer comparison semantics.
		if strings.TrimSuffix(metadata.Issuer, "/") != strings.TrimSuffix(expectedBuiltinIssuer, "/") {
			return nil, nil, fmt.Errorf("authorization server issuer %q does not match stored OAuth issuer %q; sign out before switching authorization servers", metadata.Issuer, expectedBuiltinIssuer)
		}
	}
	replay.record = false
	selected := *config
	selected.Client = client
	if selected.ClientIDMetadataDocumentConfig == nil && selected.PreregisteredClient == nil && supportsBuiltinIdentity(selected.RedirectURL, metadata) {
		selected.ClientIDMetadataDocumentConfig = &sdkauth.ClientIDMetadataDocumentConfig{URL: builtinClientIDMetadataURL}
	}
	if !supportsRegistration(&selected, metadata) {
		return nil, nil, fmt.Errorf("authorization server supports no available OAuth client identity; configure a client ID or client metadata URL, or ask the server to support native public-client metadata (including RFC 9207 issuer responses) or dynamic client registration")
	}
	handler, err := sdkauth.NewAuthorizationCodeHandler(&selected)
	return handler, metadata, err
}

func supportsBuiltinIdentity(redirectURL string, metadata *oauthex.AuthServerMeta) bool {
	return nativeCallback(redirectURL) && metadata != nil && metadata.Issuer != "" &&
		metadata.ClientIDMetadataDocumentSupported && metadata.AuthorizationResponseIssParameterSupported &&
		slices.Contains(metadata.TokenEndpointAuthMethodsSupported, "none")
}

// nativeCallback deliberately does not use redirectCompatible: the hosted
// document describes only this IPv4 loopback path, with RFC 8252 port variation.
func nativeCallback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil {
		return false
	}
	port := u.Port()
	value, err := strconv.ParseUint(port, 10, 16)
	return err == nil && value != 0 && raw == "http://127.0.0.1:"+port+"/callback"
}

func supportsRegistration(config *sdkauth.AuthorizationCodeHandlerConfig, metadata *oauthex.AuthServerMeta) bool {
	// No metadata means the SDK uses legacy /authorize, /token and /register.
	if metadata == nil {
		return true
	}
	return config.PreregisteredClient != nil ||
		(config.ClientIDMetadataDocumentConfig != nil && metadata.ClientIDMetadataDocumentSupported) ||
		(config.DynamicClientRegistrationConfig != nil && metadata.RegistrationEndpoint != "")
}

// discoveryTransport records just this flow's discovery GETs, then replays them
// to the SDK. Selection and authorization must see the same PRM and AS metadata,
// even if the server changes responses between requests. POSTs (DCR and token
// exchange) always go to the original transport and are never retried here.
// Recording finishes before the transport is handed to the SDK.
type discoveryTransport struct {
	base      http.RoundTripper
	record    bool
	responses map[string]discoveryResponse
}

type discoveryResponse struct {
	response *http.Response
	body     []byte
	err      error
}

func newDiscoveryClient(original *http.Client) (*http.Client, *discoveryTransport) {
	client := *original
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	replay := &discoveryTransport{base: base, record: true, responses: make(map[string]discoveryResponse)}
	client.Transport = replay
	return &client, replay
}

func (t *discoveryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return t.base.RoundTrip(req)
	}
	key := req.URL.String()
	cached, ok := t.responses[key]
	if !ok {
		if !t.record {
			return nil, fmt.Errorf("OAuth discovery requested metadata outside the selected snapshot")
		}
		cached.response, cached.err = t.base.RoundTrip(req)
		if cached.err == nil {
			// Match the SDK's 1 MiB metadata decoding limit and close the live
			// body before retaining its bounded, replayable snapshot.
			cached.body, cached.err = io.ReadAll(io.LimitReader(cached.response.Body, 1<<20))
			_ = cached.response.Body.Close()
		}
		t.responses[key] = cached
	}
	if cached.err != nil {
		return nil, cached.err
	}
	response := new(http.Response)
	*response = *cached.response
	response.Header = cached.response.Header.Clone()
	response.Body = io.NopCloser(bytes.NewReader(cached.body))
	response.Request = req
	return response, nil
}

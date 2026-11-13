package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

type protectedResourceMetadataCandidate struct {
	url      string
	resource string
}

// discoverAuthorizationServer follows SDK v1.8's first successful PRM and first
// authorization server rules, including its legacy origin fallback. Keep the URL
// candidates and challenge selection in sync with the SDK's discovery helpers.
func discoverAuthorizationServer(ctx context.Context, resourceURL string, challenges []oauthex.Challenge, client *http.Client) (*oauthex.AuthServerMeta, error) {
	metadataURL := ""
	for _, challenge := range challenges {
		if challenge.Params["resource_metadata"] != "" {
			metadataURL = challenge.Params["resource_metadata"]
			break
		}
	}
	var metadata *oauthex.ProtectedResourceMetadata
	for _, candidate := range protectedResourceMetadataCandidates(metadataURL, resourceURL) {
		resolved, err := oauthex.GetProtectedResourceMetadata(ctx, candidate.url, candidate.resource, client)
		if err == nil && resolved != nil {
			metadata = resolved
			break
		}
	}

	authorizationServer := ""
	if metadata != nil {
		if len(metadata.AuthorizationServers) == 0 {
			return nil, fmt.Errorf("protected resource metadata has no authorization servers specified")
		}
		authorizationServer = metadata.AuthorizationServers[0]
	} else {
		// Compatibility with the SDK's 2025-03-26 authorization flow.
		u, err := url.Parse(resourceURL)
		if err != nil {
			return nil, err
		}
		u.Path = ""
		authorizationServer = u.String()
	}
	return sdkauth.GetAuthServerMetadata(ctx, authorizationServer, client)
}

func protectedResourceMetadataCandidates(metadataURL, resourceURL string) []protectedResourceMetadataCandidate {
	var candidates []protectedResourceMetadataCandidate
	if metadataURL != "" {
		candidates = append(candidates, protectedResourceMetadataCandidate{url: metadataURL, resource: resourceURL})
	}
	u, err := url.Parse(resourceURL)
	if err != nil {
		return candidates
	}
	metadata := *u
	metadata.Path = "/.well-known/oauth-protected-resource/" + strings.TrimLeft(u.Path, "/")
	candidates = append(candidates, protectedResourceMetadataCandidate{url: metadata.String(), resource: resourceURL})
	metadata.Path = "/.well-known/oauth-protected-resource"
	rootResource := *u
	rootResource.Path = ""
	candidates = append(candidates, protectedResourceMetadataCandidate{url: metadata.String(), resource: rootResource.String()})
	return candidates
}
